package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
)

func TestChallengeUsesCorrectScopedRoutesAndPrivateCodeSubmission(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	manager := NewManager(config.Paths{Runtime: runtimeDir})
	t.Cleanup(manager.CloseAll)
	driverInstance := &challengeDriver{Driver: fake.New(driver.PlatformWeChat)}
	if err := driverInstance.Start(context.Background(), driver.AccountRuntime{
		AccountID: "account-1", Alias: "personal", StateDir: t.TempDir(), RuntimeDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	challenge, err := manager.Begin(context.Background(), "account-1", driverInstance, false, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(challenge.LocalURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname() != "127.0.0.1" || !strings.HasPrefix(parsed.Path, "/a/") {
		t.Fatalf("unexpected challenge URL %q", challenge.LocalURL)
	}
	response, err := http.Get(challenge.LocalURL) // #nosec G107 -- test URL is allocated by the loopback listener above.
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read login page: read=%v close=%v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login page status %d: %s", response.StatusCode, body)
	}
	for header, want := range map[string]string{
		"Cache-Control":          "no-store",
		"Pragma":                 "no-cache",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := response.Header.Get(header); got != want {
			t.Fatalf("login page header %s = %q, want %q", header, got, want)
		}
	}
	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("login page content security policy = %q", csp)
	}
	if !strings.Contains(string(body), `src="`+parsed.Path+`/image"`) {
		t.Fatalf("login image route is not challenge-scoped: %s", body)
	}
	if !strings.Contains(string(body), `href="`+parsed.Path+`/image" target="_blank"`) {
		t.Fatalf("login image does not link to the challenge-scoped full-size image: %s", body)
	}
	if !strings.Contains(string(body), `<form id="code" hidden>`) || !strings.Contains(string(body), `base+'/action'`) {
		t.Fatalf("login page does not gate code entry or expose the scoped user-action route: %s", body)
	}
	for _, lifecycleGuard := range []string{
		`let completed=false`,
		`if(completed||stateInFlight)return`,
		`generation!==uiGeneration`,
		`completed||generation!==imageGeneration`,
		`action.image_bound===true`,
		`bound.length!==available.length`,
		`bound.map(value=>value.id)`,
		`base+'/image?action_id='+encodeURIComponent(action.id)`,
		`signature!==actionSignature`,
		`screenLink.hidden=true`,
		`screen.replaceWith(candidate)`,
		`codeButton.disabled=true`,
		`codeButton.disabled=!state.can_submit_code`,
		`if(response.ok){form.hidden=true}`,
		`failure=await response.json()`,
		`failure&&failure.error&&failure.error.message`,
		`let actionNoticeUntil=0`,
		`Date.now()>=actionNoticeUntil`,
	} {
		if !strings.Contains(string(body), lifecycleGuard) {
			t.Fatalf("login page is missing lifecycle guard %q", lifecycleGuard)
		}
	}
	for _, responsiveRule := range []string{
		`*{box-sizing:border-box}`,
		`.screen-link{display:flex;width:100%;max-width:420px;max-height:70vh`,
		`.screen{display:block;width:auto;height:auto;max-width:100%;max-height:70vh;object-fit:contain`,
	} {
		if !strings.Contains(string(body), responsiveRule) {
			t.Fatalf("login page is missing responsive image rule %q", responsiveRule)
		}
	}
	imageResponse, err := http.Get(challenge.LocalURL + "/image?v=synthetic-cache-buster") // #nosec G107 -- test listener is allocated above.
	if err != nil {
		t.Fatal(err)
	}
	imageBody, readErr := io.ReadAll(imageResponse.Body)
	closeErr = imageResponse.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read login image: read=%v close=%v", readErr, closeErr)
	}
	if imageResponse.StatusCode != http.StatusOK || imageResponse.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("login image status=%d content-type=%q", imageResponse.StatusCode, imageResponse.Header.Get("Content-Type"))
	}
	if string(imageBody) != "fixture" {
		t.Fatalf("login image bytes changed: got %q", imageBody)
	}
	if imageResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("login image cache control = %q", imageResponse.Header.Get("Cache-Control"))
	}
	if info, err := os.Stat(challenge.LinkQRPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("link QR permissions: info=%v err=%v", info, err)
	}
	crossOriginCode, err := http.NewRequest(http.MethodPost, challenge.LocalURL+"/submit", strings.NewReader(`{"code":"123456"}`))
	if err != nil {
		t.Fatal(err)
	}
	crossOriginCode.Header.Set("Content-Type", "application/json")
	crossOriginCode.Header.Set("X-WeChatCopilot-Code", "user-entered")
	crossOriginResponse, err := http.DefaultClient.Do(crossOriginCode)
	if err != nil {
		t.Fatal(err)
	}
	_ = crossOriginResponse.Body.Close()
	if crossOriginResponse.StatusCode != http.StatusForbidden || driverInstance.submissions.Load() != 0 {
		t.Fatalf("cross-origin code status=%d calls=%d", crossOriginResponse.StatusCode, driverInstance.submissions.Load())
	}
	if err := manager.SubmitCode(context.Background(), challenge.ID, "bad code"); err == nil {
		t.Fatal("invalid verification code was accepted")
	}
	if err := manager.SubmitCode(context.Background(), challenge.ID, "123456"); err != nil {
		t.Fatal(err)
	}
	if driverInstance.submissions.Load() != 1 {
		t.Fatalf("driver received %d submissions", driverInstance.submissions.Load())
	}
	if err := manager.SubmitCode(context.Background(), challenge.ID, "123456"); !errors.Is(err, errCodeAlreadySubmitted) {
		t.Fatalf("replayed verification code error = %v, want already submitted", err)
	}
	if driverInstance.submissions.Load() != 1 {
		t.Fatalf("driver received %d submissions after replay", driverInstance.submissions.Load())
	}
	manager.CloseAll()
	if _, err := manager.Status(challenge.ID); !os.IsNotExist(err) {
		t.Fatalf("closed challenge status: %v", err)
	}
}

