package config

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainsLineHandlesProcFilesystemsFields(t *testing.T) {
	contents := []byte("nodev\tsysfs\nnodev\tbinder\next4\n")
	if !containsLine(contents, "binder") {
		t.Fatal("binder filesystem was not detected")
	}
	if containsLine(contents, "bind") {
		t.Fatal("partial filesystem name matched")
	}
}

func TestSwapConfidentialityAcceptsOnlyProtectedSwap(t *testing.T) {
	header := "Filename Type Size Used Priority\n"
	for _, test := range []struct {
		name     string
		contents string
		ok       bool
	}{
		{name: "none", contents: header, ok: true},
		{name: "zram", contents: header + "/dev/zram0 partition 1048572 0 100\n", ok: true},
		{name: "raw partition", contents: header + "/dev/sda2 partition 5242876 1 -2\n", ok: false},
		{name: "swap file", contents: header + "/swapfile file 1048572 0 -2\n", ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := swapConfidentialityCheckFrom(
				[]byte(test.contents),
				func(path string) bool { return path == "/dev/zram0" },
				func(string) bool { return false },
			)
			if check.Name != "swap_confidentiality" || check.OK != test.ok {
				t.Fatalf("swap check = %#v, want ok=%v", check, test.ok)
			}
		})
	}
	encrypted := swapConfidentialityCheckFrom(
		[]byte(header+"/dev/mapper/cryptswap partition 1048572 0 -2\n"),
		func(string) bool { return false },
		func(path string) bool { return path == "/dev/mapper/cryptswap" },
	)
	if !encrypted.OK {
		t.Fatalf("dm-crypt swap check = %#v", encrypted)
	}
	spoofedZRAM := swapConfidentialityCheckFrom(
		[]byte(header+"/dev/zram-spoof file 1048572 0 -2\n"),
		func(string) bool { return false },
		func(string) bool { return false },
	)
	if spoofedZRAM.OK {
		t.Fatalf("zram-like path bypassed the block-device check: %#v", spoofedZRAM)
	}
}

func TestSwapConfidentialityPolicyDefaultsToWarning(t *testing.T) {
	unsafe := Check{
		Name: "swap_confidentiality", OK: false,
		Detail: "one unprotected swap target", Fix: "replace it",
	}
	warning := applySwapPolicy(unsafe, false)
	if !warning.OK || !warning.Warning || !strings.Contains(warning.Detail, "warning:") {
		t.Fatalf("default swap policy = %#v, want a non-blocking warning", warning)
	}
	strict := applySwapPolicy(unsafe, true)
	if strict.OK || strict.Warning {
		t.Fatalf("strict swap policy = %#v, want a blocking failure", strict)
	}
	safe := applySwapPolicy(Check{Name: "swap_confidentiality", OK: true}, false)
	if !safe.OK || safe.Warning {
		t.Fatalf("safe swap policy = %#v, want a clean pass", safe)
	}
}

func TestSwapConfidentialityEnforcementMatrix(t *testing.T) {
	unsafe := Check{Name: "swap_confidentiality", OK: false, Detail: "unsafe swap fixture"}
	if err := enforceSwapPolicy(unsafe, false); err != nil {
		t.Fatalf("default policy blocked unsafe swap: %v", err)
	}
	if err := enforceSwapPolicy(unsafe, true); err == nil || err.Error() != unsafe.Detail {
		t.Fatalf("strict policy error = %v, want %q", err, unsafe.Detail)
	}
	if err := enforceSwapPolicy(Check{Name: "swap_confidentiality", OK: true}, true); err != nil {
		t.Fatalf("strict policy blocked protected swap: %v", err)
	}
}

