package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/daemon"
)

func TestSystemdUnitCreatesWritablePrivateRuntimeDirectory(t *testing.T) {
	unit := systemdUnit(
		"/opt/wechatcopilot",
		"/srv/private/wechatcopilot",
		"/home/operator/.config/wechatcopilot/environment",
		"/home/operator/.config/wechatcopilot/swap-policy.environment",
		"",
	)
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

func TestSystemdUnitLoadsRequiredStateMountEnvironmentLast(t *testing.T) {
	general := "/home/operator/.config/wechatcopilot/environment"
	policy := "/home/operator/.config/wechatcopilot/swap-policy.environment"
	required := "/home/operator/.config/wechatcopilot/state-mount.environment"
	unit := systemdUnit("/opt/wechatcopilot", "/srv/wechatcopilot-state", general, policy, required)
	generalLine := "EnvironmentFile=-" + `"` + general + `"`
	requiredLine := "EnvironmentFile=" + `"` + required + `"`
	generalIndex := strings.Index(unit, generalLine)
	requiredIndex := strings.Index(unit, requiredLine)
	execIndex := strings.Index(unit, "ExecStart=")
	if generalIndex < 0 || requiredIndex <= generalIndex || execIndex <= requiredIndex {
		t.Fatalf("state mount environment is not required and loaded last:\n%s", unit)
	}
}

func TestSystemdUnitPinsStrictSwapPolicyAfterGeneralEnvironment(t *testing.T) {
	general := "/home/operator/.config/wechatcopilot/environment"
	policy := "/home/operator/.config/wechatcopilot/swap-policy.environment"
	unit := systemdUnit("/opt/wechatcopilot", "/srv/wechatcopilot-state", general, policy, "")
	generalIndex := strings.Index(unit, "EnvironmentFile=-"+strconv.Quote(general))
	policyLine := "EnvironmentFile=" + strconv.Quote(policy)
	policyIndex := strings.Index(unit, policyLine)
	if generalIndex < 0 || policyIndex <= generalIndex {
		t.Fatalf("strict swap policy is not pinned after the general environment file:\n%s", unit)
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
	systemctlLog := filepath.Join(root, "systemctl.log")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYSTEMCTL_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("SYSTEMCTL_LOG", systemctlLog)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStrictSwap, "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rootCommand := newRoot("test", bytes.NewReader(nil), &stdout, &stderr, func() error { return nil })
	rootCommand.SetArgs([]string{"--home", stateHome, "--json", "daemon", "install"})
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
	swapPolicyPath := filepath.Join(configHome, "wechatcopilot", "swap-policy.environment")
	swapPolicy, err := os.ReadFile(swapPolicyPath)
	if err != nil || string(swapPolicy) != config.EnvStrictSwap+"=false\n" {
		t.Fatalf("installed swap policy = %q, err=%v", swapPolicy, err)
	}
	swapPolicyInfo, err := os.Stat(swapPolicyPath)
	if err != nil || swapPolicyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("installed swap policy mode = %v, err=%v", swapPolicyInfo, err)
	}
	if !strings.Contains(string(contents), "EnvironmentFile="+strconv.Quote(swapPolicyPath)) {
		t.Fatalf("installed unit does not require its pinned swap policy:\n%s", contents)
	}
	systemctlCalls, err := os.ReadFile(systemctlLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"--user daemon-reload",
		"--user enable wechatcopilot.service",
		"--user restart wechatcopilot.service",
	} {
		if !strings.Contains(string(systemctlCalls), expected) {
			t.Fatalf("daemon install did not run %q:\n%s", expected, systemctlCalls)
		}
	}
}

func TestDaemonServeDoesNotCreateFallbackForMissingRequiredMount(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state-must-be-mounted")
	runtimeHome := filepath.Join(root, "runtime")
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStateMountSource, "/dev/mapper/wechatcopilot-state")
	t.Setenv(config.EnvStateMountFSType, "ext4")
	t.Setenv(config.EnvStateMountUUID, "00112233-4455-6677-8899-aabbccddeeff")

	command := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	command.SetArgs([]string{"--home", stateHome, "--json", "daemon", "serve"})
	err := command.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeDaemonUnavailable {
		t.Fatalf("daemon serve error = %v, want %s", err, api.CodeDaemonUnavailable)
	}
	if _, statErr := os.Stat(stateHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon created an unmounted fallback state path: %v", statErr)
	}
}

func TestDaemonServeRejectsUnsafeSwapBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state-must-not-exist")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "")

	command := newRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}, func() error {
		return errors.New("unsafe swap fixture")
	})
	command.SetArgs([]string{"--home", stateHome, "--json", "daemon", "serve"})
	err := command.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeDaemonUnavailable {
		t.Fatalf("daemon serve unsafe swap error = %v, want %s", err, api.CodeDaemonUnavailable)
	}
	if _, statErr := os.Stat(stateHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon created state before rejecting unsafe swap: %v", statErr)
	}
}

