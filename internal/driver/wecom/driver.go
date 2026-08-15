package wecom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

var (
	ErrAuthRequired          = core.NewFailure(core.FailureAuthRequired, "WeCom authentication is required")
	ErrTargetAmbiguous       = core.NewFailure(core.FailureTargetAmbiguous, "WeCom conversation target is ambiguous")
	ErrUnsupportedCapability = core.NewFailure(core.FailureUnsupported, "WeCom driver capability is unsupported")
	ErrSendUncertain         = core.NewFailure(core.FailureSendUncertain, "WeCom send result is uncertain")
	ErrUserActionRequired    = core.NewFailure(core.FailureUserActionRequired, "direct user interaction is required")
	ErrNotFound              = core.NewFailure(core.FailureNotFound, "requested WeCom object was not found")
	ErrStale                 = core.NewFailure(core.FailureStale, "WeCom UI state is stale")
	ErrClientIncompatible    = core.NewFailure(core.FailureClientIncompatible, "the configured WeCom client runtime is incompatible")
)

type surfaceState struct {
	surface  core.Surface
	actions  map[string]CompanionAction
	sequence int64
}

type sendMemo struct {
	digest string
	result core.SendResult
}

// Driver implements the shared driver boundary using only the official WeCom
// Android APK and the repository-owned accessibility companion.
type Driver struct {
	runtime *Runtime

	operationMu sync.Mutex
	mu          sync.Mutex
	account     core.AccountRuntime
	surfaces    map[string]surfaceState
	sendMemos   map[string]sendMemo
}

var _ core.Driver = (*Driver)(nil)
var _ core.AccountPurger = (*Driver)(nil)

func New(config Config, executor Executor) (*Driver, error) {
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		return nil, err
	}
	return &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}, nil
}

func Factory(config Config, executor Executor) core.Factory {
	return func(_ core.AccountRuntime) (core.Driver, error) {
		return New(config, executor)
	}
}

func (d *Driver) Platform() core.Platform { return core.PlatformWeCom }

func (d *Driver) Start(ctx context.Context, account core.AccountRuntime) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := d.runtime.Start(ctx, account); err != nil {
		return err
	}
	d.mu.Lock()
	d.account = account
	d.surfaces = make(map[string]surfaceState)
	d.sendMemos = make(map[string]sendMemo)
	d.mu.Unlock()
	return nil
}

func (d *Driver) Stop(ctx context.Context) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := d.runtime.Stop(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	d.account = core.AccountRuntime{}
	d.surfaces = make(map[string]surfaceState)
	d.sendMemos = make(map[string]sendMemo)
	d.mu.Unlock()
	return nil
}

