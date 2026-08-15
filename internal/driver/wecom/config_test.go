package wecom

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func validTestConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	official := filepath.Join(dir, "wecom.apk")
	companion := filepath.Join(dir, "companion.apk")
	contents := []byte("PK\x03\x04synthetic APK fixture")
	if err := os.WriteFile(official, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(companion, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	config := DefaultConfig()
	config.RedroidImage = "registry.example/redroid@sha256:" + hex.EncodeToString(sum[:])
	config.OfficialAPKPath = official
	config.OfficialAPKSHA256 = hex.EncodeToString(sum[:])
	config.CompanionAPKPath = companion
	return config
}

func TestConfigRequiresPinnedImage(t *testing.T) {
	config := validTestConfig(t)
	config.RedroidImage = "redroid/redroid:13.0.0-latest"
	if err := config.Validate(); err == nil {
		t.Fatal("expected an unpinned image to be rejected")
	}
}

func TestAccountDataDirRejectsTraversal(t *testing.T) {
	if _, err := accountDataDir(t.TempDir(), "../other"); err == nil {
		t.Fatal("expected traversal-like account ID to be rejected")
	}
}

func TestAllowedAPKHosts(t *testing.T) {
	for _, host := range []string{"work.weixin.qq.com", "dldir1.qq.com", "download.work.weixin.qq.com"} {
		if !allowedAPKHost(host) {
			t.Fatalf("expected host %q to be allowed", host)
		}
	}
	for _, host := range []string{"example.com", "work.weixin.qq.com.example.com", "evilqpic.cn"} {
		if allowedAPKHost(host) {
			t.Fatalf("expected host %q to be rejected", host)
		}
	}
}