func TestDaemonInstallRejectsUnsafeSwapBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state-must-not-exist")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "")

	command := newRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}, func() error {
		return errors.New("unsafe swap fixture")
	})
	command.SetArgs([]string{"--home", stateHome, "--json", "daemon", "install"})
	err := command.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeDaemonUnavailable {
		t.Fatalf("daemon install unsafe swap error = %v, want %s", err, api.CodeDaemonUnavailable)
	}
	if _, statErr := os.Stat(stateHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon install created state before rejecting unsafe swap: %v", statErr)
	}
}

func TestDaemonInstallNoStartDefersSwapValidationToServe(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "true")

	validationCalled := false
	command := newRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}, func() error {
		validationCalled = true
		return errors.New("unsafe swap fixture")
	})
	command.SetArgs([]string{"--home", stateHome, "--json", "daemon", "install", "--no-start"})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("daemon install --no-start: %v", err)
	}
	if validationCalled {
		t.Fatal("daemon install --no-start unexpectedly validated current swap")
	}
	if _, err := os.Stat(stateHome); err != nil {
		t.Fatalf("daemon install --no-start did not create its inspectable state layout: %v", err)
	}
	policyPath := filepath.Join(root, "config", "wechatcopilot", "swap-policy.environment")
	policy, err := os.ReadFile(policyPath)
	if err != nil || string(policy) != config.EnvStrictSwap+"=true\n" {
		t.Fatalf("daemon install --no-start policy = %q, err=%v", policy, err)
	}
}

func TestDaemonInstallRejectsPartialMountGateBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state-must-not-exist")
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStateMountSource, "/dev/mapper/wechatcopilot-state")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "")

	command := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	command.SetArgs([]string{"--home", stateHome, "--json", "daemon", "install", "--no-start"})
	err := command.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeDaemonUnavailable {
		t.Fatalf("daemon install partial mount error = %v, want %s", err, api.CodeDaemonUnavailable)
	}
	if _, statErr := os.Stat(stateHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon install created state for partial mount gate: %v", statErr)
	}
}

func TestDaemonInstallRefusesPersistedMountGateDowngradeBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state-must-not-exist")
	configHome := filepath.Join(root, "config")
	stateMountEnvironment := filepath.Join(configHome, "wechatcopilot", "state-mount.environment")
	if err := os.MkdirAll(filepath.Dir(stateMountEnvironment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateMountEnvironment, []byte("persisted gate fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "")

	command := NewRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})
	command.SetArgs([]string{"--home", stateHome, "--json", "daemon", "install", "--force", "--no-start"})
	err := command.ExecuteContext(context.Background())
	var appErr *api.AppError
	if !errors.As(err, &appErr) || appErr.Code != api.CodeConflict {
		t.Fatalf("daemon install gate downgrade error = %v, want %s", err, api.CodeConflict)
	}
	if _, statErr := os.Stat(stateHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon install created state before refusing gate downgrade: %v", statErr)
	}
}

func TestPersistedMountGateIsDetectedFromUnitWhenEnvironmentFileIsMissing(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	unitPath := filepath.Join(configHome, "systemd", "user", "wechatcopilot.service")
	environmentPath := filepath.Join(configHome, "wechatcopilot", "state-mount.environment")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o700); err != nil {
		t.Fatal(err)
	}
	unit := systemdUnit(
		"/opt/wechatcopilot",
		"/srv/wechatcopilot-state",
		"/tmp/general",
		filepath.Join(configHome, "wechatcopilot", "swap-policy.environment"),
		environmentPath,
	)
	if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	persisted, err := config.HasPersistedStateMountGate()
	if err != nil || !persisted {
		t.Fatalf("persisted gate detection = %v, err=%v", persisted, err)
	}
}

func TestDaemonServeLocksStateAcrossDifferentSockets(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state")
	runtimeHome := filepath.Join(root, "runtime")
	socketOne := filepath.Join(runtimeHome, "wechatcopilot", "one.sock")
	socketTwo := filepath.Join(runtimeHome, "wechatcopilot", "two.sock")
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("WECHATCOPILOT_HOME", "")
	t.Setenv("WECHATCOPILOT_FAKE_DRIVERS", "true")

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	var firstStdout bytes.Buffer
	var firstStderr bytes.Buffer
	first := newRoot("test", bytes.NewReader(nil), &firstStdout, &firstStderr, func() error { return nil })
	first.SetArgs([]string{"--home", stateHome, "--socket", socketOne, "daemon", "serve"})
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.ExecuteContext(firstCtx) }()
	waitForPath(t, socketOne)

	second := newRoot("test", bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}, func() error { return nil })
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