func TestChallengeAuthActionRequiresSameOriginExplicitConfirmationAndCannotReplay(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	manager := NewManager(config.Paths{Runtime: runtimeDir})
	t.Cleanup(manager.CloseAll)
	driverInstance := &authActionChallengeDriver{Driver: fake.New(driver.PlatformWeCom)}
	if err := driverInstance.Start(context.Background(), driver.AccountRuntime{
		AccountID: "account-2", Alias: "work", StateDir: t.TempDir(), RuntimeDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	challenge, err := manager.Begin(context.Background(), "account-2", driverInstance, false, "")
	if err != nil {
		t.Fatal(err)
	}

	stateResponse, err := http.Get(challenge.LocalURL + "/state") // #nosec G107 -- test listener is allocated above.
	if err != nil {
		t.Fatal(err)
	}
	stateBody, readErr := io.ReadAll(stateResponse.Body)
	closeErr := stateResponse.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read action state: read=%v close=%v", readErr, closeErr)
	}
	if stateResponse.StatusCode != http.StatusOK || !strings.Contains(string(stateBody), `"id":"accept_privacy_policy"`) || !strings.Contains(string(stateBody), `"can_submit_code":false`) {
		t.Fatalf("action state status=%d body=%s", stateResponse.StatusCode, stateBody)
	}

	request, err := http.NewRequest(http.MethodPost, challenge.LocalURL+"/action", strings.NewReader(`{"action_id":"accept_privacy_policy","confirmed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WeChatCopilot-Action", "user-confirmed")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden || driverInstance.actions.Load() != 0 {
		t.Fatalf("cross-origin action status=%d calls=%d", response.StatusCode, driverInstance.actions.Load())
	}

	response = performActionRequest(t, challenge.LocalURL, false)
	if response.StatusCode != http.StatusBadRequest || driverInstance.actions.Load() != 0 {
		t.Fatalf("unconfirmed action status=%d calls=%d", response.StatusCode, driverInstance.actions.Load())
	}
	_ = response.Body.Close()

	response = performActionRequest(t, challenge.LocalURL, true)
	if response.StatusCode != http.StatusOK || driverInstance.actions.Load() != 1 {
		t.Fatalf("confirmed action status=%d calls=%d", response.StatusCode, driverInstance.actions.Load())
	}
	_ = response.Body.Close()

	response = performActionRequest(t, challenge.LocalURL, true)
	if response.StatusCode != http.StatusConflict || driverInstance.actions.Load() != 1 {
		t.Fatalf("replayed action status=%d calls=%d", response.StatusCode, driverInstance.actions.Load())
	}
	_ = response.Body.Close()
}

func TestImageBoundAuthActionRejectsStateImageAndAccountRaces(t *testing.T) {
	actionA := driver.AuthAction{
		ID: "continue_saved_account_login." + strings.Repeat("a", 64), Label: "Alice",
		RequiresConfirmation: true, ImageBound: true,
	}
	actionB := driver.AuthAction{
		ID: "continue_saved_account_login." + strings.Repeat("b", 64), Label: "Bob",
		RequiresConfirmation: true, ImageBound: true,
	}
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{actionA},
		screenshot: []byte("alice-image"),
	}
	item := newAuthActionEntry(driverInstance)
	state := readChallengeState(t, item)
	if len(state.Actions) != 1 || state.Actions[0].ID != actionA.ID || !state.Actions[0].ImageBound {
		t.Fatalf("Alice state did not advertise its image-bound action: %#v", state.Actions)
	}

	driverInstance.mu.Lock()
	driverInstance.actions = []driver.AuthAction{actionB}
	driverInstance.screenshot = []byte("bob-image")
	driverInstance.mu.Unlock()

	staleImage := httptest.NewRecorder()
	item.handleImage(staleImage, httptest.NewRequest(
		http.MethodGet, "http://127.0.0.1/image?action_id="+url.QueryEscape(actionA.ID), nil,
	))
	if staleImage.Code != http.StatusConflict || staleImage.Body.String() == "bob-image" {
		t.Fatalf("stale Alice image binding status=%d body=%q", staleImage.Code, staleImage.Body.String())
	}
	if err := item.performAuthAction(context.Background(), actionA.ID, true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("stale Alice action error = %v, want unavailable", err)
	}
	if calls := driverInstance.callCount(actionA.ID); calls != 0 {
		t.Fatalf("stale Alice action reached driver %d times", calls)
	}

	currentImage := httptest.NewRecorder()
	item.handleImage(currentImage, httptest.NewRequest(
		http.MethodGet, "http://127.0.0.1/image?action_id="+url.QueryEscape(actionB.ID), nil,
	))
	if currentImage.Code != http.StatusOK || currentImage.Body.String() != "bob-image" {
		t.Fatalf("current Bob image binding status=%d body=%q", currentImage.Code, currentImage.Body.String())
	}
	if err := item.performAuthAction(context.Background(), actionB.ID, true); err != nil {
		t.Fatalf("current Bob action: %v", err)
	}
	if calls := driverInstance.callCount(actionB.ID); calls != 1 {
		t.Fatalf("current Bob action reached driver %d times", calls)
	}
}

func TestImageBindingRequiresUniqueImageBoundAction(t *testing.T) {
	action := driver.AuthAction{ID: "bound-action", Label: "Bound", ImageBound: true}
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{action, action},
		screenshot: []byte("ambiguous-image"),
	}
	item := newAuthActionEntry(driverInstance)
	response := httptest.NewRecorder()
	item.handleImage(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/image?action_id=bound-action", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate image-bound action status=%d body=%q", response.Code, response.Body.String())
	}

	driverInstance.mu.Lock()
	driverInstance.actions = []driver.AuthAction{{ID: "static-action", Label: "Static"}}
	driverInstance.screenshot = []byte("static-image")
	driverInstance.mu.Unlock()
	response = httptest.NewRecorder()
	item.handleImage(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/image?action_id=static-action", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("non-image-bound action image status=%d body=%q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	item.handleImage(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/image", nil))
	if response.Code != http.StatusOK || response.Body.String() != "static-image" {
		t.Fatalf("static action generic image regressed: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestDistinctImageBoundAuthActionsShareOneCurrentCapture(t *testing.T) {
	generation := strings.Repeat("c", 64)
	login := driver.AuthAction{
		ID: "continue_saved_account_login." + generation, Label: "Login",
		RequiresConfirmation: true, ImageBound: true, ReplayKey: "continue_saved_account_login",
	}
	switchAction := driver.AuthAction{
		ID: "switch_saved_account_login." + generation, Label: "Switch",
		RequiresConfirmation: true, ImageBound: true, ReplayKey: "switch_saved_account_login",
	}
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{login, switchAction},
		screenshot: []byte("shared-current-image"),
	}
	item := newAuthActionEntry(driverInstance)
	state := readChallengeState(t, item)
	if len(state.Actions) != 2 {
		t.Fatalf("distinct current actions = %#v", state.Actions)
	}
	for _, action := range []driver.AuthAction{login, switchAction} {
		response := httptest.NewRecorder()
		item.handleImage(response, httptest.NewRequest(
			http.MethodGet, "http://127.0.0.1/image?action_id="+url.QueryEscape(action.ID), nil,
		))
		if response.Code != http.StatusOK || response.Body.String() != "shared-current-image" {
			t.Fatalf("action %q image status=%d body=%q", action.ID, response.Code, response.Body.String())
		}
	}
	if err := item.performAuthAction(context.Background(), login.ID, true); err != nil {
		t.Fatal(err)
	}
	state = readChallengeState(t, item)
	if len(state.Actions) != 1 || state.Actions[0].ID != switchAction.ID {
		t.Fatalf("login consumption removed independent switch action: %#v", state.Actions)
	}
}

func TestChallengeAuthActionsRunSequentiallyAndRejectSuccessfulReplay(t *testing.T) {
	driverInstance := &sequentialAuthActionDriver{Driver: fake.New(driver.PlatformWeCom)}
	item := newAuthActionEntry(driverInstance)

	if err := item.performAuthAction(context.Background(), "accept_login_agreements", true); err != nil {
		t.Fatalf("perform first action: %v", err)
	}
	if err := item.performAuthAction(context.Background(), "accept_login_agreements", true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("replayed first action error = %v, want unavailable", err)
	}
	if err := item.performAuthAction(context.Background(), "continue_with_wechat", true); err != nil {
		t.Fatalf("perform second action: %v", err)
	}
	if err := item.performAuthAction(context.Background(), "continue_with_wechat", true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("replayed second action error = %v, want unavailable", err)
	}

	calls := driverInstance.actionCalls()
	if fmt.Sprint(calls) != "[accept_login_agreements continue_with_wechat]" {
		t.Fatalf("driver action calls = %v", calls)
	}
	if item.totalActionAttempts != 2 || item.actionAttempts["accept_login_agreements"] != 1 || item.actionAttempts["continue_with_wechat"] != 1 {
		t.Fatalf("action attempt accounting = total %d per-action %#v", item.totalActionAttempts, item.actionAttempts)
	}
}

func TestChallengeAuthActionsSerializeDifferentIDsAndHideInflightControls(t *testing.T) {
	base := &sequentialAuthActionDriver{Driver: fake.New(driver.PlatformWeCom)}
	driverInstance := &blockingSequentialAuthActionDriver{
		sequentialAuthActionDriver: base,
		started:                    make(chan struct{}),
		release:                    make(chan struct{}),
	}
	item := newAuthActionEntry(driverInstance)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- item.performAuthAction(context.Background(), "accept_login_agreements", true)
	}()
	select {
	case <-driverInstance.started:
	case <-time.After(time.Second):
		close(driverInstance.release)
		t.Fatal("first action did not reach the driver")
	}

	state := readChallengeState(t, item)
	if state.CanSubmitCode || len(state.Actions) != 0 {
		close(driverInstance.release)
		t.Fatalf("in-flight state exposed controls: %#v", state)
	}
	if err := item.performAuthAction(context.Background(), "continue_with_wechat", true); !errors.Is(err, errActionInFlight) {
		close(driverInstance.release)
		t.Fatalf("concurrent second action error = %v, want in-flight", err)
	}

	close(driverInstance.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first action: %v", err)
	}
	if err := item.performAuthAction(context.Background(), "continue_with_wechat", true); err != nil {
		t.Fatalf("sequential second action: %v", err)
	}
	if calls := driverInstance.actionCalls(); fmt.Sprint(calls) != "[accept_login_agreements continue_with_wechat]" {
		t.Fatalf("serialized driver action calls = %v", calls)
	}
}

func TestConsumedAuthActionErrorPreservesCauseAndRejectsReplay(t *testing.T) {
	postconditionErr := errors.New("post-action verification timed out")
	driverInstance := &fixedAuthActionDriver{
		Driver:  fake.New(driver.PlatformWeCom),
		actions: []driver.AuthAction{{ID: "accept_login_agreements", Label: "accept"}},
		results: map[string][]error{
			"accept_login_agreements": {fmt.Errorf("verify accepted action: %w", driver.MarkAuthActionConsumed(postconditionErr))},
		},
	}
	item := newAuthActionEntry(driverInstance)

	err := item.performAuthAction(context.Background(), "accept_login_agreements", true)
	if !errors.Is(err, postconditionErr) || !driver.AuthActionWasConsumed(err) {
		t.Fatalf("consumed error = %v, cause/consumed marker was lost", err)
	}
	if err := item.performAuthAction(context.Background(), "accept_login_agreements", true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("uncertain action replay error = %v, want unavailable", err)
	}
	if calls := driverInstance.callCount("accept_login_agreements"); calls != 1 {
		t.Fatalf("consumed action reached driver %d times", calls)
	}
	state := readChallengeState(t, item)
	if len(state.Actions) != 0 {
		t.Fatalf("consumed action was advertised again: %#v", state.Actions)
	}
}

func TestSavedAccountReplayKeySurvivesImageGenerationChangeAndDriverError(t *testing.T) {
	dispatchErr := errors.New("synthetic driver failure after invocation")
	actionA := driver.AuthAction{
		ID: "continue_saved_account_login." + strings.Repeat("a", 64), Label: "Saved account A",
		ImageBound: true, ReplayKey: "continue_saved_account_login",
	}
	actionB := driver.AuthAction{
		ID: "continue_saved_account_login." + strings.Repeat("b", 64), Label: "Saved account B",
		ImageBound: true, ReplayKey: "continue_saved_account_login",
	}
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{actionA},
		screenshot: []byte("saved-account-a-png"),
		results:    map[string][]error{actionA.ID: {dispatchErr}},
	}
	item := newAuthActionEntry(driverInstance)

	response := httptest.NewRecorder()
	item.handleState(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/state", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"ReplayKey"`) ||
		strings.Contains(response.Body.String(), `"replay_key"`) {
		t.Fatalf("internal replay key escaped challenge JSON: status=%d body=%q", response.Code, response.Body.String())
	}
	if err := item.performAuthAction(context.Background(), actionA.ID, true); !errors.Is(err, dispatchErr) {
		t.Fatalf("first generation error = %v", err)
	}

	driverInstance.mu.Lock()
	driverInstance.actions = []driver.AuthAction{actionB}
	driverInstance.screenshot = []byte("saved-account-b-png")
	driverInstance.mu.Unlock()
	state := readChallengeState(t, item)
	if len(state.Actions) != 0 {
		t.Fatalf("new image generation revived consumed logical action: %#v", state.Actions)
	}
	image := httptest.NewRecorder()
	item.handleImage(image, httptest.NewRequest(
		http.MethodGet, "http://127.0.0.1/image?action_id="+url.QueryEscape(actionB.ID), nil,
	))
	if image.Code != http.StatusConflict {
		t.Fatalf("consumed replay key image status=%d body=%q", image.Code, image.Body.String())
	}
	if err := item.performAuthAction(context.Background(), actionB.ID, true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("new image generation replay error = %v, want unavailable", err)
	}
	if driverInstance.callCount(actionA.ID) != 1 || driverInstance.callCount(actionB.ID) != 0 {
		t.Fatalf("logical operation dispatch counts A=%d B=%d", driverInstance.callCount(actionA.ID), driverInstance.callCount(actionB.ID))
	}
}

func TestDefinitiveAuthActionRejectionReleasesReplayTombstoneForBoundedRetry(t *testing.T) {
	action := driver.AuthAction{
		ID: "continue_saved_account_login." + strings.Repeat("e", 64), Label: "Saved account",
		ImageBound: true, ReplayKey: "continue_saved_account_login",
	}
	stale := driver.NewFailure(driver.FailureStale, "private driver detail must not escape")
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{action},
		screenshot: []byte("saved-account-png"),
		results:    map[string][]error{action.ID: {stale, stale}},
	}
	item := newAuthActionEntry(driverInstance)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := item.performAuthAction(context.Background(), action.ID, true); !errors.Is(err, stale) {
			t.Fatalf("definitive rejection %d = %v", attempt, err)
		}
		state := readChallengeState(t, item)
		if len(state.Actions) != 1 || state.Actions[0].ID != action.ID {
			t.Fatalf("definitively rejected action was not retryable after attempt %d: %#v", attempt, state.Actions)
		}
	}
	if err := item.performAuthAction(context.Background(), action.ID, true); err != nil {
		t.Fatalf("third bounded attempt: %v", err)
	}
	if calls := driverInstance.callCount(action.ID); calls != 3 {
		t.Fatalf("bounded retry reached driver %d times", calls)
	}
	if state := readChallengeState(t, item); len(state.Actions) != 0 {
		t.Fatalf("successful action remained available: %#v", state.Actions)
	}
}

