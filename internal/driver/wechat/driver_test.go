package wechat

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

type fakeBackend struct {
	probe                ProbeResult
	screenshot           []byte
	sends                []UISendRequest
	actions              []string
	startedWith          []Profile
	surface              BackendSurface
	visibleConversations []VisibleConversation
	visibleMessages      []VisibleMessage
	selectedTitle        string
	selectedLocator      string
	appendTextOnSend     bool
}

func (f *fakeBackend) Start(_ context.Context, profile Profile) error {
	f.startedWith = append(f.startedWith, profile)
	return nil
}
func (f *fakeBackend) Stop(context.Context) error                   { return nil }
func (f *fakeBackend) Probe(context.Context) (ProbeResult, error)   { return f.probe, nil }
func (f *fakeBackend) Screenshot(context.Context) ([]byte, error)   { return f.screenshot, nil }
func (f *fakeBackend) SubmitAuthCode(context.Context, string) error { return nil }
func (f *fakeBackend) ListVisibleConversations(context.Context) ([]VisibleConversation, error) {
	return append([]VisibleConversation(nil), f.visibleConversations...), nil
}
func (f *fakeBackend) ReadVisibleMessages(_ context.Context, title, locator string) (VisibleMessages, error) {
	if title == "" {
		title, locator = f.selectedTitle, f.selectedLocator
	}
	if title == "" || locator == "" {
		return VisibleMessages{}, ErrConversationMissing
	}
	found := false
	for _, item := range f.visibleConversations {
		if item.Title == title && item.Locator == locator {
			found = true
			break
		}
	}
	if !found {
		return VisibleMessages{}, ErrConversationMissing
	}
	return VisibleMessages{
		ConversationTitle: title, ConversationLocator: locator,
		Messages: append([]VisibleMessage(nil), f.visibleMessages...),
	}, nil
}
func (f *fakeBackend) Send(_ context.Context, request UISendRequest) error {
	f.sends = append(f.sends, request)
	if f.appendTextOnSend && request.Text != "" {
		f.visibleMessages = append(f.visibleMessages, VisibleMessage{
			Text: request.Text, Kind: "text", Outgoing: true, Confidence: uiConfidence,
		})
	}
	return nil
}
func (f *fakeBackend) OpenSurface(context.Context, SurfaceTarget) (BackendSurface, error) {
	return f.surface, nil
}
func (f *fakeBackend) SnapshotSurface(context.Context) (BackendSurface, error) {
	return f.surface, nil
}
func (f *fakeBackend) ActSurface(_ context.Context, locator, _ string) (BackendSurface, error) {
	f.actions = append(f.actions, locator)
	return f.surface, nil
}
func (f *fakeBackend) CloseSurface(context.Context) error { return nil }

type fakeIndex struct {
	available bool
	waitErr   error
	waits     []OutgoingMatch
}

func TestCapabilitiesUseSharedCompleteContract(t *testing.T) {
	backend := &fakeBackend{probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()}}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	status, err := driver.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := shared.ValidateCapabilities(status.Capabilities); err != nil {
		t.Fatal(err)
	}
	if status.Capabilities[shared.CapabilityMessagesSend] != shared.SupportBeta {
		t.Fatalf("text-send capability = %q", status.Capabilities[shared.CapabilityMessagesSend])
	}
}

func (f *fakeIndex) Available(context.Context) bool { return f.available }
func (f *fakeIndex) ListConversations(context.Context, shared.ConversationQuery) ([]shared.Conversation, error) {
	return []shared.Conversation{{ID: "chat-1", Title: "Fixture chat", Complete: true, Source: "fixture"}}, nil
}
func (f *fakeIndex) ReadMessages(context.Context, shared.MessageQuery) ([]shared.Message, error) {
	return []shared.Message{{ID: "message-1", ConversationID: "chat-1", Source: "fixture", Complete: true}}, nil
}
func (f *fakeIndex) Conversation(_ context.Context, id string) (shared.Conversation, error) {
	if id != "chat-1" {
		return shared.Conversation{}, ErrConversationMissing
	}
	return shared.Conversation{ID: id, Title: "Fixture chat", Source: "fixture", Complete: true}, nil
}
func (f *fakeIndex) ResolveSurface(_ context.Context, reference string) (SurfaceTarget, error) {
	if reference != "surface-ref" {
		return SurfaceTarget{}, ErrSurfaceMissing
	}
	return SurfaceTarget{
		Reference: reference, ConversationID: "chat-1",
		ConversationTitle: "Fixture chat", AccessibleLabel: "Example article",
	}, nil
}
func (f *fakeIndex) WaitOutgoing(_ context.Context, match OutgoingMatch) (shared.Message, error) {
	f.waits = append(f.waits, match)
	if f.waitErr != nil {
		return shared.Message{}, f.waitErr
	}
	return shared.Message{ID: "sent-1", ConversationID: match.ConversationID}, nil
}

