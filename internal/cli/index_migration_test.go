package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/daemon"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/index"
)

type legacyIndexCLIFixture struct {
	paths     config.Paths
	account   account.Account
	indexPath string
}

func TestAccountsAdoptLegacyIndexRequiresConfirmationBeforeStateAccess(t *testing.T) {
	root := t.TempDir()
	missingHome := filepath.Join(root, "must-not-be-created")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv(config.EnvHome, "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")

	_, err := runLegacyIndexCLI(t, missingHome, "accounts", "adopt-legacy-index", "--account", "personal")
	assertCLIError(t, err, api.CodeInvalidArgument, "--confirm is required for legacy message index adoption")
	if _, statErr := os.Lstat(missingHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unconfirmed migration touched state home: %v", statErr)
	}
}

func TestAccountsAdoptLegacyIndexRefusesToInitializeMissingState(t *testing.T) {
	root := t.TempDir()
	missingHome := filepath.Join(root, "must-not-be-created")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv(config.EnvHome, "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")

	_, err := runLegacyIndexCLI(t, missingHome,
		"accounts", "adopt-legacy-index", "--account", "personal", "--confirm",
	)
	assertCLIError(t, err, api.CodeConflict, "registered state is unavailable for legacy index adoption")
	if _, statErr := os.Lstat(missingHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("confirmed migration initialized missing state: %v", statErr)
	}
}

