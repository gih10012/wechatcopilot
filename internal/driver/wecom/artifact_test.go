package wecom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureOfficialAPKAcceptsMatchingExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wecom.apk")
	contents := []byte("PK\x03\x04synthetic APK fixture")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	if err := (ArtifactManager{}).EnsureOfficialAPK(context.Background(), "", path, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("matching file rejected: %v", err)
	}
}

func TestEnsureOfficialAPKRejectsMismatchedExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wecom.apk")
	if err := os.WriteFile(path, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte("expected"))
	if err := (ArtifactManager{}).EnsureOfficialAPK(context.Background(), "", path, hex.EncodeToString(expected[:])); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestSnapshotAPKRejectsSymlinkSource(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.apk")
	link := filepath.Join(dir, "link.apk")
	if err := os.WriteFile(target, []byte("PK\x03\x04synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotAPK(link, dir, ""); err == nil {
		t.Fatal("expected APK symlink to be rejected")
	}
}
