package auth

import (
	"context"
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
	if !strings.Contains(string(body), `src="`+parsed.Path+`/image"`) {
		t.Fatalf("login image route is not challenge-scoped: %s", body)
	}
	if info, err := os.Stat(challenge.LinkQRPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("link QR permissions: info=%v err=%v", info, err)
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
	manager.CloseAll()
	if _, err := manager.Status(challenge.ID); !os.IsNotExist(err) {
		t.Fatalf("closed challenge status: %v", err)
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
	item.handleSubmit(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"code":"123456"}`)))
	if response.Code != http.StatusGone {
		t.Fatalf("expired submit status=%d body=%q", response.Code, response.Body.String())
	}
	if got := driverInstance.submissions.Load(); got != 0 {
		t.Fatalf("expired challenge forwarded %d verification codes", got)
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
	submissions atomic.Int32
}

func (d *challengeDriver) AuthSnapshot(context.Context) (driver.AuthSnapshot, error) {
	return driver.AuthSnapshot{
		Kind: driver.AuthQR, State: driver.StateAuthRequired, Prompt: "scan",
		QRCodePNG: []byte("fixture"), CanSubmitCode: true, ObservedAt: time.Now().UTC(),
	}, nil
}

func (d *challengeDriver) SubmitAuthCode(context.Context, string) error {
	d.submissions.Add(1)
	return nil
}