func TestSwapWarningJSONIsNonBlockingAndExplicit(t *testing.T) {
	warning := applySwapPolicy(Check{
		Name: "swap_confidentiality", OK: false, Detail: "unsafe swap fixture",
	}, false)
	payload, err := json.Marshal(struct {
		OK     bool    `json:"ok"`
		Checks []Check `json:"checks"`
	}{OK: warning.OK, Checks: []Check{warning}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	if !strings.Contains(text, `"ok":true`) || !strings.Contains(text, `"warning":true`) {
		t.Fatalf("warning JSON = %s, want non-blocking ok and explicit warning", text)
	}
}

func TestStrictSwapPolicyEnvironment(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "", want: false},
		{value: "false", want: false},
		{value: "true", want: true},
		{value: "1", want: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(EnvStrictSwap, test.value)
			got, err := StrictSwapPolicy()
			if err != nil || got != test.want {
				t.Fatalf("strictSwapPolicy() = %v, %v, want %v, nil", got, err, test.want)
			}
		})
	}
	t.Setenv(EnvStrictSwap, "sometimes")
	if _, err := StrictSwapPolicy(); err == nil {
		t.Fatal("invalid strict swap policy was accepted")
	}
}

func TestZRAMWritebackDisabled(t *testing.T) {
	sysfsDevicePath := "/sys/devices/virtual/block/zram0"
	backingDevPath := filepath.Join(sysfsDevicePath, "backing_dev")
	readError := errors.New("permission denied")
	for _, test := range []struct {
		name     string
		contents string
		err      error
		want     bool
	}{
		{name: "unsupported", err: os.ErrNotExist, want: true},
		{name: "disabled", contents: "none\n", want: true},
		{name: "raw backing device", contents: "/dev/nvme0n1p1\n", want: false},
		{name: "read error", err: readError, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := zramWritebackDisabled(sysfsDevicePath, func(path string) ([]byte, error) {
				if path != backingDevPath {
					t.Fatalf("read path = %q, want %q", path, backingDevPath)
				}
				return []byte(test.contents), test.err
			})
			if got != test.want {
				t.Fatalf("zramWritebackDisabled() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestFindMountInfoRequiresExactDecodedMountPoint(t *testing.T) {
	contents := strings.Join([]string{
		`31 20 0:42 / /srv/private rw,relatime - ext4 /dev/mapper/other rw`,
		`32 20 253:7 / /srv/private\040state rw,nosuid,nodev - ext4 /dev/mapper/wechatcopilot-state rw`,
		`33 20 253:8 / /srv/private\040state rw,nosuid,nodev - ext4 /dev/mapper/stacked-overmount rw`,
	}, "\n")
	entry, err := findMountInfo(strings.NewReader(contents), "/srv/private state", 32)
	if err != nil {
		t.Fatal(err)
	}
	if entry.MountID != 32 || entry.Major != 253 || entry.Minor != 7 || entry.Root != "/" || entry.FSType != "ext4" || entry.Source != "/dev/mapper/wechatcopilot-state" {
		t.Fatalf("mount entry = %#v", entry)
	}
	if !mountOption(entry.Options, "rw") || mountOption(entry.Options, "ro") {
		t.Fatalf("mount options = %q", entry.Options)
	}
	if _, err := findMountInfo(strings.NewReader(contents), "/srv/private"); err != nil {
		t.Fatalf("exact sibling mount was not found: %v", err)
	}
	if _, err := findMountInfo(strings.NewReader(contents), "/srv"); err == nil {
		t.Fatal("non-mount ancestor was accepted")
	}
}

func TestRequiredStateMountConfigurationIsAllOrNothing(t *testing.T) {
	t.Setenv(EnvStateMountSource, "/dev/mapper/wechatcopilot-state")
	t.Setenv(EnvStateMountFSType, "")
	t.Setenv(EnvStateMountUUID, "")
	if _, required, err := stateMountRequirementFromEnv(); !required || err == nil {
		t.Fatalf("partial mount requirement: required=%v err=%v", required, err)
	}

	t.Setenv(EnvStateMountFSType, "EXT4")
	t.Setenv(EnvStateMountUUID, "not-a-uuid")
	if _, _, err := stateMountRequirementFromEnv(); err == nil {
		t.Fatal("invalid filesystem UUID was accepted")
	}

	t.Setenv(EnvStateMountUUID, "AABBCCDD-4455-6677-8899-AABBCCDDEEFF")
	requirement, required, err := stateMountRequirementFromEnv()
	if err != nil || !required || requirement.FSType != "ext4" || requirement.UUID != "aabbccdd-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("valid mount requirement = %#v required=%v err=%v", requirement, required, err)
	}
	environment, err := RequiredStateMountEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`WECHATCOPILOT_STATE_MOUNT_SOURCE="/dev/mapper/wechatcopilot-state"`,
		`WECHATCOPILOT_STATE_MOUNT_FSTYPE="ext4"`,
		`WECHATCOPILOT_STATE_MOUNT_UUID="aabbccdd-4455-6677-8899-aabbccddeeff"`,
	}
	if strings.Join(environment, "\n") != strings.Join(want, "\n") {
		t.Fatalf("state mount environment = %#v, want %#v", environment, want)
	}
}

func TestAcquireRequiredStateMountIsNilWhenDisabled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(EnvStateMountSource, "")
	t.Setenv(EnvStateMountFSType, "")
	t.Setenv(EnvStateMountUUID, "")
	guard, err := AcquireRequiredStateMount(filepath.Join(t.TempDir(), "missing"))
	if err != nil || guard != nil {
		t.Fatalf("disabled mount guard = %#v err=%v", guard, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("close nil mount guard: %v", err)
	}
	if err := (&RequiredStateMountGuard{}).Close(); err != nil {
		t.Fatalf("close zero-value mount guard: %v", err)
	}
}

func TestPersistedStateMountGateFailsClosedWhenShellEnvironmentIsMissing(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	persistedEnvironment := filepath.Join(configHome, "wechatcopilot", "state-mount.environment")
	if err := os.MkdirAll(filepath.Dir(persistedEnvironment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persistedEnvironment, []byte("persisted gate fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv(EnvStateMountSource, "")
	t.Setenv(EnvStateMountFSType, "")
	t.Setenv(EnvStateMountUUID, "")
	stateHome := filepath.Join(root, "state-must-not-exist")
	guard, err := AcquireRequiredStateMount(stateHome)
	if guard != nil || err == nil || !strings.Contains(err.Error(), "persisted state mount gate") {
		t.Fatalf("persisted mount guard = %#v err=%v", guard, err)
	}
	checks := Doctor(Paths{
		Home: stateHome, Accounts: filepath.Join(stateHome, "accounts"), Downloads: filepath.Join(stateHome, "downloads"),
		Runtime: filepath.Join(root, "runtime"), Socket: filepath.Join(root, "runtime", "daemon.sock"), Registry: filepath.Join(stateHome, "accounts.json"),
	}, false)
	if _, statErr := os.Stat(stateHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("doctor created fallback state despite persisted gate: %v", statErr)
	}
	byName := map[string]Check{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if byName["state_mount"].OK || !strings.Contains(byName["state_mount"].Detail, "persisted state mount gate") {
		t.Fatalf("persisted state mount check = %#v", byName["state_mount"])
	}
}

func TestAcquireRequiredStateMountRejectsSpecialModeBits(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateHome, 0o700|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvStateMountSource, "/dev/mapper/wechatcopilot-state")
	t.Setenv(EnvStateMountFSType, "ext4")
	t.Setenv(EnvStateMountUUID, "00112233-4455-6677-8899-aabbccddeeff")
	guard, err := AcquireRequiredStateMount(stateHome)
	if guard != nil || err == nil || !strings.Contains(err.Error(), "no special bits") {
		t.Fatalf("special-mode mount guard = %#v err=%v", guard, err)
	}
}

func TestDoctorDoesNotCreateFallbackStateWhenRequiredMountIsMissing(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "state-must-be-mounted")
	runtimeDir := filepath.Join(home, "misconfigured-runtime")
	t.Setenv(EnvStateMountSource, "/dev/mapper/wechatcopilot-state")
	t.Setenv(EnvStateMountFSType, "ext4")
	t.Setenv(EnvStateMountUUID, "00112233-4455-6677-8899-aabbccddeeff")

	checks := Doctor(Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}, false)
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created an unmounted fallback state path: %v", err)
	}
	byName := map[string]Check{}
	for _, check := range checks {
		byName[check.Name] = check
	}
	if byName["state_mount"].OK || !strings.Contains(byName["state_mount"].Detail, "without creating it") {
		t.Fatalf("state mount check = %#v", byName["state_mount"])
	}
	if byName["state_permissions"].OK {
		t.Fatalf("state permissions check = %#v", byName["state_permissions"])
	}
	if byName["runtime_permissions"].OK {
		t.Fatalf("runtime permissions check = %#v", byName["runtime_permissions"])
	}
}

func TestSecureDirRejectsSymlinksAndRepairsMode(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "state")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureDir(directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("secureDir mode = %04o, want 0700", info.Mode().Perm())
	}

	link := filepath.Join(root, "state-link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if err := secureDir(link); err == nil || !strings.Contains(err.Error(), "without symlinks") {
		t.Fatalf("secureDir symlink error = %v", err)
	}
}

func TestSecureDirRejectsSymlinkedAndUnsafeAncestors(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if err := secureDir(filepath.Join(linkedParent, "state")); err == nil {
		t.Fatal("secureDir accepted a symlinked ancestor")
	}

	unsafeParent := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := secureDir(filepath.Join(unsafeParent, "state")); err == nil || !strings.Contains(err.Error(), "unsafe ownership or permissions") {
		t.Fatalf("secureDir unsafe ancestor error = %v", err)
	}
}

func TestDaemonSocketCheckRequiresPrivateUnixSocket(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "daemon.sock")
	if check := daemonSocketCheck(socket); !check.OK || !strings.Contains(check.Detail, "stopped") {
		t.Fatalf("missing socket check = %#v", check)
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	unixListener := listener.(*net.UnixListener)
	unixListener.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	if check := daemonSocketCheck(socket); !check.OK {
		t.Fatalf("private socket check = %#v", check)
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		t.Fatal(err)
	}
	if check := daemonSocketCheck(socket); check.OK || !strings.Contains(check.Detail, "0600") {
		t.Fatalf("shared socket check = %#v", check)
	}
}

func TestDaemonSocketCheckRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "daemon.sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if check := daemonSocketCheck(link); check.OK || !strings.Contains(check.Detail, "non-symlink") {
		t.Fatalf("symlink socket check = %#v", check)
	}
}

func TestValidateWeChatAppImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultWeChatAppImageName)
	contents := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("fixture-appimage")...)
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWeChatAppImage(path, digest); err != nil {
		t.Fatal(err)
	}
	if err := validateWeChatAppImage(path, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatched AppImage digest error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateWeChatAppImage(path, digest); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("non-executable AppImage error = %v", err)
	}
}

func TestValidateWeChatAppImageRejectsSymlinkAndNonELF(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("not-elf"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWeChatAppImage(target, digest); err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("non-ELF AppImage error = %v", err)
	}
	link := filepath.Join(root, DefaultWeChatAppImageName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := validateWeChatAppImage(link, digest); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink AppImage error = %v", err)
	}
}

func TestValidateWeComAPKs(t *testing.T) {
	root := t.TempDir()
	official := filepath.Join(root, DefaultWeComAPKName)
	companion := filepath.Join(root, DefaultWeComCompanionAPKName)
	writeTestAPK(t, official, "AndroidManifest.xml")
	writeTestAPK(t, companion, "AndroidManifest.xml", "classes.dex")
	officialDigest, err := fileSHA256(official)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAPK(official, officialDigest, false); err != nil {
		t.Fatal(err)
	}
	if err := validateAPK(official, "", false); err == nil || !strings.Contains(err.Error(), "64 hexadecimal") {
		t.Fatalf("missing official APK digest error = %v", err)
	}
	if err := validateAPK(companion, "", true); err != nil {
		t.Fatal(err)
	}
	if check := distinctArtifactsCheck(official, companion); !check.OK {
		t.Fatalf("distinct APK check = %#v", check)
	}
}

