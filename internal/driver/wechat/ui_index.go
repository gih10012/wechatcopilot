package wechat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

const uiConfidence = 0.55

type uiConversationEntry struct {
	conversation shared.Conversation
	locator      string
	ambiguous    bool
}

type outgoingBaseline struct {
	count int
	title string
}

type normalizedVisible struct {
	message shared.Message
	visible VisibleMessage
}

// UIIndex is a deliberately bounded fallback for client versions without a
// compatible read-only WCDB adapter. It exposes only currently visible data.
type UIIndex struct {
	backend   Backend
	accountID string
	now       func() time.Time
	pollEvery time.Duration

	mu            sync.Mutex
	conversations map[string]uiConversationEntry
	surfaces      map[string]SurfaceTarget
	sequences     map[string]int64
	nextSequence  int64
	baselines     map[string]outgoingBaseline
}

var _ MessageIndex = (*UIIndex)(nil)
var _ OutgoingBaselinePreparer = (*UIIndex)(nil)

func NewUIIndex(backend Backend, accountID string, now func() time.Time) *UIIndex {
	if now == nil {
		now = time.Now
	}
	return &UIIndex{
		backend: backend, accountID: accountID, now: now, pollEvery: 250 * time.Millisecond,
		conversations: make(map[string]uiConversationEntry),
		surfaces:      make(map[string]SurfaceTarget), sequences: make(map[string]int64),
		baselines: make(map[string]outgoingBaseline),
	}
}

func (i *UIIndex) Available(ctx context.Context) bool {
	probe, err := i.backend.Probe(ctx)
	return err == nil && (probe.State == shared.StateOnline || probe.State == shared.StateDegraded)
}

