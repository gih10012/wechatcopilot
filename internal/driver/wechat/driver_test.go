package wechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

type fakeBackend struct {
	probe                ProbeResult
	screenshot           []byte
	screenshotCalls      int
	sends                []UISendRequest
	actions              []string
	actionTexts          []string
	startedWith          []Profile
	surface              BackendSurface
	visibleConversations []VisibleConversation
	visibleMessages      []VisibleMessage
	selectedTitle        string
	selectedLocator      string
	appendTextOnSend     bool
	expectedWindows      []string
	closedWindow         string
	closeCalls           int
	openedNamedKind      string
	openedNamedName      string
	openedSurfaces       []SurfaceTarget
	surfaceAfterAction   *BackendSurface
	actErr               error
	authActionCalls      int
	authActionErr        error
	authGeneration       string
	authActionStarted    chan struct{}
	authActionRelease    <-chan struct{}
	submitAuthStarted    chan struct{}
}

func (f *fakeBackend) Start(_ context.Context, profile Profile) error {
	f.startedWith = append(f.startedWith, profile)
	return nil
}
func (f *fakeBackend) Stop(context.Context) error                 { return nil }
func (f *fakeBackend) Probe(context.Context) (ProbeResult, error) { return f.probe, nil }
func (f *fakeBackend) Screenshot(context.Context) ([]byte, error) {
	f.screenshotCalls++
	return f.screenshot, nil
}
func (f *fakeBackend) SubmitAuthCode(context.Context, string) error {
	if f.submitAuthStarted != nil {
		close(f.submitAuthStarted)
	}
	return nil
}
func (f *fakeBackend) ContinueSavedAccountLogin(_ context.Context, generation string) error {
	f.authActionCalls++
	f.authGeneration = generation
	if f.authActionStarted != nil {
		close(f.authActionStarted)
	}
	if f.authActionRelease != nil {
		<-f.authActionRelease
	}
	return f.authActionErr
}
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
func (f *fakeBackend) OpenSurface(_ context.Context, target SurfaceTarget) (BackendSurface, error) {
	f.openedSurfaces = append(f.openedSurfaces, target)
	return f.surface, nil
}
func (f *fakeBackend) OpenNamedSurface(_ context.Context, kind, name string) (BackendSurface, error) {
	f.openedNamedKind = kind
	f.openedNamedName = name
	return f.surface, nil
}
func (f *fakeBackend) SnapshotSurface(_ context.Context, expectedWindow string) (BackendSurface, error) {
	f.expectedWindows = append(f.expectedWindows, expectedWindow)
	if expectedWindow != f.surface.WindowIdentity {
		return BackendSurface{}, ErrActionStale
	}
	return f.surface, nil
}
func (f *fakeBackend) ActSurface(_ context.Context, expectedWindow, locator, text string) (BackendSurface, error) {
	f.expectedWindows = append(f.expectedWindows, expectedWindow)
	if expectedWindow != f.surface.WindowIdentity {
		return BackendSurface{}, ErrActionStale
	}
	f.actions = append(f.actions, locator)
	f.actionTexts = append(f.actionTexts, text)
	if f.actErr != nil {
		return BackendSurface{}, f.actErr
	}
	if f.surfaceAfterAction != nil {
		f.surface = *f.surfaceAfterAction
		f.surfaceAfterAction = nil
	}
	return f.surface, nil
}

func TestMediumSurfaceInputRequiresConfirmationAndForwardsText(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{Kind: "miniprogram", Actions: []BackendAction{{
			Action: shared.Action{
				ID: "canvas-input", Label: "Search", Kind: "input", Risk: "medium", Effect: "unknown",
			},
			ReplayID: "canvas-input-replay", Locator: "canvas-input-locator",
		}}},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	request := shared.SurfaceAction{ActionID: "canvas-input", Text: "宿舍", TextProvided: true}
	if _, err := driver.ActSurface(context.Background(), surface.ID, request); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed canvas input error = %v, want ErrConfirmationRequired", err)
	}
	if len(backend.actions) != 0 || len(backend.actionTexts) != 0 {
		t.Fatalf("unconfirmed canvas input reached backend: actions=%#v text=%#v", backend.actions, backend.actionTexts)
	}
	request.Confirmed = true
	if _, err := driver.ActSurface(context.Background(), surface.ID, request); err != nil {
		t.Fatal(err)
	}
	if len(backend.actions) != 1 || backend.actions[0] != "canvas-input-locator" ||
		len(backend.actionTexts) != 1 || backend.actionTexts[0] != "宿舍" {
		t.Fatalf("confirmed canvas input dispatch = actions=%#v text=%#v", backend.actions, backend.actionTexts)
	}
}

