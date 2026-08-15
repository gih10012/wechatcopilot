package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
	"github.com/gih10012/wechatcopilot/internal/service"
)

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
