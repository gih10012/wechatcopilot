package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateLockRejectsSecondDaemonRegardlessOfSocket(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireStateLock(home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireStateLock(home)
	if !errors.Is(err, ErrStateLocked) {
		t.Fatalf("second daemon lock error = %v, want ErrStateLocked", err)
	}
	if second != nil {
		t.Fatal("second daemon unexpectedly acquired the state lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := AcquireStateLock(home)
	if err != nil {
		t.Fatalf("lock was not released at shutdown: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStateLockRefusesSymlink(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(home, ".daemon.lock")); err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireStateLock(home); err == nil || lock != nil {
		t.Fatal("state lock followed a symlink")
	}
	contents, err := os.ReadFile(victim)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("state lock changed symlink target: contents=%q err=%v", contents, err)
	}
}