func TestSurfaceActionTextPresenceIsStrict(t *testing.T) {
	tests := []struct {
		name         string
		actionKind   string
		request      shared.SurfaceAction
		wantError    string
		wantDispatch bool
	}{
		{
			name: "input requires text presence", actionKind: "input",
			request:   shared.SurfaceAction{ActionID: "action"},
			wantError: "text is required",
		},
		{
			name: "explicit empty clears input", actionKind: "input",
			request:      shared.SurfaceAction{ActionID: "action", TextProvided: true},
			wantDispatch: true,
		},
		{
			name: "non-input rejects explicit empty", actionKind: "activate",
			request:   shared.SurfaceAction{ActionID: "action", TextProvided: true},
			wantError: "text is only accepted",
		},
		{
			name: "non-input rejects inconsistent hidden text", actionKind: "activate",
			request:   shared.SurfaceAction{ActionID: "action", Text: "hidden"},
			wantError: "presence is inconsistent",
		},
		{
			name: "input rejects excessive text", actionKind: "input",
			request:   shared.SurfaceAction{ActionID: "action", Text: strings.Repeat("界", 4_097), TextProvided: true},
			wantError: "must not exceed 4096",
		},
		{
			name: "input rejects NUL", actionKind: "input",
			request:   shared.SurfaceAction{ActionID: "action", Text: "宿\x00舍", TextProvided: true},
			wantError: "contain NUL",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{
				probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
				surface: BackendSurface{Kind: "miniprogram", Actions: []BackendAction{{
					Action:   shared.Action{ID: "action", Label: "Action", Kind: test.actionKind, Risk: "low", Effect: "observe"},
					ReplayID: "action-replay", Locator: "action-locator",
				}}},
			}
			driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
			surface, err := driver.OpenSurface(context.Background(), "surface-ref")
			if err != nil {
				t.Fatal(err)
			}
			_, err = driver.ActSurface(context.Background(), surface.ID, test.request)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("ActSurface error = %v, want substring %q", err, test.wantError)
				}
				if kind, ok := shared.ClassifyFailure(err); !ok || kind != shared.FailureInvalidArgument {
					t.Fatalf("ActSurface failure classification = %q, %v; want %q", kind, ok, shared.FailureInvalidArgument)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := len(backend.actions); (got == 1) != test.wantDispatch {
				t.Fatalf("backend dispatch count = %d, want dispatch=%v", got, test.wantDispatch)
			}
			if test.wantDispatch && (len(backend.actionTexts) != 1 || backend.actionTexts[0] != "") {
				t.Fatalf("explicit empty input was not forwarded exactly: %#v", backend.actionTexts)
			}
		})
	}
}
func (f *fakeBackend) CloseSurface(_ context.Context, expectedWindow string) error {
	f.closeCalls++
	f.closedWindow = expectedWindow
	if expectedWindow != f.surface.WindowIdentity {
		return ErrActionStale
	}
	return nil
}

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
		Kind: "web", SurfaceLocator: "signed-card-locator",
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
				{Action: shared.Action{ID: "share-1", Kind: "share"}, ReplayID: "share-replay-1", Locator: "share-locator-1"},
				{Action: shared.Action{ID: "share-2", Kind: "share"}, ReplayID: "share-replay-2", Locator: "share-locator-2"},
			},
			wantErr: ErrTargetAmbiguous,
		},
		{
			name: "blocked",
			actions: []BackendAction{
				{Action: shared.Action{ID: "share-pay", Kind: "share", Risk: "high", Disabled: true}, ReplayID: "share-pay-replay", Locator: "share-pay-locator"},
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
			{Action: shared.Action{ID: "read", Label: "Read", Kind: "activate", Risk: "low"}, ReplayID: "read-replay", Locator: "locator-read"},
			{Action: shared.Action{ID: "pay", Label: "Pay", Kind: "activate", Risk: "high", Disabled: true}, ReplayID: "pay-replay", Locator: "locator-pay"},
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

func TestOpenNamedMiniProgramUsesFrameBoundBackendOperation(t *testing.T) {
	screenshot := testSurfacePNG(t, 6, 5)
	digest := sha256.Sum256(screenshot)
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{
			Kind: "miniprogram", Title: "Campus", Generation: "generation-1",
			Screenshot: screenshot, ScreenshotSHA256: hex.EncodeToString(digest[:]),
			WindowIdentity: "mini-window-1",
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenNamedSurface(context.Background(), shared.NamedSurface{Kind: "miniprogram", Name: " 校园瞄 "})
	if err != nil {
		t.Fatal(err)
	}
	if backend.openedNamedKind != "miniprogram" || backend.openedNamedName != "校园瞄" {
		t.Fatalf("named backend target = %q/%q", backend.openedNamedKind, backend.openedNamedName)
	}
	if surface.Generation != "generation-1" || surface.ScreenshotSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("surface is not frame-bound: %#v", surface)
	}
}

func TestSurfaceActionRequiresConfirmationAndIsConsumedBeforeFailure(t *testing.T) {
	backendFailure := errors.New("backend timed out after dispatch")
	backend := &fakeBackend{
		probe:  ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		actErr: backendFailure,
		surface: BackendSurface{Kind: "web", Actions: []BackendAction{{
			Action:   shared.Action{ID: "navigate", Label: "Open", Kind: "activate", Risk: "medium", Effect: "navigate"},
			ReplayID: "navigate-replay",
			Locator:  "locator-navigate",
		}}},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	request := shared.SurfaceAction{ActionID: "navigate"}
	if _, err := driver.ActSurface(context.Background(), surface.ID, request); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed medium-risk action error = %v", err)
	}
	if len(backend.actions) != 0 {
		t.Fatalf("unconfirmed action reached backend: %#v", backend.actions)
	}
	request.Confirmed = true
	if _, err := driver.ActSurface(context.Background(), surface.ID, request); !errors.Is(err, backendFailure) {
		t.Fatalf("confirmed action error = %v", err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, request); !errors.Is(err, ErrActionStale) {
		t.Fatalf("retried consumed action error = %v", err)
	}
	if len(backend.actions) != 1 {
		t.Fatalf("consumed action dispatched %d times", len(backend.actions))
	}
}

func TestConsumedActionDoesNotReappearAcrossGeneration(t *testing.T) {
	currentTime := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	screenshot := testSurfacePNG(t, 4, 4)
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: currentTime},
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "stable-generation", Screenshot: screenshot,
			WindowIdentity: "stable-window",
			Elements: []shared.SurfaceElement{{
				ID: "stable-element", TargetID: "stable-target", Label: "Details",
				ActionID: "stable-action", ActionIDs: []string{"stable-action"},
			}},
			Actions: []BackendAction{{
				Action: shared.Action{
					ID: "stable-action", TargetID: "stable-target", Kind: "activate",
					Risk: "low", Effect: "navigate",
				},
				ReplayID: "stable-replay",
				Locator:  "fresh-locator-issued-at-first-snapshot",
			}},
		},
	}
	temporary := t.TempDir()
	driver, err := New(Config{
		Backend: backend, Index: &fakeIndex{available: true},
		VerificationTimeout: time.Millisecond, Now: func() time.Time { return currentTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main", StateDir: filepath.Join(temporary, "state"),
		RuntimeDir: filepath.Join(temporary, "runtime"),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	acted, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "stable-action"})
	if err != nil {
		t.Fatal(err)
	}
	if len(acted.Actions) != 0 || len(acted.Elements) != 1 || acted.Elements[0].ActionID != "" || len(acted.Elements[0].ActionIDs) != 0 {
		t.Fatalf("consumed action reappeared in action response: actions=%#v elements=%#v", acted.Actions, acted.Elements)
	}
	currentTime = currentTime.Add(2 * time.Second)
	backend.surface.Generation = "dynamic-generation"
	backend.surface.Actions[0].Locator = "fresh-locator-issued-two-seconds-later"
	resnapshot, err := driver.SnapshotSurface(context.Background(), surface.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resnapshot.Generation != "dynamic-generation" {
		t.Fatalf("surface generation = %q, want dynamic-generation", resnapshot.Generation)
	}
	if len(resnapshot.Actions) != 0 || resnapshot.Elements[0].ActionID != "" || len(resnapshot.Elements[0].ActionIDs) != 0 {
		t.Fatalf("consumed stable action reappeared after generation change: actions=%#v elements=%#v", resnapshot.Actions, resnapshot.Elements)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "stable-action"}); !errors.Is(err, ErrActionStale) {
		t.Fatalf("consumed stable action retry error = %v", err)
	}
	if len(backend.actions) != 1 {
		t.Fatalf("consumed stable action dispatched %d times", len(backend.actions))
	}
}

func TestSuccessfulNavigationRequiresNewContextActionID(t *testing.T) {
	screenshot := testSurfacePNG(t, 4, 4)
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "page-a", Screenshot: screenshot,
			WindowIdentity: "stable-window", SemanticText: "Page A body",
			Actions: []BackendAction{{
				Action:   shared.Action{ID: "back-action", Kind: "back", Risk: "low", Effect: "navigate"},
				ReplayID: "back-replay", Locator: "back-on-page-a",
			}},
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	backend.surface.Generation = "page-b"
	backend.surface.SemanticText = "Page B body"
	backend.surface.Actions[0].Action.ID = "back-action-b"
	backend.surface.Actions[0].Locator = "back-on-page-b"
	pageB, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "back-action"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pageB.Actions) != 1 || pageB.Actions[0].ID != "back-action-b" {
		t.Fatalf("successful navigation did not advertise the new contextual action: %#v", pageB.Actions)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "back-action"}); err != nil {
		if !errors.Is(err, ErrActionStale) {
			t.Fatalf("old page action error = %v, want ErrActionStale", err)
		}
	} else {
		t.Fatal("old page action remained callable")
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "back-action-b"}); err != nil {
		t.Fatal(err)
	}
	if len(backend.actions) != 2 || backend.actions[0] != "back-on-page-a" || backend.actions[1] != "back-on-page-b" {
		t.Fatalf("contextual back dispatches = %#v", backend.actions)
	}
}

