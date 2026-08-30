package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
	"github.com/gih10012/wechatcopilot/internal/service"
)

func TestRestoreRetryRecoversAfterDependencyBecomesReady(t *testing.T) {
	var attempts int
	var waits []time.Duration
	var reports []int
	temporary := errors.New("temporary Docker outage")
	err := runRestoreRetry(context.Background(), restoreRetryPolicy{
		maxAttempts: 5, initialBackoff: 10 * time.Millisecond, maxBackoff: 15 * time.Millisecond,
		wait: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
	}, func(context.Context) []error {
		attempts++
		if attempts < 3 {
			return []error{temporary}
		}
		return nil
	}, func(attempt, maximum int, err error) {
		if maximum != 5 || !errors.Is(err, temporary) {
			t.Fatalf("restore report = attempt %d/%d error %v", attempt, maximum, err)
		}
		reports = append(reports, attempt)
	})
	if err != nil {
		t.Fatalf("late dependency recovery failed: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("restore attempts = %d, want 3", attempts)
	}
	wantWaits := []time.Duration{10 * time.Millisecond, 15 * time.Millisecond}
	if len(waits) != len(wantWaits) {
		t.Fatalf("restore waits = %v, want %v", waits, wantWaits)
	}
	for i := range waits {
		if waits[i] != wantWaits[i] {
			t.Fatalf("restore waits = %v, want %v", waits, wantWaits)
		}
	}
	if len(reports) != 2 || reports[0] != 1 || reports[1] != 2 {
		t.Fatalf("restore reports = %v, want [1 2]", reports)
	}
}

func TestRestoreRetryStopsAtPermanentFailureLimit(t *testing.T) {
	var attempts int
	var waits int
	permanent := errors.New("Docker remains unavailable")
	err := runRestoreRetry(context.Background(), restoreRetryPolicy{
		maxAttempts: 4, initialBackoff: time.Millisecond, maxBackoff: 2 * time.Millisecond,
		wait: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	}, func(context.Context) []error {
		attempts++
		return []error{permanent}
	}, nil)
	if !errors.Is(err, permanent) {
		t.Fatalf("permanent restore error = %v", err)
	}
	if attempts != 4 || waits != 3 {
		t.Fatalf("permanent recovery attempts=%d waits=%d, want 4 and 3", attempts, waits)
	}
}

func TestRestoreRetryCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	var attempts atomic.Int32
	go func() {
		result <- runRestoreRetry(ctx, restoreRetryPolicy{
			maxAttempts: 10, initialBackoff: time.Hour, maxBackoff: time.Hour,
		}, func(context.Context) []error {
			if attempts.Add(1) == 1 {
				close(started)
			}
			return []error{errors.New("not ready")}
		}, nil)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("restore attempt did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled restore error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restore backoff ignored cancellation")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("restore attempted %d times after cancellation", got)
	}
}

func TestStopRestoreRecoveryWaitsForCanceledAttempt(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	server := &Server{
		restore: func(ctx context.Context) []error {
			once.Do(func() { close(started) })
			<-ctx.Done()
			close(exited)
			return []error{ctx.Err()}
		},
		recoveryPolicy: restoreRetryPolicy{maxAttempts: 3, initialBackoff: time.Hour, maxBackoff: time.Hour},
	}
	server.startRestoreRecovery()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("restore attempt did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.stopRestoreRecovery(ctx); err != nil {
		t.Fatalf("stop restore recovery: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("recovery stop returned before the active restore attempt exited")
	}
}

func TestShutdownDoesNotCloseServiceAcrossStuckRestore(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	var closeCalls atomic.Int32
	server := &Server{
		httpServer: &http.Server{},
		restore: func(ctx context.Context) []error {
			close(started)
			<-release
			close(exited)
			return []error{ctx.Err()}
		},
		closeService: func(context.Context) error {
			closeCalls.Add(1)
			return nil
		},
		recoveryPolicy: restoreRetryPolicy{maxAttempts: 3, initialBackoff: time.Hour, maxBackoff: time.Hour},
	}
	server.startRestoreRecovery()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("restore attempt did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	if calls := closeCalls.Load(); calls != 0 {
		t.Fatalf("service closed %d times while restore was still running", calls)
	}
	close(release)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("canceled restore attempt did not exit")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry shutdown after restore exit: %v", err)
	}
	if calls := closeCalls.Load(); calls != 1 {
		t.Fatalf("service close calls = %d, want 1", calls)
	}
}