func TestConsumedClassifiedAuthActionErrorNeverReleasesReplayTombstone(t *testing.T) {
	action := driver.AuthAction{
		ID: "continue_saved_account_login." + strings.Repeat("f", 64), Label: "Saved account",
		ImageBound: true, ReplayKey: "continue_saved_account_login",
	}
	staleAfterDispatch := driver.MarkAuthActionConsumed(
		driver.NewFailure(driver.FailureStale, "post-dispatch verification changed"),
	)
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{action},
		screenshot: []byte("saved-account-png"),
		results:    map[string][]error{action.ID: {staleAfterDispatch}},
	}
	item := newAuthActionEntry(driverInstance)

	if err := item.performAuthAction(context.Background(), action.ID, true); !driver.AuthActionWasConsumed(err) {
		t.Fatalf("consumed classified failure lost its marker: %v", err)
	}
	if state := readChallengeState(t, item); len(state.Actions) != 0 {
		t.Fatalf("consumed classified action was advertised again: %#v", state.Actions)
	}
	if err := item.performAuthAction(context.Background(), action.ID, true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("consumed classified action replay = %v", err)
	}
	if calls := driverInstance.callCount(action.ID); calls != 1 {
		t.Fatalf("consumed classified action reached driver %d times", calls)
	}
}