func TestUncertainScrollCanRecoverFromNewSnapshot(t *testing.T) {
	backendFailure := errors.New("scroll observation timed out")
	backend := &fakeBackend{
		probe:  ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		actErr: backendFailure,
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "viewport-a", Screenshot: testSurfacePNG(t, 4, 4),
			WindowIdentity: "stable-window",
			Actions: []BackendAction{{
				Action:   shared.Action{ID: "scroll-a", Kind: "scroll", Risk: "low", Effect: "observe"},
				ReplayID: "scroll-down-replay", Locator: "scroll-down-a",
			}},
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "scroll-a"}); !errors.Is(err, backendFailure) {
		t.Fatalf("uncertain scroll error = %v", err)
	}
	backend.actErr = nil
	backend.surface.Generation = "viewport-b"
	backend.surface.Actions[0].Action.ID = "scroll-b"
	backend.surface.Actions[0].Locator = "scroll-down-b"
	refreshed, err := driver.SnapshotSurface(context.Background(), surface.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed.Actions) != 1 || refreshed.Actions[0].ID != "scroll-b" {
		t.Fatalf("new viewport did not recover scroll action: %#v", refreshed.Actions)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "scroll-b"}); err != nil {
		t.Fatal(err)
	}
	if len(backend.actions) != 2 || backend.actions[0] != "scroll-down-a" || backend.actions[1] != "scroll-down-b" {
		t.Fatalf("scroll dispatches = %#v", backend.actions)
	}
}