func TestRestoreRetryRejectsZeroBackoff(t *testing.T) {
	var attempts atomic.Int32
	err := runRestoreRetry(context.Background(), restoreRetryPolicy{maxAttempts: 2}, func(context.Context) []error {
		attempts.Add(1)
		return []error{errors.New("unavailable")}
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "positive initial backoff") {
		t.Fatalf("zero-backoff retry error = %v", err)
	}
	if attempts.Load() != 0 {
		t.Fatal("invalid zero-backoff policy entered a retry loop")
	}
}

func TestSurfaceActInvalidArgumentIsHTTPBadRequest(t *testing.T) {
	root := t.TempDir()
	paths := config.Paths{
		Home: filepath.Join(root, "state"), Accounts: filepath.Join(root, "state", "accounts"),
		Downloads: filepath.Join(root, "state", "downloads"), Runtime: filepath.Join(root, "runtime"),
		Socket: filepath.Join(root, "runtime", "daemon.sock"), Registry: filepath.Join(root, "state", "accounts.json"),
	}
	control, err := service.New(paths, map[driver.Platform]driver.Factory{
		driver.PlatformWeChat: func(driver.AccountRuntime) (driver.Driver, error) {
			return &invalidArgumentSurfaceDriver{Driver: fake.New(driver.PlatformWeChat)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	account, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Activate(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}

	server := New(paths.Socket, control)
	request := httptest.NewRequest(http.MethodPost, "/v1/surfaces/act", bytes.NewBufferString(
		`{"account":"personal","id":"surface-1","action_id":"input"}`,
	))
	response := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("surface act status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	var envelope api.Response
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != api.CodeInvalidArgument {
		t.Fatalf("surface act response = %#v, want INVALID_ARGUMENT", envelope)
	}
}

type invalidArgumentSurfaceDriver struct {
	*fake.Driver
}

func (d *invalidArgumentSurfaceDriver) ActSurface(context.Context, string, driver.SurfaceAction) (driver.Surface, error) {
	return driver.Surface{}, driver.NewFailure(driver.FailureInvalidArgument, "fixture rejected surface action arguments")
}

func TestSurfaceActRequestPreservesExplicitEmptyText(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		textProvided bool
		text         string
	}{
		{name: "absent", body: `{"account":"personal","id":"surface-1","action_id":"read"}`},
		{name: "explicit empty", body: `{"account":"personal","id":"surface-1","action_id":"input","text":""}`, textProvided: true},
		{name: "non-empty", body: `{"account":"personal","id":"surface-1","action_id":"input","text":"宿舍"}`, textProvided: true, text: "宿舍"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request surfaceActRequest
			if err := json.Unmarshal([]byte(test.body), &request); err != nil {
				t.Fatal(err)
			}
			action := request.action()
			if action.TextProvided != test.textProvided || action.Text != test.text {
				t.Fatalf("surface action = %#v, want TextProvided=%v Text=%q", action, test.textProvided, test.text)
			}
		})
	}
}

func TestListenNeverDeletesRegularFile(t *testing.T) {
	control, socket := testService(t)
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	contents := []byte("operator data")
	if err := os.WriteFile(socket, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(socket, control)
	if err := server.Listen(); err == nil {
		t.Fatal("Listen replaced a regular file")
	}
	got, err := os.ReadFile(socket)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("regular socket-path file changed: contents=%q err=%v", got, err)
	}
}

func TestListenRejectsUnsafeSocketDirectory(t *testing.T) {
	control, _ := testService(t)
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shared, "daemon.sock")
	if err := New(socket, control).Listen(); err == nil {
		t.Fatal("Listen accepted a socket directory accessible by other users")
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Listen created a socket in an unsafe directory: %v", err)
	}
}

func TestListenRejectsSymlinkSocketDirectory(t *testing.T) {
	control, _ := testService(t)
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked-runtime")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := New(filepath.Join(link, "daemon.sock"), control).Listen(); err == nil {
		t.Fatal("Listen accepted a symlink socket directory")
	}
	if _, err := os.Lstat(filepath.Join(target, "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Listen created a socket through a symlink directory: %v", err)
	}
}

func TestSameUIDListenerAcceptsCurrentUser(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	guarded := &sameUIDListener{UnixListener: listener, uid: uint32(os.Geteuid())}
	accepted := make(chan error, 1)
	go func() {
		connection, err := guarded.Accept()
		if err == nil {
			err = connection.Close()
		}
		accepted <- err
	}()
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("current-UID connection was rejected: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("current-UID connection was not accepted")
	}
}

func TestListenReplacesOwnedStaleUnixSocket(t *testing.T) {
	control, socket := testService(t)
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	server := New(socket, control)
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen did not replace an owned stale socket: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned daemon socket remains after shutdown: %v", err)
	}
}

func TestShutdownPreservesReplacementAtSocketPath(t *testing.T) {
	control, socket := testService(t)
	server := New(socket, control)
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	waitForDaemonSocket(t, socket)
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	contents := []byte("replacement")
	if err := os.WriteFile(socket, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(context.Background()); err == nil {
		t.Fatal("Shutdown did not report the changed socket path")
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(socket)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("Shutdown removed socket-path replacement: contents=%q err=%v", got, err)
	}
}

func testService(t *testing.T) (*service.Service, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	paths := config.Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}
	control, err := service.New(paths, map[driver.Platform]driver.Factory{
		driver.PlatformWeChat: func(driver.AccountRuntime) (driver.Driver, error) { return fake.New(driver.PlatformWeChat), nil },
		driver.PlatformWeCom:  func(driver.AccountRuntime) (driver.Driver, error) { return fake.New(driver.PlatformWeCom), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return control, paths.Socket
}

func waitForDaemonSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("unix", socket, 25*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("daemon socket did not accept connections")
}