func TestAuthActionFailureResponseUsesStableSafeClassification(t *testing.T) {
	action := driver.AuthAction{ID: "safe-action", Label: "Safe", ReplayKey: "safe-action"}
	privateDetail := "private driver detail must not escape"
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{action},
		results: map[string][]error{
			action.ID: {driver.NewFailure(driver.FailureStale, privateDetail)},
		},
	}
	item := newAuthActionEntry(driverInstance)
	request := httptest.NewRequest(
		http.MethodPost, "http://127.0.0.1/action",
		strings.NewReader(`{"action_id":"safe-action","confirmed":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WeChatCopilot-Action", "user-confirmed")
	request.Header.Set("Origin", "http://127.0.0.1")
	response := httptest.NewRecorder()
	item.handleAction(response, request)

	if response.Code != http.StatusConflict || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("classified action response status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if strings.Contains(response.Body.String(), privateDetail) {
		t.Fatalf("private driver error escaped: %q", response.Body.String())
	}
	var payload struct {
		OK    bool `json:"ok"`
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode classified action response: %v", err)
	}
	if payload.OK || payload.Error.Code != "PAGE_CHANGED" || !payload.Error.Retryable || payload.Error.Message == "" {
		t.Fatalf("classified action payload = %#v", payload)
	}
	if state := readChallengeState(t, item); len(state.Actions) != 1 {
		t.Fatalf("safe rejection did not retain retryable action: %#v", state.Actions)
	}
}

func TestAuthActionReplayKeyIsPreconsumedBeforeConcurrentDriverCall(t *testing.T) {
	driverErr := errors.New("driver returned after dispatch attempt")
	actionA := driver.AuthAction{ID: "continue." + strings.Repeat("c", 64), ImageBound: true, ReplayKey: "continue_login"}
	actionB := driver.AuthAction{ID: "continue." + strings.Repeat("d", 64), ImageBound: true, ReplayKey: "continue_login"}
	base := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeCom), actions: []driver.AuthAction{actionA},
		results: map[string][]error{actionA.ID: {driverErr}},
	}
	driverInstance := &blockingReplayKeyAuthDriver{
		fixedAuthActionDriver: base, started: make(chan struct{}), release: make(chan struct{}),
	}
	item := newAuthActionEntry(driverInstance)
	firstDone := make(chan error, 1)
	go func() { firstDone <- item.performAuthAction(context.Background(), actionA.ID, true) }()
	select {
	case <-driverInstance.started:
	case <-time.After(time.Second):
		close(driverInstance.release)
		t.Fatal("first generation did not reach driver")
	}

	base.mu.Lock()
	base.actions = []driver.AuthAction{actionB}
	base.screenshot = []byte("changed-png")
	base.mu.Unlock()
	if err := item.performAuthAction(context.Background(), actionB.ID, true); !errors.Is(err, errActionInFlight) {
		close(driverInstance.release)
		t.Fatalf("concurrent generation error = %v, want in-flight", err)
	}
	close(driverInstance.release)
	if err := <-firstDone; !errors.Is(err, driverErr) {
		t.Fatalf("first generation error = %v", err)
	}
	if state := readChallengeState(t, item); len(state.Actions) != 0 {
		t.Fatalf("post-error generation was advertised: %#v", state.Actions)
	}
	if err := item.performAuthAction(context.Background(), actionB.ID, true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("post-error generation replay error = %v", err)
	}
	if base.callCount(actionA.ID) != 1 || base.callCount(actionB.ID) != 0 {
		t.Fatalf("concurrent logical operation dispatched A=%d B=%d", base.callCount(actionA.ID), base.callCount(actionB.ID))
	}
}

func TestSavedAccountReplayKeyIsScopedToOneChallengeAndAccount(t *testing.T) {
	actionA := driver.AuthAction{ID: "continue_saved_account_login." + strings.Repeat("a", 64), ReplayKey: "continue_saved_account_login"}
	actionB := driver.AuthAction{ID: "continue_saved_account_login." + strings.Repeat("b", 64), ReplayKey: "continue_saved_account_login"}
	actionC := driver.AuthAction{ID: "continue_saved_account_login." + strings.Repeat("c", 64), ReplayKey: "continue_saved_account_login"}
	firstDriver := &fixedAuthActionDriver{Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{actionA}}
	secondDriver := &fixedAuthActionDriver{Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{actionB}}
	thirdDriver := &fixedAuthActionDriver{Driver: fake.New(driver.PlatformWeChat), actions: []driver.AuthAction{actionC}}
	first := newAuthActionEntry(firstDriver)
	first.public.AccountID = "account-a"
	second := newAuthActionEntry(secondDriver)
	second.public.AccountID = "account-b"
	third := newAuthActionEntry(thirdDriver)
	third.public.AccountID = "account-a"
	if err := first.performAuthAction(context.Background(), actionA.ID, true); err != nil {
		t.Fatalf("first challenge action: %v", err)
	}
	if err := second.performAuthAction(context.Background(), actionB.ID, true); err != nil {
		t.Fatalf("second challenge action was polluted by first replay scope: %v", err)
	}
	if err := third.performAuthAction(context.Background(), actionC.ID, true); err != nil {
		t.Fatalf("new same-account challenge was polluted by prior replay scope: %v", err)
	}
	if firstDriver.callCount(actionA.ID) != 1 || secondDriver.callCount(actionB.ID) != 1 || thirdDriver.callCount(actionC.ID) != 1 {
		t.Fatalf(
			"challenge-local dispatch counts first=%d second=%d third=%d",
			firstDriver.callCount(actionA.ID), secondDriver.callCount(actionB.ID), thirdDriver.callCount(actionC.ID),
		)
	}
}

func TestDuplicateAuthActionReplayKeyFailsClosed(t *testing.T) {
	actionA := driver.AuthAction{ID: "policy.a", ReplayKey: "accept_policy"}
	actionB := driver.AuthAction{ID: "policy.b", ReplayKey: "accept_policy"}
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeCom), actions: []driver.AuthAction{actionA, actionB},
	}
	item := newAuthActionEntry(driverInstance)
	if state := readChallengeState(t, item); len(state.Actions) != 0 {
		t.Fatalf("duplicate replay scope was advertised: %#v", state.Actions)
	}
	if err := item.performAuthAction(context.Background(), actionA.ID, true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("duplicate replay scope action error = %v", err)
	}
	if driverInstance.callCount(actionA.ID) != 0 {
		t.Fatal("duplicate replay scope reached driver")
	}
}

func TestAuthActionPreconsumesFreshAdvertisedDispatchEvenOnDriverError(t *testing.T) {
	dispatchErr := errors.New("driver returned failure")
	driverInstance := &fixedAuthActionDriver{
		Driver: fake.New(driver.PlatformWeCom),
		actions: []driver.AuthAction{
			{ID: "action-a", Label: "A"},
			{ID: "action-b", Label: "B"},
		},
		results: map[string][]error{
			"action-a": {dispatchErr},
		},
	}
	item := newAuthActionEntry(driverInstance)

	if err := item.performAuthAction(context.Background(), "action-a", false); !errors.Is(err, errConfirmationRequired) {
		t.Fatalf("unconfirmed action error = %v", err)
	}
	if err := item.performAuthAction(context.Background(), "not-advertised", true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("unadvertised action error = %v", err)
	}
	if item.totalActionAttempts != 0 {
		t.Fatalf("non-dispatched requests consumed %d attempts", item.totalActionAttempts)
	}
	if err := item.performAuthAction(context.Background(), "action-a", true); !errors.Is(err, dispatchErr) {
		t.Fatalf("action-a error = %v", err)
	}
	if err := item.performAuthAction(context.Background(), "action-a", true); !errors.Is(err, errActionUnavailable) {
		t.Fatalf("action-a replay error = %v", err)
	}
	state := readChallengeState(t, item)
	if len(state.Actions) != 1 || state.Actions[0].ID != "action-b" {
		t.Fatalf("preconsumed failed action was advertised again: %#v", state.Actions)
	}
	if err := item.performAuthAction(context.Background(), "action-b", true); err != nil {
		t.Fatalf("action-b was starved by action-a failures: %v", err)
	}
	if item.totalActionAttempts != 2 || driverInstance.callCount("action-a") != 1 {
		t.Fatalf("dispatch accounting = total %d action-a calls %d", item.totalActionAttempts, driverInstance.callCount("action-a"))
	}
}

func TestAuthActionTotalAttemptLimitBoundsDistinctFailures(t *testing.T) {
	actions := make([]driver.AuthAction, 0, maxAuthActionAttemptsPerChallenge+1)
	results := make(map[string][]error, maxAuthActionAttemptsPerChallenge+1)
	for index := 0; index <= maxAuthActionAttemptsPerChallenge; index++ {
		id := fmt.Sprintf("action-%d", index)
		actions = append(actions, driver.AuthAction{ID: id, Label: id})
		results[id] = []error{errors.New("synthetic dispatch failure")}
	}
	driverInstance := &fixedAuthActionDriver{Driver: fake.New(driver.PlatformWeCom), actions: actions, results: results}
	item := newAuthActionEntry(driverInstance)
	for index := 0; index < maxAuthActionAttemptsPerChallenge; index++ {
		if err := item.performAuthAction(context.Background(), fmt.Sprintf("action-%d", index), true); err == nil || errors.Is(err, errTooManyActionAttempts) {
			t.Fatalf("bounded failure %d = %v", index, err)
		}
	}
	if err := item.performAuthAction(context.Background(), fmt.Sprintf("action-%d", maxAuthActionAttemptsPerChallenge), true); !errors.Is(err, errTooManyActionAttempts) {
		t.Fatalf("total over-limit error = %v", err)
	}
	if item.totalActionAttempts != maxAuthActionAttemptsPerChallenge {
		t.Fatalf("total action attempts = %d", item.totalActionAttempts)
	}
	state := readChallengeState(t, item)
	if len(state.Actions) != 0 {
		t.Fatalf("total attempt limit still advertised actions: %#v", state.Actions)
	}
}

func TestChallengeStateLatchesOnlineAndNeverReexposesControls(t *testing.T) {
	driverInstance := &mutableAuthStateDriver{
		Driver: fake.New(driver.PlatformWeCom),
		snapshot: driver.AuthSnapshot{
			State: driver.StateOnline, Kind: driver.AuthSMS, CanSubmitCode: true,
			Actions: []driver.AuthAction{{ID: "stale-action", Label: "stale"}}, ObservedAt: time.Now().UTC(),
		},
	}
	item := newAuthActionEntry(driverInstance)
	state := readChallengeState(t, item)
	if state.State != driver.StateStarting || state.CompletedAt != nil || state.CanSubmitCode || len(state.Actions) != 0 {
		t.Fatalf("first online observation was not provisional: %#v", state)
	}
	item.mu.Lock()
	item.onlineCandidateSince = time.Now().UTC().Add(-onlineStabilityWindow)
	item.onlineObservations = minimumStableOnlineObservations - 1
	item.mu.Unlock()
	state = readChallengeState(t, item)
	if state.State != driver.StateOnline || state.CompletedAt == nil || state.CanSubmitCode || len(state.Actions) != 0 {
		t.Fatalf("stable online state was not terminal: %#v", state)
	}

	driverInstance.setSnapshot(driver.AuthSnapshot{
		State: driver.StateAuthRequired, Kind: driver.AuthSMS, CanSubmitCode: true,
		Actions: []driver.AuthAction{{ID: "stale-action", Label: "stale"}}, ObservedAt: time.Now().UTC(),
	})
	state = readChallengeState(t, item)
	if state.State != driver.StateOnline || state.CompletedAt == nil || state.CanSubmitCode || len(state.Actions) != 0 {
		t.Fatalf("terminal state regressed after stale snapshot: %#v", state)
	}
	// The first provisional and second stable observations precede the
	// terminal request, which must not perform a third driver query.
	if calls := driverInstance.snapshotCalls.Load(); calls != 2 {
		t.Fatalf("terminal state queried driver again: calls=%d", calls)
	}
}

func TestChallengeTransientOnlineNeverCompletesAndResetsStability(t *testing.T) {
	driverInstance := &mutableAuthStateDriver{
		Driver: fake.New(driver.PlatformWeChat),
		snapshot: driver.AuthSnapshot{
			State: driver.StateOnline, ObservedAt: time.Now().UTC(),
		},
	}
	item := newAuthActionEntry(driverInstance)
	for observation := 0; observation < minimumStableOnlineObservations+2; observation++ {
		state := readChallengeState(t, item)
		if state.State != driver.StateStarting || state.CompletedAt != nil {
			t.Fatalf("rapid online observation %d completed challenge: %#v", observation, state)
		}
	}

	firstCandidate := item.onlineCandidateSince
	driverInstance.setSnapshot(driver.AuthSnapshot{
		State: driver.StateAuthRequired, Kind: driver.AuthPhoneConfirm,
		Prompt: "login required", ObservedAt: time.Now().UTC(),
	})
	state := readChallengeState(t, item)
	if state.State != driver.StateAuthRequired || state.CompletedAt != nil || state.Prompt != "login required" {
		t.Fatalf("online rollback did not return to authentication: %#v", state)
	}
	if !item.onlineCandidateSince.IsZero() || item.onlineObservations != 0 {
		t.Fatalf("online rollback retained candidate: since=%v observations=%d", item.onlineCandidateSince, item.onlineObservations)
	}

	driverInstance.setSnapshot(driver.AuthSnapshot{
		State: driver.StateOnline, ObservedAt: time.Now().UTC(),
	})
	state = readChallengeState(t, item)
	if state.State != driver.StateStarting || state.CompletedAt != nil ||
		item.onlineCandidateSince.IsZero() || !item.onlineCandidateSince.After(firstCandidate) {
		t.Fatalf("new online candidate reused rolled-back stability: state=%#v since=%v first=%v", state, item.onlineCandidateSince, firstCandidate)
	}
}

func TestDelayedStateSnapshotCannotOverwriteConcurrentCompletion(t *testing.T) {
	driverInstance := &delayedAuthStateDriver{
		Driver:  fake.New(driver.PlatformWeCom),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	item := newAuthActionEntry(driverInstance)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		item.handleState(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/state", nil))
		close(done)
	}()
	select {
	case <-driverInstance.started:
	case <-time.After(time.Second):
		close(driverInstance.release)
		t.Fatal("state request did not reach delayed snapshot")
	}
	item.mu.Lock()
	item.markCompletedLocked(time.Now().UTC())
	item.mu.Unlock()
	close(driverInstance.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delayed state request did not finish")
	}

	var state challengeStatePayload
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode delayed state: %v body=%q", err, response.Body.String())
	}
	if state.State != driver.StateOnline || state.CompletedAt == nil || state.CanSubmitCode || len(state.Actions) != 0 {
		t.Fatalf("delayed snapshot regressed completion: %#v", state)
	}
}

func TestVerificationCodeRequiresCurrentAdvertisedSMSInput(t *testing.T) {
	driverInstance := &challengeDriver{Driver: fake.New(driver.PlatformWeChat), kind: driver.AuthQR, canSubmitCode: false}
	item := &entry{
		public: Challenge{State: driver.StateAuthRequired, ExpiresAt: time.Now().UTC().Add(time.Minute)},
		driver: driverInstance,
	}
	if err := item.submitCode(context.Background(), "123456"); err == nil {
		t.Fatal("verification code was accepted without an advertised SMS input")
	}
	if driverInstance.submissions.Load() != 0 {
		t.Fatalf("driver received %d unavailable code submissions", driverInstance.submissions.Load())
	}
}

func TestVerificationCodeRejectsConcurrentAndSuccessfulReplay(t *testing.T) {
	driverInstance := &blockingCodeChallengeDriver{
		Driver: fake.New(driver.PlatformWeChat), started: make(chan struct{}), release: make(chan struct{}),
	}
	item := &entry{
		public: Challenge{State: driver.StateAuthRequired, ExpiresAt: time.Now().UTC().Add(time.Minute)},
		driver: driverInstance,
	}
	assertCodeUnavailable := func(stage string) {
		t.Helper()
		response := httptest.NewRecorder()
		item.handleState(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/state", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"can_submit_code":false`) {
			t.Fatalf("%s state status=%d body=%q", stage, response.Code, response.Body.String())
		}
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- item.submitCode(context.Background(), "123456") }()
	select {
	case <-driverInstance.started:
	case <-time.After(time.Second):
		close(driverInstance.release)
		t.Fatal("first verification submission did not reach the driver")
	}
	assertCodeUnavailable("in-flight")
	if err := item.submitCode(context.Background(), "654321"); !errors.Is(err, errCodeInFlight) {
		close(driverInstance.release)
		t.Fatalf("concurrent verification error = %v, want in flight", err)
	}
	close(driverInstance.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first verification submission: %v", err)
	}
	assertCodeUnavailable("submitted")
	if err := item.submitCode(context.Background(), "654321"); !errors.Is(err, errCodeAlreadySubmitted) {
		t.Fatalf("replayed verification error = %v, want already submitted", err)
	}
	if got := driverInstance.submissions.Load(); got != 1 {
		t.Fatalf("driver received %d verification submissions, want 1", got)
	}
}