func TestSuccessfulMutationReplayStaysConsumedAcrossContext(t *testing.T) {
	for _, effect := range []string{"external_write", "unknown"} {
		t.Run(effect, func(t *testing.T) {
			backend := &fakeBackend{
				probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
				surface: BackendSurface{
					Kind: "miniprogram", Generation: "page-a", Screenshot: testSurfacePNG(t, 3, 3),
					WindowIdentity: "stable-window",
					Actions: []BackendAction{{
						Action:   shared.Action{ID: "write-action", Kind: "activate", Risk: "medium", Effect: effect},
						ReplayID: "write-replay", Locator: "write-on-page-a",
					}},
				},
			}
			driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
			surface, err := driver.OpenSurface(context.Background(), "surface-ref")
			if err != nil {
				t.Fatal(err)
			}
			backend.surface.Generation = "page-b"
			backend.surface.Screenshot = testSurfacePNG(t, 4, 3)
			backend.surface.Actions[0].Action.ID = "write-action-b"
			backend.surface.Actions[0].Locator = "write-on-page-b"
			acted, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{
				ActionID: "write-action", Confirmed: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(acted.Actions) != 0 {
				t.Fatalf("%s replay reappeared under a new contextual ID: %#v", effect, acted.Actions)
			}
		})
	}
}

func TestSuccessfulMutationReplayStaysConsumedAcrossNewWindow(t *testing.T) {
	for _, effect := range []string{"external_write", "unknown"} {
		t.Run(effect, func(t *testing.T) {
			backend := &fakeBackend{
				probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
				surface: BackendSurface{
					Kind: "miniprogram", Generation: "page-a", Screenshot: testSurfacePNG(t, 3, 3),
					WindowIdentity: "window-a",
					Actions: []BackendAction{{
						Action: shared.Action{
							ID: "write-a", Kind: "activate", Risk: "medium", Effect: effect,
						},
						ReplayID: "write-replay", Locator: "write-on-window-a",
					}},
				},
			}
			driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
			surface, err := driver.OpenSurface(context.Background(), "surface-ref")
			if err != nil {
				t.Fatal(err)
			}
			next := backend.surface
			next.Generation = "page-b"
			next.WindowIdentity = "window-b"
			next.Screenshot = testSurfacePNG(t, 4, 3)
			next.Actions[0].Action.ID = "write-b"
			next.Actions[0].Locator = "write-on-window-b"
			backend.surfaceAfterAction = &next

			acted, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{
				ActionID: "write-a", Confirmed: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(acted.Actions) != 0 {
				t.Fatalf("%s replay reappeared after a window transition: %#v", effect, acted.Actions)
			}
		})
	}
}

func TestConfirmedUnknownVisualActionClearsNewElementLinkAndTombstonesReplay(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "visual-a", Screenshot: testSurfacePNG(t, 5, 4),
			WindowIdentity: "stable-window",
			Elements: []shared.SurfaceElement{{
				ID: "visual-element", TargetID: "visual-target", ActionID: "visual-a", ActionIDs: []string{"visual-a"},
			}},
			Actions: []BackendAction{{
				Action: shared.Action{
					ID: "visual-a", TargetID: "visual-target", Kind: "visual_activate",
					Risk: "medium", Effect: "unknown",
				},
				ReplayID: "visual-replay", Locator: "ocr-locator-a",
			}},
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	backend.surface.Generation = "visual-b"
	backend.surface.Screenshot = testSurfacePNG(t, 6, 4)
	backend.surface.Elements[0].ActionID = "visual-b"
	backend.surface.Elements[0].ActionIDs = []string{"visual-b"}
	backend.surface.Actions[0].Action.ID = "visual-b"
	backend.surface.Actions[0].Locator = "ocr-locator-b"
	acted, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{
		ActionID: "visual-a", Confirmed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(acted.Actions) != 0 || len(acted.Elements) != 1 || acted.Elements[0].ActionID != "" || len(acted.Elements[0].ActionIDs) != 0 {
		t.Fatalf("confirmed visual replay reappeared after remote pixel/context change: actions=%#v elements=%#v", acted.Actions, acted.Elements)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "visual-b", Confirmed: true}); !errors.Is(err, ErrActionStale) {
		t.Fatalf("confirmed visual replay retry error = %v", err)
	}
}

func TestSuccessfulSearchInputCanUseNewContextActionID(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "search-a", Screenshot: testSurfacePNG(t, 4, 4),
			WindowIdentity: "stable-window",
			Actions: []BackendAction{{
				Action:   shared.Action{ID: "search-a", Kind: "input", Risk: "low", Effect: "search_input"},
				ReplayID: "search-replay", Locator: "search-locator-a",
			}},
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	backend.surface.Generation = "search-b"
	backend.surface.Actions[0].Action.ID = "search-b"
	backend.surface.Actions[0].Locator = "search-locator-b"
	acted, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{
		ActionID: "search-a", Text: "dorm", TextProvided: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(acted.Actions) != 1 || acted.Actions[0].ID != "search-b" {
		t.Fatalf("verified replacement input did not receive a new contextual action: %#v", acted.Actions)
	}
}

func TestUncertainReplayDoesNotReappearWithNewContextActionID(t *testing.T) {
	backendFailure := errors.New("backend timed out after dispatch")
	backend := &fakeBackend{
		probe:  ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		actErr: backendFailure,
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "page-a", Screenshot: testSurfacePNG(t, 4, 4),
			WindowIdentity: "stable-window",
			Elements: []shared.SurfaceElement{{
				ID: "element-a", TargetID: "target-a", ActionID: "context-a", ActionIDs: []string{"context-a"},
			}},
			Actions: []BackendAction{{
				Action:   shared.Action{ID: "context-a", TargetID: "target-a", Kind: "activate", Risk: "low", Effect: "navigate"},
				ReplayID: "stable-replay", Locator: "locator-a",
			}},
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "context-a"}); !errors.Is(err, backendFailure) {
		t.Fatalf("uncertain action error = %v", err)
	}
	backend.actErr = nil
	backend.surface.Generation = "page-b"
	backend.surface.Elements[0].ActionID = "context-b"
	backend.surface.Elements[0].ActionIDs = []string{"context-b"}
	backend.surface.Actions[0].Action.ID = "context-b"
	backend.surface.Actions[0].Locator = "locator-b"
	resnapshot, err := driver.SnapshotSurface(context.Background(), surface.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resnapshot.Actions) != 0 || resnapshot.Elements[0].ActionID != "" || len(resnapshot.Elements[0].ActionIDs) != 0 {
		t.Fatalf("uncertain replay reappeared in a new context: actions=%#v elements=%#v", resnapshot.Actions, resnapshot.Elements)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "context-b"}); !errors.Is(err, ErrActionStale) {
		t.Fatalf("uncertain replay retry error = %v", err)
	}
	if len(backend.actions) != 1 {
		t.Fatalf("uncertain replay dispatched %d times", len(backend.actions))
	}
}

func TestSurfaceWindowSwitchFailsClosed(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{Kind: "web", Actions: []BackendAction{{
			Action: shared.Action{ID: "read", Kind: "activate", Risk: "low"}, ReplayID: "read-replay", Locator: "locator-read",
		}}},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	backend.surface.WindowIdentity = "unrelated-window"
	if _, err := driver.SnapshotSurface(context.Background(), surface.ID); !errors.Is(err, ErrActionStale) {
		t.Fatalf("snapshot after window switch error = %v", err)
	}
	if _, err := driver.ActSurface(context.Background(), surface.ID, shared.SurfaceAction{ActionID: "read"}); !errors.Is(err, ErrActionStale) {
		t.Fatalf("action after window switch error = %v", err)
	}
	if len(backend.actions) != 0 {
		t.Fatalf("window-switched action reached backend: %#v", backend.actions)
	}
	if err := driver.CloseSurface(context.Background(), surface.ID); !errors.Is(err, ErrActionStale) {
		t.Fatalf("close after window switch error = %v", err)
	}
	if err := driver.CloseSurface(context.Background(), surface.ID); !errors.Is(err, ErrSurfaceMissing) {
		t.Fatalf("retried close error = %v", err)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("close dispatched %d times", backend.closeCalls)
	}
}

func TestSurfaceAssetExportCropsLatestExactScreenshot(t *testing.T) {
	screenshot := testSurfacePNG(t, 8, 7)
	digest := sha256.Sum256(screenshot)
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		surface: BackendSurface{
			Kind: "miniprogram", Generation: "generation-1", Screenshot: screenshot,
			ScreenshotSHA256: hex.EncodeToString(digest[:]), WindowIdentity: "mini-window",
			Assets: []shared.SurfaceAsset{{
				ID: "image-1", Token: "backend-token", Kind: "image",
				Bounds: shared.Bounds{X: 2, Y: 1, Width: 3, Height: 4}, Source: "atspi", Confidence: 1,
			}},
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{available: true})
	surface, err := driver.OpenSurface(context.Background(), "surface-ref")
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Assets) != 1 || surface.Assets[0].Token == "" || surface.Assets[0].Token == "backend-token" {
		t.Fatalf("asset token was not wrapped: %#v", surface.Assets)
	}
	exported, err := driver.ExportSurfaceAsset(context.Background(), surface.ID, surface.Assets[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	imageValue, err := png.Decode(bytes.NewReader(exported.Data))
	if err != nil {
		t.Fatal(err)
	}
	if imageValue.Bounds().Dx() != 3 || imageValue.Bounds().Dy() != 4 || exported.Fidelity != "rendered" {
		t.Fatalf("rendered export = %#v bounds=%v", exported, imageValue.Bounds())
	}
	refreshed, err := driver.SnapshotSurface(context.Background(), surface.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed.Assets) != 1 || refreshed.Assets[0].Token == surface.Assets[0].Token {
		t.Fatalf("snapshot did not rotate asset token: old=%q new=%#v", surface.Assets[0].Token, refreshed.Assets)
	}
	if _, err := driver.ExportSurfaceAsset(context.Background(), surface.ID, surface.Assets[0].Token); !errors.Is(err, ErrAssetStale) {
		t.Fatalf("old asset token error = %v", err)
	}
}

func testSurfacePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	imageValue := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			imageValue.Set(x, y, color.RGBA{R: uint8(x * 13), G: uint8(y * 17), B: 99, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageValue); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestAuthSnapshotPassesThroughSavedAccountAction(t *testing.T) {
	generation := strings.Repeat("a", 64)
	action := savedAccountLoginAction(generation)
	screenshot := testSurfacePNG(t, 8, 8)
	backend := &fakeBackend{
		probe: ProbeResult{
			State: shared.StateAuthRequired, AuthKind: shared.AuthPhoneConfirm,
			Prompt: "Confirm the saved account", Actions: []shared.AuthAction{action},
			ScreenshotPNG: screenshot, ObservedAt: time.Now().UTC(),
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{})
	snapshot, err := driver.AuthSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != shared.StateAuthRequired || snapshot.Kind != shared.AuthPhoneConfirm ||
		len(snapshot.Actions) != 1 || snapshot.Actions[0] != action {
		t.Fatalf("saved-account authentication snapshot = %#v", snapshot)
	}
	snapshot.Actions[0].ID = "mutated-by-caller"
	if backend.probe.Actions[0].ID != savedAccountLoginActionPrefix+generation {
		t.Fatal("AuthSnapshot exposed the backend action slice to caller mutation")
	}
	if !bytes.Equal(snapshot.ScreenshotPNG, screenshot) {
		t.Fatal("AuthSnapshot did not preserve the screenshot captured with the dynamic action")
	}
	if backend.screenshotCalls != 0 {
		t.Fatalf("image-bound AuthSnapshot performed %d unbound screenshot captures", backend.screenshotCalls)
	}
}

func TestSavedAccountLoginActionUsesFixedInternalReplayKeyAcrossGenerations(t *testing.T) {
	first := savedAccountLoginAction(strings.Repeat("a", 64))
	second := savedAccountLoginAction(strings.Repeat("b", 64))
	if first.ID == second.ID || first.ReplayKey != continueSavedAccountLoginOperation ||
		second.ReplayKey != continueSavedAccountLoginOperation {
		t.Fatalf("saved-account replay binding first=%#v second=%#v", first, second)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"ReplayKey"`)) || bytes.Contains(encoded, []byte(`"replay_key"`)) {
		t.Fatalf("internal saved-account replay key escaped JSON: %s", encoded)
	}
}

func TestAuthSnapshotRejectsImageBoundActionWithoutAtomicScreenshot(t *testing.T) {
	generation := strings.Repeat("f", 64)
	backend := &fakeBackend{
		probe: ProbeResult{
			State: shared.StateAuthRequired, AuthKind: shared.AuthPhoneConfirm,
			Actions: []shared.AuthAction{savedAccountLoginAction(generation)}, ObservedAt: time.Now().UTC(),
		},
		screenshot: testSurfacePNG(t, 8, 8),
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{})
	if _, err := driver.AuthSnapshot(context.Background()); !errors.Is(err, ErrClientIncompatible) {
		t.Fatalf("unbound authentication screenshot error = %v, want ErrClientIncompatible", err)
	}
	if backend.screenshotCalls != 0 {
		t.Fatal("driver tried to repair an image-bound action with a later screenshot")
	}
}

func TestPerformSavedAccountLoginRequiresAdvertisedConfirmedFixedAction(t *testing.T) {
	generation := strings.Repeat("b", 64)
	action := savedAccountLoginAction(generation)
	backend := &fakeBackend{
		probe: ProbeResult{
			State: shared.StateAuthRequired, AuthKind: shared.AuthPhoneConfirm,
			Actions: []shared.AuthAction{action}, ObservedAt: time.Now().UTC(),
		},
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{})

	for _, invalidID := range []string{
		"arbitrary-login-action",
		savedAccountLoginActionPrefix + strings.Repeat("a", 63),
		savedAccountLoginActionPrefix + strings.Repeat("A", 64),
		savedAccountLoginActionPrefix + strings.Repeat("a", 65),
	} {
		if err := driver.PerformAuthAction(context.Background(), shared.AuthActionRequest{
			ActionID: invalidID, Confirmed: true,
		}); !errors.Is(err, ErrActionStale) {
			t.Fatalf("invalid authentication action %q error = %v, want ErrActionStale", invalidID, err)
		}
	}
	if err := driver.PerformAuthAction(context.Background(), shared.AuthActionRequest{
		ActionID: action.ID,
	}); !errors.Is(err, ErrUserActionRequired) {
		t.Fatalf("unconfirmed authentication action error = %v, want ErrUserActionRequired", err)
	}
	if backend.authActionCalls != 0 {
		t.Fatalf("invalid authentication actions reached backend %d times", backend.authActionCalls)
	}

	backend.probe.Actions = []shared.AuthAction{savedAccountLoginAction(strings.Repeat("c", 64))}
	if err := driver.PerformAuthAction(context.Background(), shared.AuthActionRequest{
		ActionID: action.ID, Confirmed: true,
	}); !errors.Is(err, ErrActionStale) {
		t.Fatalf("changed-account authentication action error = %v, want ErrActionStale", err)
	}
	if backend.authActionCalls != 0 {
		t.Fatal("stale account-bound authentication action reached backend")
	}
}

func TestPerformSavedAccountLoginPreservesConsumedOutcome(t *testing.T) {
	generation := strings.Repeat("d", 64)
	action := savedAccountLoginAction(generation)
	dispatchErr := errors.New("control response was lost after dispatch")
	backend := &fakeBackend{
		probe: ProbeResult{
			State: shared.StateAuthRequired, AuthKind: shared.AuthPhoneConfirm,
			Actions: []shared.AuthAction{action}, ObservedAt: time.Now().UTC(),
		},
		authActionErr: dispatchErr,
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{})
	err := driver.PerformAuthAction(context.Background(), shared.AuthActionRequest{
		ActionID: action.ID, Confirmed: true,
	})
	if !errors.Is(err, dispatchErr) || !shared.AuthActionWasConsumed(err) {
		t.Fatalf("uncertain authentication action error = %v, want consumed original cause", err)
	}
	if backend.authActionCalls != 1 {
		t.Fatalf("authentication action dispatches = %d, want 1", backend.authActionCalls)
	}
	if backend.authGeneration != generation {
		t.Fatalf("backend expected generation = %q, want %q", backend.authGeneration, generation)
	}

	backend.authActionErr = ErrActionStale
	err = driver.PerformAuthAction(context.Background(), shared.AuthActionRequest{
		ActionID: action.ID, Confirmed: true,
	})
	if !errors.Is(err, ErrActionStale) || shared.AuthActionWasConsumed(err) {
		t.Fatalf("explicit pre-dispatch rejection = %v, want unconsumed ErrActionStale", err)
	}
}

func TestPerformSavedAccountLoginSerializesAuthenticationMutation(t *testing.T) {
	generation := strings.Repeat("e", 64)
	action := savedAccountLoginAction(generation)
	actionStarted := make(chan struct{})
	actionRelease := make(chan struct{})
	codeStarted := make(chan struct{})
	backend := &fakeBackend{
		probe: ProbeResult{
			State: shared.StateAuthRequired, AuthKind: shared.AuthPhoneConfirm,
			Actions: []shared.AuthAction{action}, ObservedAt: time.Now().UTC(),
		},
		authActionStarted: actionStarted,
		authActionRelease: actionRelease,
		submitAuthStarted: codeStarted,
	}
	driver := startFixtureDriver(t, backend, &fakeIndex{})
	actionDone := make(chan error, 1)
	go func() {
		actionDone <- driver.PerformAuthAction(context.Background(), shared.AuthActionRequest{
			ActionID: action.ID, Confirmed: true,
		})
	}()
	select {
	case <-actionStarted:
	case <-time.After(time.Second):
		t.Fatal("authentication action did not reach backend")
	}
	codeDone := make(chan error, 1)
	go func() { codeDone <- driver.SubmitAuthCode(context.Background(), "123456") }()
	select {
	case <-codeStarted:
		t.Fatal("verification code interleaved with authentication action")
	case <-time.After(30 * time.Millisecond):
	}
	close(actionRelease)
	if err := <-actionDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-codeStarted:
	case <-time.After(time.Second):
		t.Fatal("verification code did not proceed after authentication action")
	}
	if err := <-codeDone; err != nil {
		t.Fatal(err)
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

func TestBoundedBytesBufferStopsAtExactLimit(t *testing.T) {
	buffer := boundedBytesBuffer{maximum: 4}
	if written, err := buffer.Write([]byte("abc")); err != nil || written != 3 {
		t.Fatalf("initial write = (%d, %v), want (3, nil)", written, err)
	}
	written, err := buffer.Write([]byte("de"))
	if written != 1 || !errors.Is(err, errRenderedAssetTooLarge) {
		t.Fatalf("overflow write = (%d, %v), want (1, size error)", written, err)
	}
	if got := buffer.String(); got != "abcd" {
		t.Fatalf("bounded contents = %q, want exact prefix", got)
	}
	if written, err := buffer.Write([]byte("z")); written != 0 || !errors.Is(err, errRenderedAssetTooLarge) {
		t.Fatalf("post-limit write = (%d, %v), want (0, size error)", written, err)
	}
}

func startFixtureDriver(t *testing.T, backend *fakeBackend, index MessageIndex) *Driver {
	t.Helper()
	if len(backend.surface.Screenshot) == 0 {
		backend.surface.Screenshot = testSurfacePNG(t, 2, 2)
	}
	if backend.surface.WindowIdentity == "" {
		backend.surface.WindowIdentity = "fixture-window"
	}
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