func TestValidateCompanionAPKRequiresClassesAndDistinctFile(t *testing.T) {
	root := t.TempDir()
	official := filepath.Join(root, "official.apk")
	missingClasses := filepath.Join(root, "companion.apk")
	writeTestAPK(t, official, "AndroidManifest.xml")
	writeTestAPK(t, missingClasses, "AndroidManifest.xml")
	if err := validateAPK(missingClasses, "", true); err == nil || !strings.Contains(err.Error(), "classes.dex") {
		t.Fatalf("companion without classes error = %v", err)
	}
	hardlink := filepath.Join(root, "companion-hardlink.apk")
	if err := os.Link(official, hardlink); err != nil {
		t.Fatal(err)
	}
	if check := distinctArtifactsCheck(official, hardlink); check.OK {
		t.Fatalf("hard-linked APKs were accepted: %#v", check)
	}
}

func TestPinnedImagePatternRequiresLowercaseDigest(t *testing.T) {
	valid := "registry.example/redroid:13@sha256:" + strings.Repeat("a", 64)
	if !pinnedImagePattern.MatchString(valid) {
		t.Fatalf("valid pinned image did not match: %s", valid)
	}
	for _, invalid := range []string{
		"registry.example/redroid:13",
		"registry.example/redroid:13@sha256:" + strings.Repeat("A", 64),
		"registry.example/redroid:13@sha256:short",
	} {
		if pinnedImagePattern.MatchString(invalid) {
			t.Fatalf("invalid pinned image matched: %s", invalid)
		}
	}
}