func TestLoginImageRejectsSnapshotThatBecameOnline(t *testing.T) {
	driverInstance := &challengeDriver{Driver: fake.New(driver.PlatformWeChat), state: driver.StateOnline}
	item := &entry{
		public: Challenge{State: driver.StateAuthRequired, ExpiresAt: time.Now().UTC().Add(time.Minute)},
		driver: driverInstance,
	}
	response := httptest.NewRecorder()
	item.handleImage(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/image", nil))
	if response.Code != http.StatusGone {
		t.Fatalf("online snapshot image status=%d body=%q", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "fixture") {
		t.Fatal("online client screenshot was returned by the login image endpoint")
	}
	if item.public.CompletedAt != nil || item.public.State != driver.StateStarting {
		t.Fatalf("one image request completed an unstable login: %#v", item.public)
	}
}

func TestExpiredChallengeRejectsEveryEntryPoint(t *testing.T) {
	driverInstance := &challengeDriver{Driver: fake.New(driver.PlatformWeChat)}
	item := &entry{
		public: Challenge{ID: "expired", ExpiresAt: time.Now().UTC().Add(-time.Second)},
		driver: driverInstance,
	}
	manager := NewManager(config.Paths{})
	manager.entries[item.public.ID] = item
	if _, err := manager.Status(item.public.ID); !os.IsNotExist(err) {
		t.Fatalf("expired challenge status: %v", err)
	}
	if err := manager.SubmitCode(context.Background(), item.public.ID, "123456"); !os.IsNotExist(err) {
		t.Fatalf("expired CLI submission: %v", err)
	}

	getHandlers := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "page", handler: item.handlePage},
		{name: "state", handler: item.handleState},
		{name: "image", handler: item.handleImage},
	}
	for _, test := range getHandlers {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			test.handler(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if response.Code != http.StatusGone {
				t.Fatalf("expired %s status=%d body=%q", test.name, response.Code, response.Body.String())
			}
		})
	}
	response := httptest.NewRecorder()
	submitRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/submit", strings.NewReader(`{"code":"123456"}`))
	submitRequest.Header.Set("Content-Type", "application/json")
	submitRequest.Header.Set("X-WeChatCopilot-Code", "user-entered")
	submitRequest.Header.Set("Origin", "http://127.0.0.1")
	item.handleSubmit(response, submitRequest)
	if response.Code != http.StatusGone {
		t.Fatalf("expired submit status=%d body=%q", response.Code, response.Body.String())
	}
	if got := driverInstance.submissions.Load(); got != 0 {
		t.Fatalf("expired challenge forwarded %d verification codes", got)
	}
	actionRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/action", strings.NewReader(`{"action_id":"accept_privacy_policy","confirmed":true}`))
	actionRequest.Header.Set("Content-Type", "application/json")
	actionRequest.Header.Set("X-WeChatCopilot-Action", "user-confirmed")
	actionRequest.Header.Set("Origin", "http://127.0.0.1")
	actionResponse := httptest.NewRecorder()
	item.handleAction(actionResponse, actionRequest)
	if actionResponse.Code != http.StatusGone {
		t.Fatalf("expired action status=%d body=%q", actionResponse.Code, actionResponse.Body.String())
	}
}