func TestAccountsAdoptLegacyIndexMigratesNonEmptyDatabaseByAlias(t *testing.T) {
	fixture := newLegacyIndexCLIFixture(t)
	stdout, err := runLegacyIndexCLI(t, fixture.paths.Home,
		"accounts", "adopt-legacy-index", "--account", fixture.account.Alias, "--confirm",
	)
	if err != nil {
		t.Fatalf("adopt legacy index: %v, stdout=%s", err, stdout)
	}
	var envelope struct {
		OK   bool                      `json:"ok"`
		Data legacyIndexAdoptionResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode migration output: %v: %s", err, stdout)
	}
	if !envelope.OK || !envelope.Data.Adopted || envelope.Data.AccountID != fixture.account.ID ||
		envelope.Data.AccountAlias != fixture.account.Alias || envelope.Data.Platform != fixture.account.Platform {
		t.Fatalf("unexpected migration result: %#v", envelope)
	}
	if strings.Contains(stdout, "preserved legacy body") || strings.Contains(stdout, fixture.indexPath) {
		t.Fatalf("migration output exposed indexed content or a local path: %s", stdout)
	}

	store, err := index.Open(fixture.indexPath, fixture.account.ID)
	if err != nil {
		t.Fatalf("reopen adopted index: %v", err)
	}
	defer store.Close()
	messages, err := store.ListMessages(context.Background(), driver.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "message-legacy" || messages[0].Text != "preserved legacy body" {
		t.Fatalf("migration did not preserve the legacy row: %#v", messages)
	}
}

func TestAccountsAdoptLegacyIndexAcceptsExactAccountID(t *testing.T) {
	fixture := newLegacyIndexCLIFixture(t)
	stdout, err := runLegacyIndexCLI(t, fixture.paths.Home,
		"accounts", "adopt-legacy-index", "--account", fixture.account.ID, "--confirm",
	)
	if err != nil {
		t.Fatalf("adopt legacy index by ID: %v, stdout=%s", err, stdout)
	}
	if !strings.Contains(stdout, `"account_id":"`+fixture.account.ID+`"`) {
		t.Fatalf("migration did not resolve the exact account ID: %s", stdout)
	}
}

func TestAccountsAdoptLegacyIndexRequiresDaemonStateLock(t *testing.T) {
	fixture := newLegacyIndexCLIFixture(t)
	lock, err := daemon.AcquireStateLock(fixture.paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	_, err = runLegacyIndexCLI(t, fixture.paths.Home,
		"accounts", "adopt-legacy-index", "--account", fixture.account.ID, "--confirm",
	)
	assertCLIError(t, err, api.CodeConflict, "stop the daemon before adopting a legacy message index")
	if !errors.Is(err, daemon.ErrStateLocked) {
		t.Fatalf("migration lock error = %v, want ErrStateLocked", err)
	}
	if legacyIndexHasMetadata(t, fixture.indexPath) {
		t.Fatal("migration changed the index while the daemon state lock was held")
	}
}

func TestAccountsAdoptLegacyIndexRejectsUnknownAccountBeforeSelectingPath(t *testing.T) {
	fixture := newLegacyIndexCLIFixture(t)
	outside := filepath.Join(fixture.paths.Home, "outside")
	_, err := runLegacyIndexCLI(t, fixture.paths.Home,
		"accounts", "adopt-legacy-index", "--account", "../outside", "--confirm",
	)
	assertCLIError(t, err, api.CodeNotFound, "account not found")
	if _, statErr := os.Lstat(outside); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unknown account selected an unregistered path: %v", statErr)
	}
	if legacyIndexHasMetadata(t, fixture.indexPath) {
		t.Fatal("wrong-account migration changed the registered account's index")
	}
}

func TestAccountsAdoptLegacyIndexNeverCreatesMissingRegisteredState(t *testing.T) {
	t.Run("account directory", func(t *testing.T) {
		fixture := newLegacyIndexCLIFixture(t)
		accountDir := filepath.Dir(fixture.indexPath)
		backup := accountDir + ".saved"
		if err := os.Rename(accountDir, backup); err != nil {
			t.Fatal(err)
		}

		_, err := runLegacyIndexCLI(t, fixture.paths.Home,
			"accounts", "adopt-legacy-index", "--account", fixture.account.ID, "--confirm",
		)
		assertCLIError(t, err, api.CodeConflict, "legacy message index cannot be adopted")
		if _, statErr := os.Lstat(accountDir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("migration recreated a missing account directory: %v", statErr)
		}
	})

	t.Run("index file", func(t *testing.T) {
		fixture := newLegacyIndexCLIFixture(t)
		if err := os.Remove(fixture.indexPath); err != nil {
			t.Fatal(err)
		}

		_, err := runLegacyIndexCLI(t, fixture.paths.Home,
			"accounts", "adopt-legacy-index", "--account", fixture.account.ID, "--confirm",
		)
		assertCLIError(t, err, api.CodeConflict, "legacy message index cannot be adopted")
		if _, statErr := os.Lstat(fixture.indexPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("migration recreated a missing index: %v", statErr)
		}
	})

	t.Run("symlinked account directory", func(t *testing.T) {
		fixture := newLegacyIndexCLIFixture(t)
		accountDir := filepath.Dir(fixture.indexPath)
		backup := accountDir + ".saved"
		if err := os.Rename(accountDir, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(backup, accountDir); err != nil {
			t.Fatal(err)
		}

		_, err := runLegacyIndexCLI(t, fixture.paths.Home,
			"accounts", "adopt-legacy-index", "--account", fixture.account.ID, "--confirm",
		)
		assertCLIError(t, err, api.CodeConflict, "legacy message index cannot be adopted")
		if legacyIndexHasMetadata(t, filepath.Join(backup, "index.sqlite3")) {
			t.Fatal("migration followed a symlinked account directory")
		}
	})
}

func newLegacyIndexCLIFixture(t *testing.T) legacyIndexCLIFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime-root"))
	t.Setenv(config.EnvHome, "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "")

	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	paths, err = paths.WithHome(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	registry, err := account.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	item, err := registry.Add("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(paths.Accounts, item.ID, "index.sqlite3")
	store, err := index.Open(indexPath, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	conversation := driver.Conversation{
		ID: "conversation-legacy", ExternalID: "external-legacy", Title: "Legacy", Kind: "group",
		Complete: true, Source: "fixture",
	}
	if err := store.UpsertConversations(context.Background(), []driver.Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessages(context.Background(), []driver.Message{{
		ID: "message-legacy", ExternalID: "external-message-legacy", ConversationID: conversation.ID,
		SentAt: time.Now().UTC(), Kind: "text", Text: "preserved legacy body", Source: "fixture", Complete: true, Confidence: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`DROP TABLE wechatcopilot_metadata`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if legacyIndexHasMetadata(t, indexPath) {
		t.Fatal("legacy fixture still has ownership metadata")
	}
	return legacyIndexCLIFixture{paths: paths, account: item, indexPath: indexPath}
}

func runLegacyIndexCLI(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := newRoot("test", bytes.NewReader(nil), &stdout, &stderr, func() error { return nil })
	command.SetArgs(append([]string{"--home", home, "--json"}, args...))
	err := command.ExecuteContext(context.Background())
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func assertCLIError(t *testing.T, err error, code, message string) {
	t.Helper()
	var appErr *api.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("CLI error = %v, want AppError", err)
	}
	if appErr.Code != code || appErr.Message != message {
		t.Fatalf("CLI error = %#v, want code=%s message=%q", appErr, code, message)
	}
}

func legacyIndexHasMetadata(t *testing.T, path string) bool {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='wechatcopilot_metadata'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count != 0
}