// Purge removes only an inactive container whose exact name and ownership
// labels match the requested account. Account files remain owned by the core
// account store, which deletes them after this hook succeeds.
func (d *Driver) Purge(ctx context.Context, account core.AccountRuntime) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := validateAccountID(account.AccountID); err != nil {
		return err
	}
	if active, running := d.runtime.Account(); running {
		if active.AccountID == account.AccountID {
			return errors.New("cannot purge an active WeCom account")
		}
		return errors.New("cannot purge while another WeCom account runtime is active")
	}
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		return err
	}
	if strings.Contains(dataDir, ",") {
		return errors.New("account data path cannot contain a comma")
	}
	network := networkName(account.AccountID)
	if _, err := inspectAccountNetwork(ctx, d.runtime.executor, d.runtime.config.DockerBinary, network, account.AccountID); err != nil {
		return fmt.Errorf("verify isolated account network before purge: %w", err)
	}
	name := containerName(account.AccountID)
	out, inspectErr := d.runtime.executor.Run(ctx, d.runtime.config.DockerBinary, "container", "inspect", name)
	if inspectErr != nil {
		listed, listErr := d.runtime.executor.Run(
			ctx,
			d.runtime.config.DockerBinary,
			"container", "ls", "--all", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}",
		)
		if listErr != nil {
			return fmt.Errorf("inspect account container before purge: %w", inspectErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			if err := os.RemoveAll(dataDir); err != nil {
				return fmt.Errorf("remove account data without a remaining container: %w", err)
			}
			return removeAccountNetwork(ctx, d.runtime.executor, d.runtime.config.DockerBinary, network, account.AccountID)
		}
		return fmt.Errorf("inspect existing account container before purge: %w", inspectErr)
	}
	if err := verifyPurgeContainer(out, name, account.AccountID, d.runtime.config.RedroidImage, dataDir); err != nil {
		return err
	}
	cleanupName := name + "-purge"
	cleanupArgs := []string{
		"container", "run", "--rm",
		"--pull", "never",
		"--name", cleanupName,
		"--network", "none",
		"--read-only",
		"--security-opt", "no-new-privileges:true",
		"--cap-drop", "ALL",
		"--cap-add", "DAC_OVERRIDE",
		"--cap-add", "FOWNER",
		"--label", labelDriver + "=wecom-purge",
		"--label", labelAccount + "=" + account.AccountID,
		"--mount", "type=bind,src=" + dataDir + ",dst=/account-data",
		"--entrypoint", "/system/bin/sh",
		d.runtime.config.RedroidImage,
		"-c", `rm -rf -- /account-data/* /account-data/.[!.]* /account-data/..?*`,
	}
	if _, err := d.runtime.executor.Run(ctx, d.runtime.config.DockerBinary, cleanupArgs...); err != nil {
		return fmt.Errorf("clear root-owned account data in restricted cleanup container: %w", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify cleared account data: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("restricted cleanup container left account data behind")
	}
	if err == nil {
		if err := os.Remove(dataDir); err != nil {
			return fmt.Errorf("remove cleared account data directory: %w", err)
		}
	}
	if _, err := d.runtime.executor.Run(ctx, d.runtime.config.DockerBinary, "container", "rm", name); err != nil {
		return fmt.Errorf("remove inactive WeCom container: %w", err)
	}
	return removeAccountNetwork(ctx, d.runtime.executor, d.runtime.config.DockerBinary, network, account.AccountID)
}