func TestSelectPrivateLANAddressPrefersDefaultRouteInterface(t *testing.T) {
	interfaces := []lanInterface{
		{Name: "eth0", Index: 2, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("10.0.0.8")}},
		{Name: "wlan0", Index: 3, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("192.168.50.9")}},
		{Name: "docker0", Index: 4, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("172.17.0.1")}, Excluded: true},
	}
	selected, err := selectPrivateLANAddress("", interfaces, []string{"docker0", "wlan0", "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if selected != "192.168.50.9" {
		t.Fatalf("selected address = %s, want default-route wlan0 address", selected)
	}
}

func TestSelectPrivateLANAddressFallsBackDeterministically(t *testing.T) {
	interfaces := []lanInterface{
		{Name: "public0", Index: 2, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("203.0.113.9")}},
		{Name: "eth1", Index: 8, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("192.168.1.20")}},
		{Name: "eth0", Index: 3, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("10.20.30.40")}},
	}
	selected, err := selectPrivateLANAddress("", interfaces, []string{"public0"})
	if err != nil {
		t.Fatal(err)
	}
	if selected != "10.20.30.40" {
		t.Fatalf("fallback address = %s, want lowest-index eligible interface", selected)
	}
}

func TestExplicitLANAddressMustBePrivateAssignedAndEligible(t *testing.T) {
	interfaces := []lanInterface{
		{Name: "eth0", Index: 2, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("192.168.1.20")}},
		{Name: "down0", Index: 3, Addresses: []net.IP{net.ParseIP("10.0.0.3")}},
		{Name: "docker0", Index: 4, Flags: net.FlagUp, Addresses: []net.IP{net.ParseIP("172.17.0.1")}, Excluded: true},
	}
	selected, err := selectPrivateLANAddress("192.168.1.20", interfaces, nil)
	if err != nil || selected != "192.168.1.20" {
		t.Fatalf("assigned explicit address = %q err=%v", selected, err)
	}
	for _, invalid := range []string{
		"0.0.0.0",
		"127.0.0.1",
		"8.8.8.8",
		"192.168.1.99",
		"10.0.0.3",
		"172.17.0.1",
		"::1",
	} {
		if selected, err := selectPrivateLANAddress(invalid, interfaces, nil); err == nil {
			t.Fatalf("invalid explicit address %s selected as %s", invalid, selected)
		}
	}
}

