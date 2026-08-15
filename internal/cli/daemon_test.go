package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/daemon"
)

func TestSystemdUnitCreatesWritablePrivateRuntimeDirectory(t *testing.T) {
	unit := systemdUnit("/opt/wechatcopilot", "/srv/private/wechatcopilot", "/home/operator/.config/wechatcopilot/environment")
	for _, expected := range []string{
		"RuntimeDirectory=wechatcopilot",
		"RuntimeDirectoryMode=0700",
		"ReadWritePaths=\"/srv/private/wechatcopilot\"",
		"ReadWritePaths=%t/wechatcopilot",
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("systemd unit is missing %q:\n%s", expected, unit)
		}
	}
}

func TestLANAddressFlagRequiresLAN(t *testing.T) {
	root := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{
		"--json", "accounts", "login", "--account", "fixture", "--lan-address", "192.168.1.20",
	})
	err := root.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeInvalidArgument {
		t.Fatalf("login without --lan error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestSurfaceOpenAcceptsOnlySurfaceReference(t *testing.T) {
	root := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	command, _, err := root.Find([]string{"surfaces", "open"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := command.Flags().Lookup("ref"); flag == nil || flag.Hidden {
		t.Fatal("--ref must be the documented surface-reference flag")
	}
	if flag := command.Flags().Lookup("message"); flag != nil {
		t.Fatal("--message must not alias --ref because message IDs are not surface references")
	}
}

func TestMessagesHistoryLatestRejectsNonzeroCursor(t *testing.T) {
	root := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{
		"--json", "messages", "history", "--account", "fixture", "--latest", "--cursor", "42",
	})
	err := root.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeInvalidArgument {
		t.Fatalf("messages history --latest --cursor error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestDaemonInstallInitializesCustomStateHome(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "new-state")
	configHome := filepath.Join(root, "config")
	runtimeHome := filepath.Join(root, "runtime")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	systemctl := filepath.Join(binDir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("WECHATCOPILOT_HOME", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rootCommand := NewRoot("test", bytes.NewReader(nil), &stdout, &stderr)
	rootCommand.SetArgs([]string{"--home", stateHome, "--json", "daemon", "install", "--no-start"})
	if err := rootCommand.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("daemon install: %v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
	for _, path := range []string{
		stateHome,
		filepath.Join(stateHome, "accounts"),
		filepath.Join(stateHome, "downloads"),
		filepath.Join(runtimeHome, "wechatcopilot"),
	} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("daemon install did not initialize private directory %s: info=%v err=%v", path, info, err)
		}
	}
	unitPath := filepath.Join(configHome, "systemd", "user", "wechatcopilot.service")
	contents, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "ReadWritePaths=%t/wechatcopilot") {
		t.Fatalf("installed unit does not allow the runtime socket directory:\n%s", contents)
	}
}

func TestDaemonServeLocksStateAcrossDifferentSockets(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state")
	runtimeHome := filepath.Join(root, "runtime")
	socketOne := filepath.Join(runtimeHome, "wechatcopilot", "one.sock")
	socketTwo := filepath.Join(runtimeHome, "wechatcopilot", "two.sock")
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv("WECHATCOPILOT_FAKE_DRIVERS", "true")

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	var firstStdout bytes.Buffer
	var firstStderr bytes.Buffer
	first := NewRoot("test", bytes.NewReader(nil), &firstStdout, &firstStderr)
	first.SetArgs([]string{"--home", stateHome, "--socket", socketOne, "daemon", "serve"})
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.ExecuteContext(firstCtx) }()
	waitForPath(t, socketOne)

	second := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	second.SetArgs([]string{"--home", stateHome, "--socket", socketTwo, "daemon", "serve"})
	err := second.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeConflict || !errors.Is(err, daemon.ErrStateLocked) {
		t.Fatalf("second daemon error = %v, want state-lock conflict", err)
	}
	if _, err := os.Lstat(socketTwo); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second daemon reached socket acquisition: %v", err)
	}

	cancelFirst()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first daemon shutdown: %v stderr=%s", err, firstStderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first daemon did not shut down")
	}
	lock, err := daemon.AcquireStateLock(stateHome)
	if err != nil {
		t.Fatalf("daemon shutdown did not release state lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("path was not created: %s", path)
}