type dockerContainerInspection struct {
	Name   string `json:"Name"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func verifyPurgeContainer(raw []byte, expectedName, accountID, image, dataDir string) error {
	var inspections []dockerContainerInspection
	if err := json.Unmarshal(raw, &inspections); err != nil || len(inspections) != 1 {
		return errors.New("cannot decode a unique container inspection before purge")
	}
	inspection := inspections[0]
	if inspection.Name != "/"+expectedName ||
		inspection.Config.Labels[labelDriver] != "wecom" ||
		inspection.Config.Labels[labelAccount] != accountID ||
		inspection.Config.Image != image {
		return errors.New("refusing to purge a container without exact WeCom account ownership, name, and image")
	}
	if inspection.State.Running {
		return errors.New("refusing to purge a running WeCom container")
	}
	expectedSource := canonicalPath(dataDir)
	matched := 0
	for _, mount := range inspection.Mounts {
		if mount.Destination != "/data" {
			continue
		}
		matched++
		if mount.Type != "bind" || !mount.RW || canonicalPath(mount.Source) != expectedSource {
			return errors.New("refusing to purge a container whose /data bind mount does not exactly match the account")
		}
	}
	if matched != 1 {
		return errors.New("refusing to purge a container without exactly one verified /data bind mount")
	}
	return nil
}

func canonicalPath(path string) string {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved
	}
	return cleaned
}

func (d *Driver) Status(ctx context.Context) (core.Status, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	observed := time.Now().UTC()
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Status{State: core.StateStopped, ObservedAt: observed, Capabilities: capabilities()}, nil
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.Status{
			State:        core.StateOffline,
			Reason:       "accessibility companion is unavailable",
			ObservedAt:   observed,
			Capabilities: capabilities(),
		}, nil
	}
	state, reason := classifyLogin(snapshot)
	return core.Status{
		State:         state,
		Reason:        reason,
		ClientVersion: d.runtime.ClientVersion(),
		ObservedAt:    observed,
		Capabilities:  capabilities(),
	}, nil
}

func capabilities() map[string]core.Support {
	return core.CapabilityMap(map[string]core.Support{
		core.CapabilityAuthQR:          core.SupportExperimental,
		core.CapabilityAuthSMS:         core.SupportExperimental,
		core.CapabilityMessagesWatch:   core.SupportExperimental,
		core.CapabilityMessagesSend:    core.SupportExperimental,
		core.CapabilityWebOpen:         core.SupportExperimental,
		core.CapabilityMiniProgramOpen: core.SupportExperimental,
		core.CapabilitySurfaceAct:      core.SupportExperimental,
	})
}

func (d *Driver) AuthSnapshot(ctx context.Context) (core.AuthSnapshot, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	client, err := d.runtime.Companion()
	if err != nil {
		return core.AuthSnapshot{}, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.AuthSnapshot{}, err
	}
	android, err := d.runtime.Android()
	if err != nil {
		return core.AuthSnapshot{}, err
	}
	png, err := android.Screenshot(ctx)
	if err != nil {
		return core.AuthSnapshot{}, err
	}
	state, prompt := classifyLogin(snapshot)
	kind := core.AuthPhoneConfirm
	canSubmit := false
	text := snapshotText(snapshot)
	if containsAny(text, "验证码", "verification code", "短信") {
		kind = core.AuthSMS
		canSubmit = countEditable(snapshot) == 1
	} else if containsAny(text, "二维码", "扫码", "scan") {
		kind = core.AuthQR
	}
	result := core.AuthSnapshot{
		Kind:          kind,
		State:         state,
		Prompt:        prompt,
		ScreenshotPNG: png,
		CanSubmitCode: canSubmit,
		ObservedAt:    time.Now().UTC(),
	}
	if kind == core.AuthQR {
		// The full login frame is deliberate: cropping an unverified visual
		// region could present the wrong QR code to the user.
		result.QRCodePNG = png
	}
	return result, nil
}

func (d *Driver) SubmitAuthCode(ctx context.Context, code string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	code = strings.TrimSpace(code)
	if len(code) < 4 || len(code) > 8 {
		return errors.New("verification code must contain 4 to 8 digits")
	}
	for _, char := range code {
		if char < '0' || char > '9' {
			return errors.New("verification code must contain digits only")
		}
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return err
	}
	editable := editableNodes(snapshot)
	if len(editable) != 1 {
		return fmt.Errorf("%w: expected one verification input, found %d", ErrTargetAmbiguous, len(editable))
	}
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionSetText, NodeID: editable[0].ID, Text: code, ExpectedSequence: snapshot.Sequence}); err != nil {
		return err
	}
	updated, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return err
	}
	button, err := uniqueNode(updated, func(node Node) bool {
		return node.Clickable && matchesAny(nodeLabel(node), "确定", "登录", "下一步", "验证", "submit", "continue")
	})
	if err != nil {
		return fmt.Errorf("submit verification code: %w", err)
	}
	_, err = client.Act(ctx, CompanionAction{Kind: ActionClick, NodeID: button.ID, ExpectedSequence: updated.Sequence})
	return err
}

func (d *Driver) ListConversations(ctx context.Context, query core.ConversationQuery) ([]core.Conversation, error) {
	client, err := d.runtime.Companion()
	if err != nil {
		return nil, err
	}
	events, err := allEvents(ctx, client)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	byID := make(map[string]core.Conversation)
	for _, event := range events {
		title := event.Conversation
		if title == "" {
			title = event.Title
		}
		if event.PackageName != d.runtime.config.WeComPackage {
			continue
		}
		if !sha256Pattern.MatchString(event.ConversationKey) {
			return nil, fmt.Errorf("%w: companion event lacks a stable conversation identity", ErrClientIncompatible)
		}
		if title == "" {
			continue
		}
		if query.Search != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(query.Search)) {
			continue
		}
		id := conversationID(accountID, event.ConversationKey)
		conversation := byID[id]
		conversation.ID = id
		conversation.ExternalID = id
		conversation.Title = title
		conversation.Kind = "unknown"
		conversation.UnreadCount = 1
		conversation.Complete = false
		conversation.Source = "android_notification"
		if event.PostedAt.After(conversation.LastMessageAt) {
			conversation.LastMessageAt = event.PostedAt
		}
		byID[id] = conversation
	}
	result := make([]core.Conversation, 0, len(byID))
	for _, conversation := range byID {
		if query.Unread && conversation.UnreadCount == 0 {
			continue
		}
		result = append(result, conversation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LastMessageAt.After(result[j].LastMessageAt) })
	limit := query.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (d *Driver) ReadMessages(ctx context.Context, query core.MessageQuery) ([]core.Message, error) {
	client, err := d.runtime.Companion()
	if err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	page, err := client.Events(ctx, query.AfterSequence, limit)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	messages := make([]core.Message, 0, len(page.Events))
	for _, event := range page.Events {
		if event.PackageName != d.runtime.config.WeComPackage {
			continue
		}
		if !sha256Pattern.MatchString(event.ConversationKey) {
			return nil, fmt.Errorf("%w: companion event lacks a stable conversation identity", ErrClientIncompatible)
		}
		title := event.Conversation
		if title == "" {
			title = event.Title
		}
		conversation := conversationID(accountID, event.ConversationKey)
		if query.ConversationID != "" && query.ConversationID != conversation {
			continue
		}
		if !query.Before.IsZero() && !event.PostedAt.Before(query.Before) {
			continue
		}
		id := messageID(accountID, event.Sequence)
		surfaceRef := ""
		if event.Openable {
			surfaceRef = "wecom-notification:" + strconv.FormatInt(event.Sequence, 10)
		}
		messages = append(messages, core.Message{
			ID:             id,
			ExternalID:     id,
			ConversationID: conversation,
			SenderName:     event.Sender,
			SentAt:         event.PostedAt,
			Kind:           "text",
			Text:           event.Text,
			SurfaceRef:     surfaceRef,
			Source:         "android_notification",
			Complete:       false,
			Confidence:     0.7,
			Sequence:       event.Sequence,
		})
	}
	return messages, nil
}

func (d *Driver) Send(ctx context.Context, request core.SendRequest) (core.SendResult, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if request.ConversationID == "" || request.IdempotencyKey == "" {
		return core.SendResult{}, errors.New("conversation ID and idempotency key are required")
	}
	if request.Text == "" || len(request.Attachments) != 0 || request.ShareSurfaceID != "" {
		return core.SendResult{}, fmt.Errorf("%w: v0 WeCom driver sends text only", ErrUnsupportedCapability)
	}
	digest := wecomSendDigest(request)
	d.mu.Lock()
	memo, exists := d.sendMemos[request.IdempotencyKey]
	d.mu.Unlock()
	if exists {
		if memo.digest != digest {
			return core.SendResult{}, errors.New("idempotency key was already used for different content")
		}
		return memo.result, nil
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.SendResult{}, err
	}
	event, err := d.resolveConversationEvent(ctx, request.ConversationID)
	if err != nil {
		return core.SendResult{}, err
	}
	if err := d.openNotificationConversation(ctx, client, event); err != nil {
		return core.SendResult{}, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.SendResult{}, err
	}
	baseline := countNodeText(snapshot, request.Text)
	input, err := uniqueNode(snapshot, func(node Node) bool { return node.Editable && node.Enabled })
	if err != nil {
		return core.SendResult{}, fmt.Errorf("locate message input: %w", err)
	}
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionSetText, NodeID: input.ID, Text: request.Text, ExpectedSequence: snapshot.Sequence}); err != nil {
		return core.SendResult{}, err
	}
	updated, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return core.SendResult{}, err
	}
	button, err := uniqueNode(updated, func(node Node) bool {
		return node.Clickable && matchesAny(nodeLabel(node), "发送", "send")
	})
	if err != nil {
		return core.SendResult{}, fmt.Errorf("locate send button: %w", err)
	}
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionClick, NodeID: button.ID, ExpectedSequence: updated.Sequence}); err != nil {
		result := core.SendResult{Uncertain: true, Detail: "send action may have reached the official client, but its result was not observed"}
		d.rememberSend(request.IdempotencyKey, digest, result)
		return result, nil
	}
	confirmed, verifyErr := waitForNodeTextIncrease(ctx, client, request.Text, baseline, 8*time.Second)
	if verifyErr != nil || !confirmed {
		result := core.SendResult{Uncertain: true, Detail: ErrSendUncertain.Error()}
		d.rememberSend(request.IdempotencyKey, digest, result)
		return result, nil
	}
	result := core.SendResult{
		MessageID: "wecom-ui-" + digestID(request.IdempotencyKey),
		Verified:  true,
		Detail:    "outbound bubble observed in the official client",
	}
	d.rememberSend(request.IdempotencyKey, digest, result)
	return result, nil
}

func (d *Driver) rememberSend(key, digest string, result core.SendResult) {
	d.mu.Lock()
	d.sendMemos[key] = sendMemo{digest: digest, result: result}
	d.mu.Unlock()
}

func wecomSendDigest(request core.SendRequest) string {
	request.IdempotencyKey = ""
	encoded, _ := json.Marshal(request)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (d *Driver) OpenSurface(ctx context.Context, reference string) (core.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return core.Surface{}, errors.New("surface reference is required")
	}
	sequenceText, ok := strings.CutPrefix(reference, "wecom-notification:")
	if !ok {
		return core.Surface{}, fmt.Errorf("%w: v0 opens only message-backed WeCom notification references", ErrUnsupportedCapability)
	}
	sequence, err := strconv.ParseInt(sequenceText, 10, 64)
	if err != nil || sequence <= 0 {
		return core.Surface{}, fmt.Errorf("%w: notification surface reference is invalid", ErrNotFound)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	event, err := d.notificationEvent(ctx, client, sequence)
	if err != nil {
		return core.Surface{}, err
	}
	if !event.Openable {
		return core.Surface{}, fmt.Errorf("%w: notification no longer has an openable PendingIntent", ErrStale)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionOpenNotification, NodeID: sequenceText}); err != nil {
		return core.Surface{}, fmt.Errorf("%w: open notification surface: %w", ErrStale, err)
	}
	opened, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return core.Surface{}, err
	}
	if snapshotShowsAuthRequired(opened) {
		return core.Surface{}, ErrAuthRequired
	}
	if snapshotRequiresUserAction(opened) {
		return core.Surface{}, ErrUserActionRequired
	}
	if opened.PackageName != d.runtime.config.WeComPackage {
		return core.Surface{}, fmt.Errorf("%w: notification opened outside the verified WeCom package", ErrTargetAmbiguous)
	}
	return d.recordSurface(ctx, opened)
}

func (d *Driver) notificationEvent(ctx context.Context, client *CompanionClient, sequence int64) (CompanionEvent, error) {
	events, err := allEvents(ctx, client)
	if err != nil {
		return CompanionEvent{}, err
	}
	var matches []CompanionEvent
	for _, event := range events {
		if event.Sequence == sequence && event.PackageName == d.runtime.config.WeComPackage {
			matches = append(matches, event)
		}
	}
	if len(matches) == 0 {
		return CompanionEvent{}, fmt.Errorf("%w: notification sequence is absent from the bounded journal", ErrNotFound)
	}
	if len(matches) != 1 {
		return CompanionEvent{}, ErrTargetAmbiguous
	}
	return matches[0], nil
}

func (d *Driver) SnapshotSurface(ctx context.Context, surfaceID string) (core.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	_, exists := d.surfaces[surfaceID]
	d.mu.Unlock()
	if !exists {
		return core.Surface{}, fmt.Errorf("%w: unknown or closed surface", ErrNotFound)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	return d.updateSurface(ctx, surfaceID, snapshot)
}

func (d *Driver) ActSurface(ctx context.Context, surfaceID string, action core.SurfaceAction) (core.Surface, error) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	state, exists := d.surfaces[surfaceID]
	companionAction, allowed := state.actions[action.ActionID]
	d.mu.Unlock()
	if !exists {
		return core.Surface{}, fmt.Errorf("%w: unknown or closed surface", ErrNotFound)
	}
	if !allowed {
		return core.Surface{}, fmt.Errorf("%w: surface action was not advertised", ErrStale)
	}
	if companionAction.Kind == ActionSetText {
		companionAction.Text = action.Text
	} else if action.Text != "" {
		return core.Surface{}, fmt.Errorf("%w: this surface action does not accept text", ErrUnsupportedCapability)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return core.Surface{}, err
	}
	before, err := client.Snapshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	if before.Sequence != state.sequence {
		return core.Surface{}, fmt.Errorf("%w: surface changed since the action was advertised", ErrStale)
	}
	if surfaceHighRisk(before) && (companionAction.Kind == ActionClick || companionAction.Kind == ActionSetText) {
		return core.Surface{}, fmt.Errorf("%w: mutating actions are disabled on a high-risk surface", ErrUserActionRequired)
	}
	companionAction.ExpectedSequence = before.Sequence
	if _, err := client.Act(ctx, companionAction); err != nil {
		return core.Surface{}, err
	}
	after, err := waitForSnapshotChange(ctx, client, before.Sequence)
	if err != nil {
		return core.Surface{}, err
	}
	return d.updateSurface(ctx, surfaceID, after)
}

func (d *Driver) CloseSurface(ctx context.Context, surfaceID string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	d.mu.Lock()
	_, exists := d.surfaces[surfaceID]
	d.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: unknown or closed surface", ErrNotFound)
	}
	client, err := d.runtime.Companion()
	if err != nil {
		return err
	}
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionGlobalBack}); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.surfaces, surfaceID)
	d.mu.Unlock()
	return nil
}

func (d *Driver) resolveConversationEvent(ctx context.Context, id string) (CompanionEvent, error) {
	client, err := d.runtime.Companion()
	if err != nil {
		return CompanionEvent{}, err
	}
	events, err := allEvents(ctx, client)
	if err != nil {
		return CompanionEvent{}, err
	}
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	var matches []CompanionEvent
	for _, event := range events {
		if event.PackageName == d.runtime.config.WeComPackage && sha256Pattern.MatchString(event.ConversationKey) && conversationID(accountID, event.ConversationKey) == id {
			matches = append(matches, event)
		}
	}
	if len(matches) == 0 {
		return CompanionEvent{}, fmt.Errorf("%w: conversation ID is not present in the bounded notification journal", ErrNotFound)
	}
	title := matches[0].Conversation
	if title == "" {
		title = matches[0].Title
	}
	var latest CompanionEvent
	for _, event := range matches {
		eventTitle := event.Conversation
		if eventTitle == "" {
			eventTitle = event.Title
		}
		if eventTitle == "" || eventTitle != title {
			return CompanionEvent{}, fmt.Errorf("%w: one notification identity maps to multiple conversation titles", ErrTargetAmbiguous)
		}
		if event.Openable && event.Sequence > latest.Sequence {
			latest = event
		}
	}
	if latest.Sequence <= 0 {
		return CompanionEvent{}, fmt.Errorf("%w: conversation has no verified notification PendingIntent", ErrTargetAmbiguous)
	}
	return latest, nil
}

func (d *Driver) openNotificationConversation(ctx context.Context, client *CompanionClient, event CompanionEvent) error {
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return err
	}
	sequence := strconv.FormatInt(event.Sequence, 10)
	if _, err := client.Act(ctx, CompanionAction{Kind: ActionOpenNotification, NodeID: sequence}); err != nil {
		return fmt.Errorf("%w: open verified conversation notification: %w", ErrStale, err)
	}
	opened, err := waitForSnapshotChange(ctx, client, snapshot.Sequence)
	if err != nil {
		return err
	}
	if snapshotShowsAuthRequired(opened) {
		return ErrAuthRequired
	}
	if snapshotRequiresUserAction(opened) {
		return ErrUserActionRequired
	}
	title := event.Conversation
	if title == "" {
		title = event.Title
	}
	if opened.PackageName != d.runtime.config.WeComPackage || !snapshotConfirmsTitle(opened, title) {
		return fmt.Errorf("%w: opened notification did not confirm the exact conversation title", ErrTargetAmbiguous)
	}
	return nil
}

func (d *Driver) recordSurface(ctx context.Context, snapshot UISnapshot) (core.Surface, error) {
	d.mu.Lock()
	accountID := d.account.AccountID
	d.mu.Unlock()
	id := "surface-" + digestID(accountID+":"+strconv.FormatInt(time.Now().UnixNano(), 10))
	return d.updateSurface(ctx, id, snapshot)
}

func (d *Driver) updateSurface(ctx context.Context, id string, snapshot UISnapshot) (core.Surface, error) {
	android, err := d.runtime.Android()
	if err != nil {
		return core.Surface{}, err
	}
	png, err := android.Screenshot(ctx)
	if err != nil {
		return core.Surface{}, err
	}
	actions := make([]core.Action, 0)
	allowed := make(map[string]CompanionAction)
	highRisk := surfaceHighRisk(snapshot)
	for _, node := range snapshot.Nodes {
		label := nodeLabel(node)
		if label == "" {
			label = node.ClassName
		}
		var kinds []string
		if node.Editable && node.Enabled {
			kinds = append(kinds, ActionSetText)
		}
		if node.Clickable && node.Enabled {
			kinds = append(kinds, ActionClick)
		}
		if node.Scrollable && node.Enabled {
			kinds = append(kinds, ActionScrollForward, ActionScrollBackward)
		}
		for _, kind := range kinds {
			actionID := "act-" + digestID(id+":"+node.ID+":"+kind)
			actionLabel := label
			if kind == ActionScrollForward {
				actionLabel = "Scroll forward: " + label
			} else if kind == ActionScrollBackward {
				actionLabel = "Scroll backward: " + label
			}
			action := core.Action{ID: actionID, Label: actionLabel, Kind: kind}
			mutating := kind == ActionClick || kind == ActionSetText
			if mutating && (highRisk || sensitiveSurfaceLabel(label)) {
				action.Risk = "high"
				action.Disabled = true
			} else {
				allowed[actionID] = CompanionAction{Kind: kind, NodeID: node.ID}
			}
			actions = append(actions, action)
		}
	}
	surface := core.Surface{
		ID:         id,
		Kind:       classifySurfaceKind(snapshot),
		Title:      snapshot.WindowTitle,
		Screenshot: png,
		Actions:    actions,
		ObservedAt: time.Now().UTC(),
	}
	d.mu.Lock()
	d.surfaces[id] = surfaceState{surface: surface, actions: allowed, sequence: snapshot.Sequence}
	d.mu.Unlock()
	return surface, nil
}

func classifyLogin(snapshot UISnapshot) (core.RuntimeState, string) {
	text := snapshotText(snapshot)
	bottomNav := 0
	for _, label := range []string{"消息", "邮件", "文档", "工作台", "通讯录"} {
		if containsAny(text, label) {
			bottomNav++
		}
	}
	if snapshot.PackageName == DefaultWeComPackage && bottomNav >= 2 {
		return core.StateOnline, ""
	}
	if containsAny(text, "扫码登录", "微信登录", "手机号登录", "验证码", "登录企业微信", "scan to log in") {
		return core.StateAuthRequired, "complete login in the official WeCom client"
	}
	if snapshot.PackageName == "" {
		return core.StateStarting, "waiting for an accessibility window"
	}
	return core.StateDegraded, "official client state is not recognized by this compatibility profile"
}

func classifySurfaceKind(snapshot UISnapshot) string {
	classText := strings.ToLower(snapshot.WindowTitle + " " + snapshot.PackageName + " " + snapshotText(snapshot))
	if strings.Contains(classText, "小程序") || strings.Contains(classText, "miniprogram") {
		return "miniprogram"
	}
	if strings.Contains(classText, "http://") || strings.Contains(classText, "https://") || strings.Contains(classText, "webview") {
		return "web"
	}
	return "android_surface"
}

func snapshotText(snapshot UISnapshot) string {
	var builder strings.Builder
	builder.WriteString(snapshot.WindowTitle)
	for _, node := range snapshot.Nodes {
		builder.WriteByte('\n')
		builder.WriteString(node.Text)
		builder.WriteByte(' ')
		builder.WriteString(node.ContentDescription)
	}
	return strings.ToLower(builder.String())
}

func countEditable(snapshot UISnapshot) int { return len(editableNodes(snapshot)) }

func editableNodes(snapshot UISnapshot) []Node {
	return matchingNodes(snapshot, func(node Node) bool { return node.Editable && node.Enabled })
}

func uniqueNode(snapshot UISnapshot, match func(Node) bool) (Node, error) {
	nodes := matchingNodes(snapshot, match)
	if len(nodes) == 0 {
		return Node{}, ErrNotFound
	}
	if len(nodes) > 1 {
		return Node{}, ErrTargetAmbiguous
	}
	return nodes[0], nil
}

func matchingNodes(snapshot UISnapshot, match func(Node) bool) []Node {
	result := make([]Node, 0)
	for _, node := range snapshot.Nodes {
		if match(node) {
			result = append(result, node)
		}
	}
	return result
}

func nodeLabel(node Node) string {
	if node.Text != "" {
		return strings.TrimSpace(node.Text)
	}
	return strings.TrimSpace(node.ContentDescription)
}

func containsAny(value string, candidates ...string) bool {
	value = strings.ToLower(value)
	for _, candidate := range candidates {
		if strings.Contains(value, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

func matchesAny(value string, candidates ...string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range candidates {
		if value == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func waitForSnapshotChange(ctx context.Context, client *CompanionClient, previous int64) (UISnapshot, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil && snapshot.Sequence > previous {
			return snapshot, nil
		}
		if time.Now().After(deadline) {
			return UISnapshot{}, fmt.Errorf("%w: timed out waiting for official client UI change", ErrStale)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return UISnapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForNodeTextIncrease(ctx context.Context, client *CompanionClient, text string, baseline int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		snapshot, err := client.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		if countNodeText(snapshot, text) > baseline {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func countNodeText(snapshot UISnapshot, text string) int {
	count := 0
	for _, node := range snapshot.Nodes {
		if !node.Editable && node.Text == text {
			count++
		}
	}
	return count
}

func allEvents(ctx context.Context, client *CompanionClient) ([]CompanionEvent, error) {
	const maxEvents = 2_000
	result := make([]CompanionEvent, 0, 500)
	var cursor int64
	for len(result) < maxEvents {
		page, err := client.Events(ctx, cursor, 500)
		if err != nil {
			return nil, err
		}
		if len(page.Events) == 0 {
			break
		}
		result = append(result, page.Events...)
		if page.NextCursor <= cursor {
			break
		}
		cursor = page.NextCursor
	}
	return result, nil
}

func snapshotConfirmsTitle(snapshot UISnapshot, title string) bool {
	if title == "" {
		return false
	}
	if snapshot.WindowTitle == title {
		return true
	}
	for _, node := range snapshot.Nodes {
		if nodeLabel(node) == title {
			return true
		}
	}
	return false
}

func sensitiveSurfaceLabel(label string) bool {
	return containsAny(
		label,
		"支付", "付款", "收款", "转账", "红包", "授权", "允许", "身份验证", "实名认证",
		"账号安全", "账户安全", "购买", "下单", "充值", "提现", "签署", "登录", "密码", "银行卡",
		"pay", "payment", "transfer", "red packet", "authorize", "permission", "identity verification",
		"account security", "purchase", "checkout", "place order", "top up", "recharge", "withdraw",
		"sign agreement", "sign in", "log in", "login", "password", "bank card",
	)
}

func surfaceHighRisk(snapshot UISnapshot) bool { return sensitiveSurfaceLabel(snapshotText(snapshot)) }

func snapshotShowsAuthRequired(snapshot UISnapshot) bool {
	state, _ := classifyLogin(snapshot)
	return state == core.StateAuthRequired
}

func snapshotRequiresUserAction(snapshot UISnapshot) bool {
	return containsAny(
		snapshotText(snapshot),
		"账号存在风险", "账户存在风险", "设备验证", "安全验证", "异常登录", "确认本人操作",
		"account risk", "device verification", "security verification", "unusual login", "confirm on your phone",
	)
}

func conversationID(accountID, conversationKey string) string {
	return "wecom-conv-" + digestID(accountID+"\x00"+strings.ToLower(conversationKey))
}

func messageID(accountID string, sequence int64) string {
	return "wecom-msg-" + digestID(accountID+":"+strconv.FormatInt(sequence, 10))
}

func digestID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}
