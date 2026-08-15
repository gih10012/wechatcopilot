package wechat

import (
	"os"
	"path/filepath"
	"testing"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestProfileManagerPersistsIsolatedIdentity(t *testing.T) {
	temporary := t.TempDir()
	protected := filepath.Join(temporary, "operator", ".xwechat")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(protected, "untouched")
	if err := os.WriteFile(marker, []byte("operator data"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := ProfileManager{ProtectedPaths: []string{protected}}
	account := shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main account",
		StateDir:   filepath.Join(temporary, "state", "wx-main"),
		RuntimeDir: filepath.Join(temporary, "runtime", "wx-main"),
	}

	first, err := manager.Ensure(account)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Ensure(account)
	if err != nil {
		t.Fatal(err)
	}
	if first.MachineID == "" || first.MachineID != second.MachineID {
		t.Fatalf("machine identity was not persisted: %q != %q", first.MachineID, second.MachineID)
	}
	if first.Hostname != second.Hostname {
		t.Fatalf("hostname changed across starts: %q != %q", first.Hostname, second.Hostname)
	}
	for _, path := range []string{first.Root, first.ClientHome, first.Files, first.Runtime} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %o, want 700", path, info.Mode().Perm())
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "operator data" {
		t.Fatalf("operator profile was touched: data=%q err=%v", data, err)
	}
}

func TestProfileManagerRejectsProtectedClientTree(t *testing.T) {
	temporary := t.TempDir()
	protected := filepath.Join(temporary, ".xwechat")
	manager := ProfileManager{ProtectedPaths: []string{protected}}
	_, err := manager.Ensure(shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main",
		StateDir:   filepath.Join(protected, "copilot"),
		RuntimeDir: filepath.Join(temporary, "runtime"),
	})
	if err == nil {
		t.Fatal("expected protected path to be rejected")
	}
	if _, statErr := os.Stat(protected); !os.IsNotExist(statErr) {
		t.Fatalf("protected path was unexpectedly created: %v", statErr)
	}
}
