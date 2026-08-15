package config

import (
	"archive/zip"
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
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	paths := Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}
	checks := Doctor(paths, false)
	foundSocket := false
	for _, check := range checks {
		if check.Name == "daemon_socket" {
			foundSocket = true
		}
		if check.Name == "wechat_appimage" || check.Name == "wecom_apk" {
			t.Fatalf("runtime artifact check %q ran with runtimeChecks=false", check.Name)
		}
	}
	if !foundSocket {
		t.Fatal("daemon socket was not checked")
	}
}

func TestDoctorChecksLocalRuntimeArtifactsWithoutADB(t *testing.T) {
	root := t.TempDir()
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