func (i *UIIndex) ListConversations(ctx context.Context, query shared.ConversationQuery) ([]shared.Conversation, error) {
	if query.Unread {
		return nil, errors.New("UI fallback cannot determine unread state without guessing")
	}
	visible, err := i.backend.ListVisibleConversations(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, item := range visible {
		if validVisibleTitle(item.Title) {
			counts[item.Title]++
		}
	}
	entries := make(map[string]uiConversationEntry)
	orderedIDs := make([]string, 0, len(visible))
	for _, item := range visible {
		if !validVisibleTitle(item.Title) || item.Locator == "" {
			continue
		}
		id := stableUIID("wxui_c_", i.accountID, item.Title)
		if _, exists := entries[id]; exists {
			continue
		}
		kind := strings.TrimSpace(item.Kind)
		if kind == "" {
			kind = "visible"
		}
		entry := uiConversationEntry{
			conversation: shared.Conversation{
				ID: id, ExternalID: stableUIID("ui:", i.accountID, item.Title),
				Title: item.Title, Kind: kind, UnreadCount: max(item.Unread, 0),
				Complete: false, Source: "ui",
			},
			locator: item.Locator, ambiguous: item.Ambiguous || counts[item.Title] > 1,
		}
		entries[id] = entry
		orderedIDs = append(orderedIDs, id)
	}
	i.mu.Lock()
	i.conversations = entries
	i.mu.Unlock()

	search := strings.ToLower(strings.TrimSpace(query.Search))
	limit := query.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	result := make([]shared.Conversation, 0, min(limit, len(orderedIDs)))
	for _, id := range orderedIDs {
		entry := entries[id]
		if search != "" && !strings.Contains(strings.ToLower(entry.conversation.Title), search) {
			continue
		}
		result = append(result, entry.conversation)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (i *UIIndex) ReadMessages(ctx context.Context, query shared.MessageQuery) ([]shared.Message, error) {
	if !query.Before.IsZero() {
		return nil, errors.New("UI fallback has observation times, not reliable message timestamps")
	}
	var title, locator string
	if query.ConversationID != "" {
		entry, err := i.refreshConversation(ctx, query.ConversationID)
		if err != nil {
			return nil, err
		}
		title, locator = entry.conversation.Title, entry.locator
	}
	visible, err := i.backend.ReadVisibleMessages(ctx, title, locator)
	if err != nil {
		return nil, err
	}
	if title != "" && visible.ConversationTitle != title {
		return nil, ErrTargetAmbiguous
	}
	entry, err := i.rememberVisibleConversation(visible.ConversationTitle, visible.ConversationLocator)
	if err != nil {
		return nil, err
	}
	normalized := i.normalizeVisible(entry, visible.Messages)
	limit := query.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	result := make([]shared.Message, 0, min(limit, len(normalized)))
	for _, item := range normalized {
		message := item.message
		if message.Sequence <= query.AfterSequence {
			continue
		}
		result = append(result, message)
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

func (i *UIIndex) Conversation(ctx context.Context, id string) (shared.Conversation, error) {
	entry, err := i.refreshConversation(ctx, id)
	if err != nil {
		return shared.Conversation{}, err
	}
	if entry.ambiguous {
		return shared.Conversation{}, ErrTargetAmbiguous
	}
	conversation := entry.conversation
	conversation.ExternalID = entry.locator
	return conversation, nil
}

func (i *UIIndex) ResolveSurface(_ context.Context, reference string) (SurfaceTarget, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	target, ok := i.surfaces[reference]
	if !ok {
		return SurfaceTarget{}, ErrSurfaceMissing
	}
	entry, ok := i.conversations[target.ConversationID]
	if !ok || entry.ambiguous {
		return SurfaceTarget{}, ErrTargetAmbiguous
	}
	target.ConversationTitle = entry.conversation.Title
	target.ConversationLocator = entry.locator
	return target, nil
}

func (i *UIIndex) PrepareOutgoing(ctx context.Context, match OutgoingMatch) error {
	if match.Text == "" || match.Text != strings.TrimSpace(match.Text) || match.AttachmentCount != 0 {
		return nil
	}
	i.mu.Lock()
	entry, ok := i.conversations[match.ConversationID]
	i.mu.Unlock()
	if !ok {
		return ErrConversationMissing
	}
	if entry.ambiguous {
		return ErrTargetAmbiguous
	}
	visible, err := i.backend.ReadVisibleMessages(ctx, entry.conversation.Title, entry.locator)
	if err != nil {
		return err
	}
	if visible.ConversationTitle != entry.conversation.Title {
		return ErrTargetAmbiguous
	}
	count := countExactOutgoing(visible.Messages, match.Text)
	i.mu.Lock()
	i.baselines[outgoingKey(match)] = outgoingBaseline{
		count: count, title: entry.conversation.Title,
	}
	i.mu.Unlock()
	return nil
}

func (i *UIIndex) WaitOutgoing(ctx context.Context, match OutgoingMatch) (shared.Message, error) {
	if match.Text == "" || match.Text != strings.TrimSpace(match.Text) || match.AttachmentCount != 0 {
		return shared.Message{}, errors.New("UI fallback cannot verify attachment-only or shared-surface sends")
	}
	key := outgoingKey(match)
	i.mu.Lock()
	baseline, ok := i.baselines[key]
	i.mu.Unlock()
	if !ok {
		return shared.Message{}, errors.New("UI outgoing baseline was not captured")
	}
	defer func() {
		i.mu.Lock()
		delete(i.baselines, key)
		i.mu.Unlock()
	}()
	deadline := time.NewTimer(match.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(i.pollEvery)
	defer ticker.Stop()
	for {
		entry, refreshErr := i.refreshConversation(ctx, match.ConversationID)
		if refreshErr != nil || entry.ambiguous || entry.conversation.Title != baseline.title {
			select {
			case <-ctx.Done():
				return shared.Message{}, ctx.Err()
			case <-deadline.C:
				return shared.Message{}, errors.New("exact outgoing conversation was no longer visible")
			case <-ticker.C:
				continue
			}
		}
		visible, err := i.backend.ReadVisibleMessages(ctx, entry.conversation.Title, entry.locator)
		if err == nil && visible.ConversationTitle == baseline.title {
			entry, rememberErr := i.rememberVisibleConversation(visible.ConversationTitle, visible.ConversationLocator)
			if rememberErr == nil {
				normalized := i.normalizeVisible(entry, visible.Messages)
				seen := 0
				for _, item := range normalized {
					if item.visible.Outgoing && item.visible.Text == match.Text {
						seen++
						if seen > baseline.count {
							return item.message, nil
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return shared.Message{}, ctx.Err()
		case <-deadline.C:
			return shared.Message{}, errors.New("new outgoing UI message was not observed")
		case <-ticker.C:
		}
	}
}

func (i *UIIndex) refreshConversation(ctx context.Context, id string) (uiConversationEntry, error) {
	if _, err := i.ListConversations(ctx, shared.ConversationQuery{Limit: 500}); err != nil {
		return uiConversationEntry{}, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	entry, ok := i.conversations[id]
	if !ok {
		return uiConversationEntry{}, ErrConversationMissing
	}
	return entry, nil
}

func (i *UIIndex) rememberVisibleConversation(title, locator string) (uiConversationEntry, error) {
	if !validVisibleTitle(title) || locator == "" {
		return uiConversationEntry{}, ErrConversationMissing
	}
	id := stableUIID("wxui_c_", i.accountID, title)
	i.mu.Lock()
	defer i.mu.Unlock()
	entry, exists := i.conversations[id]
	if exists && entry.conversation.Title != title {
		return uiConversationEntry{}, ErrTargetAmbiguous
	}
	if !exists {
		entry = uiConversationEntry{conversation: shared.Conversation{
			ID: id, ExternalID: stableUIID("ui:", i.accountID, title),
			Title: title, Kind: "visible", Complete: false, Source: "ui",
		}}
	}
	entry.locator = locator
	i.conversations[id] = entry
	return entry, nil
}

func (i *UIIndex) normalizeVisible(entry uiConversationEntry, visible []VisibleMessage) []normalizedVisible {
	observedAt := i.now().UTC()
	occurrences := make(map[string]int)
	result := make([]normalizedVisible, 0, len(visible))
	for _, item := range visible {
		item.AccessibleLabel = strings.TrimSpace(item.AccessibleLabel)
		if strings.TrimSpace(item.Text) == "" && item.AccessibleLabel == "" {
			continue
		}
		kind := strings.TrimSpace(item.Kind)
		if kind == "" {
			kind = "text"
		}
		item.Kind = kind
		signature := visibleSignature(item)
		occurrences[signature]++
		externalID := stableUIID("ui:", i.accountID, entry.conversation.ID, signature, strconv.Itoa(occurrences[signature]))
		messageID := stableUIID("wxui_m_", externalID)
		confidence := item.Confidence
		if confidence <= 0 || confidence >= 1 {
			confidence = uiConfidence
		}
		message := shared.Message{
			ID: messageID, ExternalID: externalID, ConversationID: entry.conversation.ID,
			SenderName: item.SenderName, SentAt: observedAt, Kind: kind, Text: item.Text,
			Source: "ui", Complete: false, Confidence: confidence,
		}
		if item.SurfaceKind != "" && item.AccessibleLabel != "" {
			message.SurfaceRef = stableUIID("wxui_s_", messageID, item.AccessibleLabel)
			i.mu.Lock()
			i.surfaces[message.SurfaceRef] = SurfaceTarget{
				Reference: message.SurfaceRef, ConversationID: entry.conversation.ID,
				ConversationTitle: entry.conversation.Title, ConversationLocator: entry.locator,
				AccessibleLabel: item.AccessibleLabel, Kind: item.SurfaceKind,
			}
			i.mu.Unlock()
		}
		i.mu.Lock()
		sequence, ok := i.sequences[messageID]
		if !ok {
			i.nextSequence++
			sequence = i.nextSequence
			i.sequences[messageID] = sequence
		}
		i.mu.Unlock()
		message.Sequence = sequence
		result = append(result, normalizedVisible{message: message, visible: item})
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].message.Sequence < result[right].message.Sequence
	})
	return result
}

func validVisibleTitle(value string) bool {
	return value == strings.TrimSpace(value) && value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 256
}

func countExactOutgoing(messages []VisibleMessage, text string) int {
	count := 0
	for _, message := range messages {
		if message.Outgoing && message.Text == text {
			count++
		}
	}
	return count
}

func visibleSignature(message VisibleMessage) string {
	direction := "in"
	if message.Outgoing {
		direction = "out"
	}
	return stableUIID("sig:", direction, message.Kind, message.SenderName, message.Text, message.AccessibleLabel)
}

func outgoingKey(match OutgoingMatch) string {
	return stableUIID("out:", match.ConversationID, match.Text, strconv.Itoa(match.AttachmentCount))
}

func stableUIID(prefix string, values ...string) string {
	hasher := sha256.New()
	for _, value := range values {
		_, _ = hasher.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hasher.Write([]byte{':'})
		_, _ = hasher.Write([]byte(value))
	}
	return prefix + hex.EncodeToString(hasher.Sum(nil)[:16])
}

func (i *UIIndex) String() string {
	return fmt.Sprintf("wechat UI fallback for account %s", i.accountID)
}