func TestDriverSendIsVerifiedAndIdempotent(t *testing.T) {
	backend := &fakeBackend{probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()}}
	index := &fakeIndex{available: true}
	driver := startFixtureDriver(t, backend, index)
	request := shared.SendRequest{
		ConversationID: "chat-1", Text: "hello", IdempotencyKey: "idem-1",
	}
	first, err := driver.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Verified || first.MessageID != "sent-1" || first != second {
		t.Fatalf("unexpected send result: first=%+v second=%+v", first, second)
	}
	if len(backend.sends) != 1 || len(index.waits) != 1 {
		t.Fatalf("idempotent replay sent again: sends=%d waits=%d", len(backend.sends), len(index.waits))
	}
	request.Text = "different"
	if _, err := driver.Send(context.Background(), request); err == nil {
		t.Fatal("expected reuse of idempotency key with different content to fail")
	}
	if len(backend.sends) != 1 {
		t.Fatal("different idempotent payload reached the UI backend")
	}
}

func TestDriverReturnsAndMemoizesUncertainSend(t *testing.T) {
	backend := &fakeBackend{probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()}}
	index := &fakeIndex{available: true, waitErr: errors.New("fixture timeout")}
	driver := startFixtureDriver(t, backend, index)
	request := shared.SendRequest{ConversationID: "chat-1", Text: "hello", IdempotencyKey: "idem-uncertain"}
	result, err := driver.Send(context.Background(), request)
	if !errors.Is(err, ErrSendUncertain) || !result.Uncertain || result.Verified {
		t.Fatalf("expected uncertain send, result=%+v err=%v", result, err)
	}
	result, err = driver.Send(context.Background(), request)
	if !errors.Is(err, ErrSendUncertain) || !result.Uncertain || len(backend.sends) != 1 {
		t.Fatalf("uncertain retry was not memoized: result=%+v err=%v sends=%d", result, err, len(backend.sends))
	}
}

func TestDriverRequiresOneSafeShareAction(t *testing.T) {
	tests := []struct {
		name    string
		actions []BackendAction
		wantErr error
	}{
		{
			name: "ambiguous",
			actions: []BackendAction{
				{Action: shared.Action{ID: "share-1", Kind: "share"}, Locator: "share-locator-1"},
				{Action: shared.Action{ID: "share-2", Kind: "share"}, Locator: "share-locator-2"},
			},
			wantErr: ErrTargetAmbiguous,
		},
		{
			name: "blocked",
			actions: []BackendAction{
				{Action: shared.Action{ID: "share-pay", Kind: "share", Risk: "high", Disabled: true}, Locator: "share-pay-locator"},
			},
			wantErr: ErrUserActionRequired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{
				probe:   ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
				surface: BackendSurface{Kind: "web", Title: "Article", Actions: test.actions},
			}
			driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
			surface, err := driver.OpenSurface(context.Background(), "surface-ref")
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.Send(context.Background(), shared.SendRequest{
				ConversationID: "chat-1", ShareSurfaceID: surface.ID, IdempotencyKey: "share-send",
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("share error = %v, want %v", err, test.wantErr)
			}
			if len(backend.sends) != 0 {
				t.Fatalf("ambiguous or blocked share reached backend: %#v", backend.sends)
			}
		})
	}
}

func TestDriverSurfaceRejectsHighRiskAndStaleActions(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{Kind: "web", Title: "Article", Actions: []BackendAction{
			{Action: shared.Action{ID: "read", Label: "Read", Kind: "activate", Risk: "low"}, Locator: "locator-read"},
			{Action: shared.Action{ID: "pay", Label: "Pay", Kind: "activate", Risk: "high", Disabled: true}, Locator: "locator-pay"},
		}},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "pay"}); !errors.Is(err, ErrUserActionRequired) {
		t.Fatalf("high-risk action error = %v", err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "unknown"}); !errors.Is(err, ErrActionStale) {
		t.Fatalf("stale action error = %v", err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "read"}); err != nil {
		t.Fatal(err)
	}
	if len(backend.actions) != 1 || backend.actions[0] != "locator-read" {
		t.Fatalf("unexpected backend actions: %#v", backend.actions)
	}
}

func TestAuthSnapshotPreservesCompleteScreenshotWhenQRBoundsAreInexact(t *testing.T) {
	imageValue := image.NewRGBA(image.Rect(0, 0, 20, 20))
	for y := 5; y < 15; y++ {
		for x := 4; x < 14; x++ {
			imageValue.Set(x, y, color.Black)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageValue); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{
		probe: ProbeResult{
			State: shared.StateAuthRequired, AuthKind: shared.AuthQR,
			ObservedAt: time.Now(), QRBounds: &Rectangle{X: 4, Y: 5, Width: 10, Height: 10},
		},
		screenshot: encoded.Bytes(),
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{})
	snapshot, err := driver.AuthSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	complete, err := png.Decode(bytes.NewReader(snapshot.QRCodePNG))
	if err != nil {
		t.Fatal(err)
	}
	if complete.Bounds() != imageValue.Bounds() {
		t.Fatalf("QR image bounds = %v, want complete screenshot %v", complete.Bounds(), imageValue.Bounds())
	}
	if !bytes.Equal(snapshot.QRCodePNG, encoded.Bytes()) {
		t.Fatal("QR image was altered using unreliable accessibility bounds")
	}
}

func startFixtureDriver(t *testing.T, backend *fakeBackend, index MessageIndex) *Driver {
	t.Helper()
	temporary := t.TempDir()
	driver, err := New(Config{Backend: backend, Index: index, VerificationTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	err = driver.Start(context.Background(), shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main",
		StateDir: filepath.Join(temporary, "state"), RuntimeDir: filepath.Join(temporary, "runtime"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	return driver
}