func TestDefaultRouteInterfacesOrdersByMetric(t *testing.T) {
	path := filepath.Join(t.TempDir(), "route")
	contents := "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n" +
		"eth0 00000000 0100000A 0003 0 0 200 00000000 0 0 0\n" +
		"wlan0 00000000 0101A8C0 0003 0 0 20 00000000 0 0 0\n" +
		"down0 00000000 00000000 0000 0 0 1 00000000 0 0 0\n" +
		"eth1 0002A8C0 00000000 0001 0 0 1 00FFFFFF 0 0 0\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	routes := defaultRouteInterfaces(path)
	if len(routes) != 2 || routes[0] != "wlan0" || routes[1] != "eth0" {
		t.Fatalf("default routes = %#v", routes)
	}
}

type challengeDriver struct {
	*fake.Driver
	submissions   atomic.Int32
	kind          driver.AuthKind
	state         driver.RuntimeState
	canSubmitCode bool
}

func (d *challengeDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	kind := d.kind
	if kind == "" {
		kind = driver.AuthSMS
	}
	canSubmit := d.canSubmitCode
	if d.kind == "" {
		canSubmit = true
	}
	state := d.state
	if state == "" {
		state = driver.StateAuthRequired
	}
	return driver.AuthSnapshot{
		Kind: kind, State: state, Prompt: "scan",
		QRCodePNG: []byte("fixture"), CanSubmitCode: canSubmit, ObservedAt: time.Now().UTC(),
	}, nil
}