func TestDoctorWithoutRuntimeChecksStillChecksSocket(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	paths := Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}
	checks := Doctor(paths, false)
	foundSocket := false
	foundSwap := false
	for _, check := range checks {
		if check.Name == "daemon_socket" {
			foundSocket = true
		}
		if check.Name == "swap_confidentiality" {
			foundSwap = true
		}
		if check.Name == "wechat_appimage" || check.Name == "wecom_apk" {
			t.Fatalf("runtime artifact check %q ran with runtimeChecks=false", check.Name)
		}
	}
	if !foundSocket {
		t.Fatal("daemon socket was not checked")
	}
	if !foundSwap {
		t.Fatal("swap confidentiality was skipped with runtimeChecks=false")
	}
}

func TestDoctorChecksLocalRuntimeArtifactsWithoutADB(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	paths := Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	appImage := filepath.Join(paths.Downloads, DefaultWeChatAppImageName)
	if err := os.WriteFile(appImage, append([]byte{0x7f, 'E', 'L', 'F'}, []byte("fixture")...), 0o700); err != nil {
		t.Fatal(err)
	}
	appImageDigest, err := fileSHA256(appImage)
	if err != nil {
		t.Fatal(err)
	}
	official := filepath.Join(paths.Downloads, DefaultWeComAPKName)
	companion := filepath.Join(paths.Downloads, DefaultWeComCompanionAPKName)
	writeTestAPK(t, official, "AndroidManifest.xml")
	writeTestAPK(t, companion, "AndroidManifest.xml", "classes.dex")
	officialDigest, err := fileSHA256(official)
	if err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(root, "docker-fixture")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ncase \"$1\" in\n  info) echo 26.0.0 ;;\n  image) echo sha256:fixture ;;\n  *) exit 1 ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvDocker, docker)
	t.Setenv(EnvWeChatAppImage, appImage)
	t.Setenv(EnvWeChatAppImageSHA256, appImageDigest)
	t.Setenv(EnvWeChatImage, "wechatcopilot/wechat-runtime:test")
	t.Setenv(EnvWeComRedroidImage, "registry.example/redroid:13@sha256:"+strings.Repeat("a", 64))
	t.Setenv(EnvWeComAPK, official)
	t.Setenv(EnvWeComAPKSHA256, officialDigest)
	t.Setenv(EnvWeComCompanionAPK, companion)

	checks := Doctor(paths, true)
	byName := make(map[string]Check, len(checks))
	for _, check := range checks {
		byName[check.Name] = check
	}
	for _, name := range []string{"docker", "docker_access", "wechat_appimage", "wechat_image", "wecom_redroid_image", "wecom_apk", "wecom_companion_apk", "wecom_apk_separation"} {
		if check, ok := byName[name]; !ok || !check.OK {
			t.Fatalf("runtime check %q = %#v", name, check)
		}
	}
	if _, exists := byName["adb"]; exists {
		t.Fatalf("doctor still requires host ADB: %#v", byName["adb"])
	}
}

func writeTestAPK(t *testing.T, path string, entries ...string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	var writeErr error
	for _, name := range entries {
		entry, err := archive.Create(name)
		if err == nil {
			_, err = entry.Write([]byte("fixture"))
		}
		writeErr = errors.Join(writeErr, err)
	}
	writeErr = errors.Join(writeErr, archive.Close(), file.Close())
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}
