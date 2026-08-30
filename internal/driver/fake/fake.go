package fake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/gih10012/wechatcopilot/internal/driver"
)

var fixtureSurfaceScreenshot = func() []byte {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		panic(err)
	}
	return encoded.Bytes()
}()

// Driver is a deterministic in-memory backend used by contract tests and
// replay development. It never contacts a real client.
type Driver struct {
	mu            sync.Mutex
	platform      driver.Platform
	state         driver.RuntimeState
	runtime       driver.AccountRuntime
	conversations []driver.Conversation
	messages      []driver.Message
	surfaces      map[string]driver.Surface
}

func New(platform driver.Platform) *Driver {
	now := time.Now().UTC()
	conversation := driver.Conversation{ID: "conv-file-transfer", ExternalID: "file-transfer", Title: "File Transfer", Kind: "direct", Complete: true, Source: "fake", LastMessageAt: now}
	return &Driver{
		platform:      platform,
		state:         driver.StateStopped,
		conversations: []driver.Conversation{conversation},
		messages: []driver.Message{{
			ID: "msg-welcome", ExternalID: "welcome", ConversationID: conversation.ID,
			SenderID: "system", SenderName: "Fixture", SentAt: now, Kind: "text",
			Text: "wechatcopilot fake driver is ready", Source: "fake", Complete: true, Confidence: 1, Sequence: 1,
		}},
		surfaces: make(map[string]driver.Surface),
	}
}

func (d *Driver) Platform() driver.Platform { return d.platform }

func (d *Driver) Start(_ context.Context, runtime driver.AccountRuntime) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime = runtime
	d.state = driver.StateOnline
	return nil
}

func (d *Driver) Stop(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = driver.StateStopped
	return nil
}

func (d *Driver) Status(context.Context) (driver.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return driver.Status{
		State:         d.state,
		Identity:      &driver.Identity{PlatformID: "fake-" + d.runtime.AccountID, DisplayName: d.runtime.Alias},
		ClientVersion: "fake-1", ObservedAt: time.Now().UTC(),
		Capabilities: driver.CapabilityMap(map[string]driver.Support{
			driver.CapabilityMessagesHistory: driver.SupportStable,
			driver.CapabilityMessagesWatch:   driver.SupportStable,
			driver.CapabilityMessagesSend:    driver.SupportStable,
			driver.CapabilityAttachmentsSend: driver.SupportStable,
			driver.CapabilityWebOpen:         driver.SupportStable,
			driver.CapabilityMiniProgramOpen: driver.SupportStable,
			driver.CapabilitySurfaceAct:      driver.SupportStable,
		}),
	}, nil
}

func (d *Driver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	return driver.AuthSnapshot{State: d.state, ObservedAt: time.Now().UTC()}, nil
}

func (d *Driver) SubmitAuthCode(context.Context, string) error { return nil }

func (d *Driver) ListConversations(_ context.Context, query driver.ConversationQuery) ([]driver.Conversation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var result []driver.Conversation
	for _, item := range d.conversations {
		if query.Search == "" || strings.Contains(strings.ToLower(item.Title), strings.ToLower(query.Search)) {
			result = append(result, item)
		}
	}
	return result, nil
}

func (d *Driver) ReadMessages(_ context.Context, query driver.MessageQuery) ([]driver.Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var result []driver.Message
	for _, item := range d.messages {
		if item.Sequence <= query.AfterSequence {
			continue
		}
		if query.ConversationID != "" && item.ConversationID != query.ConversationID {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func (d *Driver) Send(_ context.Context, request driver.SendRequest) (driver.SendResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state != driver.StateOnline {
		return driver.SendResult{}, errors.New("fake driver is offline")
	}
	found := false
	for _, item := range d.conversations {
		if item.ID == request.ConversationID {
			found = true
			break
		}
	}
	if !found {
		return driver.SendResult{}, errors.New("conversation not found")
	}
	id := "msg-send-" + request.IdempotencyKey
	d.messages = append(d.messages, driver.Message{
		ID: id, ExternalID: id, ConversationID: request.ConversationID, SenderID: d.runtime.AccountID,
		SenderName: d.runtime.Alias, SentAt: time.Now().UTC(), Kind: "text", Text: request.Text,
		Attachments: request.Attachments, Source: "fake", Complete: true, Confidence: 1,
		Sequence: int64(len(d.messages) + 1),
	})
	return driver.SendResult{MessageID: id, Verified: true}, nil
}

func (d *Driver) OpenSurface(_ context.Context, ref string) (driver.Surface, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := "surface-" + ref
	screenshot := append([]byte(nil), fixtureSurfaceScreenshot...)
	digest := sha256.Sum256(screenshot)
	surface := driver.Surface{
		ID: id, Kind: "web", Title: "Fixture surface", URL: "https://example.invalid/",
		Screenshot: screenshot, ScreenshotSHA256: hex.EncodeToString(digest[:]),
		OCRText: "Fixture surface", ObservedAt: time.Now().UTC(),
		Actions: []driver.Action{{ID: "close", Label: "Close", Kind: "close"}},
	}
	d.surfaces[id] = surface
	return surface, nil
}

func (d *Driver) SnapshotSurface(_ context.Context, id string) (driver.Surface, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	surface, ok := d.surfaces[id]
	if !ok {
		return driver.Surface{}, errors.New("surface not found")
	}
	return surface, nil
}

func (d *Driver) ActSurface(ctx context.Context, id string, action driver.SurfaceAction) (driver.Surface, error) {
	if action.ActionID == "close" {
		if err := d.CloseSurface(ctx, id); err != nil {
			return driver.Surface{}, err
		}
		return driver.Surface{ID: id, Kind: "closed", ObservedAt: time.Now().UTC()}, nil
	}
	return d.SnapshotSurface(ctx, id)
}

func (d *Driver) CloseSurface(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.surfaces[id]; !ok {
		return errors.New("surface not found")
	}
	delete(d.surfaces, id)
	return nil
}

func (d *Driver) Purge(context.Context, driver.AccountRuntime) error { return nil }

var _ driver.Driver = (*Driver)(nil)
var _ driver.AccountPurger = (*Driver)(nil)