func (d *challengeDriver) SubmitAuthCode(context.Context, string) error {
	d.submissions.Add(1)
	return nil
}

type blockingCodeChallengeDriver struct {
	*fake.Driver
	started     chan struct{}
	release     chan struct{}
	submissions atomic.Int32
}

func (d *blockingCodeChallengeDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	return driver.AuthSnapshot{
		Kind: driver.AuthSMS, State: driver.StateAuthRequired,
		CanSubmitCode: true, ObservedAt: time.Now().UTC(),
	}, nil
}

func (d *blockingCodeChallengeDriver) SubmitAuthCode(context.Context, string) error {
	if d.submissions.Add(1) == 1 {
		close(d.started)
		<-d.release
	}
	return nil
}

type authActionChallengeDriver struct {
	*fake.Driver
	actions atomic.Int32
}

func (d *authActionChallengeDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	return driver.AuthSnapshot{
		Kind: driver.AuthPhoneConfirm, State: driver.StateAuthRequired, Prompt: "review privacy policy",
		ScreenshotPNG: []byte("fixture"), ObservedAt: time.Now().UTC(),
		Actions: []driver.AuthAction{{
			ID: "accept_privacy_policy", Label: "accept", Risk: "high",
			RequiresConfirmation: true,
		}},
	}, nil
}

func (d *authActionChallengeDriver) PerformAuthAction(_ context.Context, request driver.AuthActionRequest) error {
	if request.ActionID != "accept_privacy_policy" || !request.Confirmed {
		return errors.New("unexpected authentication action")
	}
	d.actions.Add(1)
	return nil
}

func performActionRequest(t *testing.T, challengeURL string, confirmed bool) *http.Response {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		challengeURL+"/action",
		strings.NewReader(fmt.Sprintf(`{"action_id":"accept_privacy_policy","confirmed":%t}`, confirmed)),
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(challengeURL)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-WeChatCopilot-Action", "user-confirmed")
	request.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type challengeStatePayload struct {
	Challenge
	CanSubmitCode bool                `json:"can_submit_code"`
	Actions       []driver.AuthAction `json:"actions"`
}

func newAuthActionEntry(instance driver.Driver) *entry {
	return &entry{
		public: Challenge{
			State: driver.StateAuthRequired, ExpiresAt: time.Now().UTC().Add(time.Minute),
		},
		driver: instance, actionAttempts: make(map[string]int), performedActions: make(map[string]bool),
		performedReplayKeys: make(map[string]bool),
	}
}

func readChallengeState(t *testing.T, item *entry) challengeStatePayload {
	t.Helper()
	response := httptest.NewRecorder()
	item.handleState(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/state", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%q", response.Code, response.Body.String())
	}
	var state challengeStatePayload
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v body=%q", err, response.Body.String())
	}
	return state
}

type sequentialAuthActionDriver struct {
	*fake.Driver
	mu    sync.Mutex
	phase int
	calls []string
}

func (d *sequentialAuthActionDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	actions := []driver.AuthAction(nil)
	switch d.phase {
	case 0:
		actions = []driver.AuthAction{{ID: "accept_login_agreements", Label: "accept"}}
	case 1:
		actions = []driver.AuthAction{{ID: "continue_with_wechat", Label: "continue"}}
	}
	return driver.AuthSnapshot{
		State: driver.StateAuthRequired, Kind: driver.AuthPhoneConfirm,
		Actions: actions, ObservedAt: time.Now().UTC(),
	}, nil
}

func (d *sequentialAuthActionDriver) PerformAuthAction(_ context.Context, request driver.AuthActionRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := ""
	switch d.phase {
	case 0:
		want = "accept_login_agreements"
	case 1:
		want = "continue_with_wechat"
	}
	if !request.Confirmed || request.ActionID != want {
		return errors.New("unexpected authentication action")
	}
	d.calls = append(d.calls, request.ActionID)
	d.phase++
	return nil
}

func (d *sequentialAuthActionDriver) actionCalls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

type blockingSequentialAuthActionDriver struct {
	*sequentialAuthActionDriver
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingSequentialAuthActionDriver) PerformAuthAction(ctx context.Context, request driver.AuthActionRequest) error {
	if request.ActionID == "accept_login_agreements" {
		d.once.Do(func() { close(d.started) })
		select {
		case <-d.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.sequentialAuthActionDriver.PerformAuthAction(ctx, request)
}

type fixedAuthActionDriver struct {
	*fake.Driver
	mu         sync.Mutex
	actions    []driver.AuthAction
	screenshot []byte
	results    map[string][]error
	calls      map[string]int
}

type blockingReplayKeyAuthDriver struct {
	*fixedAuthActionDriver
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *blockingReplayKeyAuthDriver) PerformAuthAction(ctx context.Context, request driver.AuthActionRequest) error {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.fixedAuthActionDriver.PerformAuthAction(ctx, request)
}

func (d *fixedAuthActionDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return driver.AuthSnapshot{
		State: driver.StateAuthRequired, Kind: driver.AuthPhoneConfirm,
		Actions:       append([]driver.AuthAction(nil), d.actions...),
		ScreenshotPNG: append([]byte(nil), d.screenshot...), ObservedAt: time.Now().UTC(),
	}, nil
}

func (d *fixedAuthActionDriver) PerformAuthAction(_ context.Context, request driver.AuthActionRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls == nil {
		d.calls = make(map[string]int)
	}
	index := d.calls[request.ActionID]
	d.calls[request.ActionID] = index + 1
	if values := d.results[request.ActionID]; index < len(values) {
		return values[index]
	}
	return nil
}

func (d *fixedAuthActionDriver) callCount(actionID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[actionID]
}

type mutableAuthStateDriver struct {
	*fake.Driver
	mu            sync.Mutex
	snapshot      driver.AuthSnapshot
	snapshotCalls atomic.Int32
}

func (d *mutableAuthStateDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	d.snapshotCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshot, nil
}

func (d *mutableAuthStateDriver) setSnapshot(snapshot driver.AuthSnapshot) {
	d.mu.Lock()
	d.snapshot = snapshot
	d.mu.Unlock()
}

type delayedAuthStateDriver struct {
	*fake.Driver
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *delayedAuthStateDriver) AuthSnapshot(ctx context.Context) (driver.AuthSnapshot, error) {
	d.once.Do(func() { close(d.started) })
	select {
	case <-d.release:
	case <-ctx.Done():
		return driver.AuthSnapshot{}, ctx.Err()
	}
	return driver.AuthSnapshot{
		State: driver.StateAuthRequired, Kind: driver.AuthSMS, CanSubmitCode: true,
		Actions: []driver.AuthAction{{ID: "stale-action", Label: "stale"}}, ObservedAt: time.Now().UTC(),
	}, nil
}
