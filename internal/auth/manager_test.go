package auth

import (
	"context"
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
		`screen.replaceWith(candidate)`,
		`codeButton.disabled=true`,
		`codeButton.disabled=!state.can_submit_code`,
		`if(response.ok){form.hidden=true}`,
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
