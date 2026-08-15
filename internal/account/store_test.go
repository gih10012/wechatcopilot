package account

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
)

func TestDeletingAccountPersistsAndFailsClosed(t *testing.T) {
	paths := testPaths(t)
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Add("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	marked, err := store.BeginDelete(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !marked.Deleting || marked.Active || marked.State != driver.StateStopped {
		t.Fatalf("unexpected deletion marker: %#v", marked)
	}
	repeated, err := store.BeginDelete(item.Alias)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != item.ID || !repeated.Deleting {
		t.Fatalf("repeated BeginDelete changed the account: %#v", repeated)
	}

	reopened, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Deleting || persisted.Active || persisted.State != driver.StateStopped {
		t.Fatalf("deletion marker did not survive reopen: %#v", persisted)
	}
	if _, _, err := reopened.Activate(item.ID); !errors.Is(err, ErrDeleting) {
		t.Fatalf("Activate error = %v, want ErrDeleting", err)
	}
	if _, err := reopened.Deactivate(item.ID); !errors.Is(err, ErrDeleting) {
		t.Fatalf("Deactivate error = %v, want ErrDeleting", err)
	}
	if err := reopened.UpdateStatus(item.ID, driver.Status{State: driver.StateOnline}); !errors.Is(err, ErrDeleting) {
		t.Fatalf("UpdateStatus error = %v, want ErrDeleting", err)
	}

	deleteFailure := errors.New("fixture purge failed")
	if err := reopened.RecordDeleteFailure(item.ID, deleteFailure); err != nil {
		t.Fatal(err)
	}
	afterFailure, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := afterFailure.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.LastError != deleteFailure.Error() || !failed.Deleting {
		t.Fatalf("delete failure was not persisted: %#v", failed)
	}
}

func TestOpenNormalizesDeletingAccountToStopped(t *testing.T) {
	paths := testPaths(t)
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Add("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginDelete(item.ID); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	store.data.Accounts[0].Active = true
	store.data.Accounts[0].State = driver.StateOnline
	if err := store.saveLocked(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()

	reopened, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := reopened.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Active || normalized.State != driver.StateStopped || !normalized.Deleting {
		t.Fatalf("deleting account was not normalized: %#v", normalized)
	}
	reopenedAgain, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopenedAgain.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Active || persisted.State != driver.StateStopped {
		t.Fatalf("normalization was not persisted: %#v", persisted)
	}
}

func TestFinalizeDeleteRemovesManagedStateAndRegistryEntry(t *testing.T) {
	paths := testPaths(t)
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Add("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginDelete(item.ID); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(paths.Accounts, item.ID)
	runtimeDir := filepath.Join(paths.Runtime, "accounts", item.ID)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "session"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "pid"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := store.FinalizeDelete(item.Alias)
	if err != nil {
		t.Fatal(err)
	}
	if removed.ID != item.ID || !removed.Deleting {
		t.Fatalf("unexpected removed account: %#v", removed)
	}
	for _, path := range []string{stateDir, runtimeDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("managed path %s still exists: %v", path, err)
		}
	}
	if _, err := store.Resolve(item.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed account Resolve error = %v, want os.ErrNotExist", err)
	}
	reopened, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Resolve(item.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registry removal did not persist: %v", err)
	}
}

func TestDeleteTransitionGuards(t *testing.T) {
	store, err := Open(testPaths(t))
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Add("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeDelete(item.ID); !errors.Is(err, ErrNotDeleting) {
		t.Fatalf("FinalizeDelete error = %v, want ErrNotDeleting", err)
	}
	if _, _, err := store.Activate(item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginDelete(item.ID); err == nil {
		t.Fatal("BeginDelete accepted an active account")
	}
}

func TestAddRollsBackMemoryAndDirectoryWhenRegistryWriteFails(t *testing.T) {
	paths := testPaths(t)
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	store.writeFile = failingRegistryWriter
	if _, err := store.Add("personal", driver.PlatformWeChat); err == nil {
		t.Fatal("Add succeeded with a failing registry writer")
	}
	if accounts := store.List(); len(accounts) != 0 {
		t.Fatalf("failed Add remained in memory: %#v", accounts)
	}
	entries, err := os.ReadDir(paths.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed Add left account directories: %#v", entries)
	}
	reopened, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	if accounts := reopened.List(); len(accounts) != 0 {
		t.Fatalf("failed Add reached disk: %#v", accounts)
	}
}

func TestAccountMutationsUseCopyOnWrite(t *testing.T) {
	paths := testPaths(t)
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Add("second", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Activate(first.ID); err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(operation string, before []Account, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s succeeded with a failing registry writer", operation)
		}
		if after := store.List(); !reflect.DeepEqual(after, before) {
			t.Fatalf("%s changed in-memory registry after write failure:\nbefore=%#v\nafter=%#v", operation, before, after)
		}
	}
	store.writeFile = failingRegistryWriter
	before := store.List()
	_, _, err = store.Activate(second.ID)
	assertUnchanged("Activate", before, err)
	_, err = store.Deactivate(first.ID)
	assertUnchanged("Deactivate", before, err)
	err = store.UpdateStatus(first.ID, driver.Status{
		State: driver.StateOnline, Identity: &driver.Identity{DisplayName: "changed"}, ClientVersion: "fixture",
	})
	assertUnchanged("UpdateStatus", before, err)
	_, err = store.BeginDelete(second.ID)
	assertUnchanged("BeginDelete", before, err)

	store.writeFile = config.AtomicWrite
	if _, err := store.BeginDelete(second.ID); err != nil {
		t.Fatal(err)
	}
	deleting := store.List()
	store.writeFile = failingRegistryWriter
	err = store.RecordDeleteFailure(second.ID, errors.New("fixture failure"))
	assertUnchanged("RecordDeleteFailure", deleting, err)
	_, err = store.FinalizeDelete(second.ID)
	assertUnchanged("FinalizeDelete", deleting, err)

	reopened, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Resolve(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Deleting {
		t.Fatalf("durable deletion marker was lost: %#v", persisted)
	}
}

func TestReturnedIdentityCannotMutateStore(t *testing.T) {
	store, err := Open(testPaths(t))
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Add("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	identity := &driver.Identity{PlatformID: "wxid-fixture", DisplayName: "Original"}
	if err := store.UpdateStatus(item.ID, driver.Status{State: driver.StateOnline, Identity: identity}); err != nil {
		t.Fatal(err)
	}
	identity.DisplayName = "Caller mutation"
	resolved, err := store.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Identity == nil || resolved.Identity.DisplayName != "Original" {
		t.Fatalf("caller-owned identity mutated the store: %#v", resolved.Identity)
	}
	resolved.Identity.DisplayName = "Result mutation"
	again, err := store.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Identity.DisplayName != "Original" {
		t.Fatalf("resolved identity alias mutated the store: %#v", again.Identity)
	}
}

func TestOpenRejectsAccountIDAliasCollisions(t *testing.T) {
	const (
		firstID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		secondID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	for _, test := range []struct {
		name     string
		accounts []Account
	}{
		{
			name: "alias matches previous ID",
			accounts: []Account{
				fixtureAccount(firstID, "first"),
				fixtureAccount(secondID, firstID),
			},
		},
		{
			name: "ID matches previous alias",
			accounts: []Account{
				fixtureAccount(firstID, secondID),
				fixtureAccount(secondID, "second"),
			},
		},
		{
			name:     "same account ID and alias",
			accounts: []Account{fixtureAccount(firstID, firstID)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := testPaths(t)
			if err := paths.Ensure(); err != nil {
				t.Fatal(err)
			}
			writeRegistry(t, paths, test.accounts)
			if _, err := Open(paths); err == nil || !strings.Contains(err.Error(), "alias") {
				t.Fatalf("Open collision error = %v", err)
			}
		})
	}
}

func TestAddKeepsAliasesAndIDsInDisjointNamespaces(t *testing.T) {
	const (
		aliasID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		existingID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		uniqueID   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	)
	paths := testPaths(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeRegistry(t, paths, []Account{fixtureAccount(existingID, aliasID)})
	store, err := Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(existingID, driver.PlatformWeCom); err == nil || !strings.Contains(err.Error(), "existing account ID") {
		t.Fatalf("Add alias matching ID error = %v", err)
	}

	generated := []string{existingID, aliasID, uniqueID}
	store.newAccountID = func() string {
		id := generated[0]
		generated = generated[1:]
		return id
	}
	added, err := store.Add("work", driver.PlatformWeCom)
	if err != nil {
		t.Fatal(err)
	}
	if added.ID != uniqueID {
		t.Fatalf("allocated ID = %s, want %s", added.ID, uniqueID)
	}
}

func fixtureAccount(id, alias string) Account {
	now := time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC)
	return Account{ID: id, Alias: alias, Platform: driver.PlatformWeChat, State: driver.StateStopped, CreatedAt: now, UpdatedAt: now}
}

func writeRegistry(t *testing.T, paths config.Paths, accounts []Account) {
	t.Helper()
	contents, err := json.MarshalIndent(registry{Version: 1, Accounts: accounts}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.AtomicWrite(paths.Registry, append(contents, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func failingRegistryWriter(string, []byte, os.FileMode) error {
	return errors.New("fixture registry write failed")
}

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	return config.Paths{
		Home:      home,
		Accounts:  filepath.Join(home, "accounts"),
		Downloads: filepath.Join(home, "downloads"),
		Runtime:   runtimeDir,
		Socket:    filepath.Join(runtimeDir, "daemon.sock"),
		Registry:  filepath.Join(home, "accounts.json"),
	}
}
