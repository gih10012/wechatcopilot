package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/driver"
)

func TestStoreIndexesMessagesAndSendResults(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	store, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := driver.Conversation{
		ID: "conversation-1", ExternalID: "external-1", Title: "测试群", Kind: "group",
		Complete: true, Source: "fixture", LastMessageAt: time.Now().UTC(),
	}
	if err := store.UpsertConversations(ctx, []driver.Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	messages, err := store.AddMessages(ctx, []driver.Message{{
		ID: "message-1", ExternalID: "external-message-1", ConversationID: conversation.ID,
		SenderName: "小明", SentAt: time.Now().UTC(), Kind: "text", Text: "项目进度正常",
		Source: "fixture", Complete: true, Confidence: 1, GapBefore: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Sequence < 1 {
		t.Fatalf("unexpected inserted messages: %#v", messages)
	}
	listed, err := store.ListMessages(ctx, driver.MessageQuery{ConversationID: conversation.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Text != "项目进度正常" || listed[0].Raw != nil || !listed[0].GapBefore {
		t.Fatalf("unexpected listed messages: %#v", listed)
	}
	searched, err := store.SearchMessages(ctx, "项目", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(searched) != 1 || searched[0].ID != "message-1" || !searched[0].GapBefore {
		t.Fatalf("unexpected search result: %#v", searched)
	}

	result := driver.SendResult{MessageID: "outgoing-1", Verified: true}
	if err := store.SaveSendResult(ctx, "key-1", "transaction-1", "hash-1", result); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetSendResult(ctx, "key-1", "hash-1")
	if err != nil || stored == nil || stored.MessageID != result.MessageID {
		t.Fatalf("stored idempotency result: %#v err=%v", stored, err)
	}
	key, stored, err := store.GetSendResultForTransaction(ctx, "transaction-1")
	if err != nil || key != "key-1" || stored == nil || stored.MessageID != result.MessageID {
		t.Fatalf("stored transaction result: key=%q result=%#v err=%v", key, stored, err)
	}

	provisional := driver.SendResult{Uncertain: true, Detail: "reserved"}
	if err := store.ReserveSend(ctx, "key-2", "transaction-2", "hash-2", provisional); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetSendResult(ctx, "key-2", "hash-2")
	if err != nil || stored == nil || !stored.Uncertain {
		t.Fatalf("stored send reservation: result=%#v err=%v", stored, err)
	}
	final := driver.SendResult{MessageID: "outgoing-2", Verified: true}
	if err := store.FinalizeSend(ctx, "key-2", "transaction-2", "hash-2", final); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetSendResult(ctx, "key-2", "hash-2")
	if err != nil || stored == nil || stored.MessageID != final.MessageID || !stored.Verified {
		t.Fatalf("finalized send reservation: result=%#v err=%v", stored, err)
	}

	if err := store.ReserveSend(ctx, "key-3", "transaction-3", "hash-3", provisional); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSendReservation(ctx, "key-3", "transaction-3", "hash-3"); err != nil {
		t.Fatal(err)
	}
	stored, err = store.GetSendResult(ctx, "key-3", "hash-3")
	if err != nil || stored != nil {
		t.Fatalf("deleted send reservation: result=%#v err=%v", stored, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("index permissions: info=%v err=%v", info, err)
	}
}

func TestStoreRejectsMessageWithoutConversation(t *testing.T) {
	store, err := Open(indexTestPath(t, "account-1"), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.AddMessages(context.Background(), []driver.Message{{
		ID: "message-1", ExternalID: "external-1", ConversationID: "missing",
		SentAt: time.Now().UTC(), Kind: "text", Source: "fixture", Complete: true, Confidence: 1,
	}})
	if err == nil {
		t.Fatal("AddMessages succeeded without a parent conversation")
	}
}

func TestStorePromotesGapMarkerOnDuplicateMessage(t *testing.T) {
	ctx := context.Background()
	store, err := Open(indexTestPath(t, "account-1"), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := driver.Conversation{
		ID: "conversation-1", ExternalID: "external-1", Title: "One", Kind: "group", Complete: true, Source: "fixture",
	}
	if err := store.UpsertConversations(ctx, []driver.Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	message := driver.Message{
		ID: "message-1", ExternalID: "external-message-1", ConversationID: conversation.ID,
		SentAt: time.Now().UTC(), Kind: "text", Text: "preserved", Source: "fixture", Complete: true, Confidence: 1,
	}
	if inserted, err := store.AddMessages(ctx, []driver.Message{message}); err != nil || len(inserted) != 1 {
		t.Fatalf("initial insert = %#v, err=%v", inserted, err)
	}
	message.GapBefore = true
	if inserted, err := store.AddMessages(ctx, []driver.Message{message}); err != nil || len(inserted) != 0 {
		t.Fatalf("duplicate gap promotion = %#v, err=%v", inserted, err)
	}
	listed, err := store.ListMessages(ctx, driver.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].GapBefore || listed[0].Text != "preserved" {
		t.Fatalf("promoted message = %#v", listed)
	}
}

func TestStorePreservesIndependentConversationGapsWhenFiltered(t *testing.T) {
	ctx := context.Background()
	store, err := Open(indexTestPath(t, "account-1"), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversations := []driver.Conversation{
		{ID: "conversation-a", ExternalID: "external-a", Title: "A", Kind: "group", Source: "fixture"},
		{ID: "conversation-b", ExternalID: "external-b", Title: "B", Kind: "group", Source: "fixture"},
	}
	if err := store.UpsertConversations(ctx, conversations); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	fixtures := []driver.Message{
		{ID: "a-1", ExternalID: "external-a-1", ConversationID: "conversation-a", SentAt: base, Kind: "text", Text: "a-boundary", Source: "fixture", GapBefore: true},
		{ID: "a-2", ExternalID: "external-a-2", ConversationID: "conversation-a", SentAt: base.Add(time.Second), Kind: "text", Text: "a-next", Source: "fixture"},
		{ID: "b-1", ExternalID: "external-b-1", ConversationID: "conversation-b", SentAt: base.Add(2 * time.Second), Kind: "text", Text: "b-boundary", Source: "fixture", GapBefore: true},
		{ID: "b-2", ExternalID: "external-b-2", ConversationID: "conversation-b", SentAt: base.Add(3 * time.Second), Kind: "text", Text: "b-next", Source: "fixture"},
	}
	if _, err := store.AddMessages(ctx, fixtures); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(ctx, driver.MessageQuery{ConversationID: "conversation-b", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Text != "b-boundary" || !messages[0].GapBefore || messages[1].GapBefore {
		t.Fatalf("filtered persisted gaps = %#v", messages)
	}
}

func TestStoreListsLatestMatchingWindowInAscendingSequenceOrder(t *testing.T) {
	ctx := context.Background()
	store, err := Open(indexTestPath(t, "account-1"), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conversations := []driver.Conversation{
		{ID: "conversation-1", ExternalID: "external-1", Title: "One", Kind: "group", Complete: true, Source: "fixture"},
		{ID: "conversation-2", ExternalID: "external-2", Title: "Two", Kind: "group", Complete: true, Source: "fixture"},
	}
	if err := store.UpsertConversations(ctx, conversations); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	fixtures := []driver.Message{
		{ID: "message-1", ExternalID: "external-message-1", ConversationID: "conversation-1", SentAt: base, Kind: "text", Text: "one", Source: "fixture", Complete: true, Confidence: 1},
		{ID: "message-2", ExternalID: "external-message-2", ConversationID: "conversation-1", SentAt: base.Add(time.Minute), Kind: "text", Text: "two", Source: "fixture", Complete: true, Confidence: 1},
		{ID: "message-other", ExternalID: "external-message-other", ConversationID: "conversation-2", SentAt: base.Add(2 * time.Minute), Kind: "text", Text: "other", Source: "fixture", Complete: true, Confidence: 1},
		{ID: "message-3", ExternalID: "external-message-3", ConversationID: "conversation-1", SentAt: base.Add(3 * time.Minute), Kind: "text", Text: "three", Source: "fixture", Complete: true, Confidence: 1},
		{ID: "message-4", ExternalID: "external-message-4", ConversationID: "conversation-1", SentAt: base.Add(4 * time.Minute), Kind: "text", Text: "four", Source: "fixture", Complete: true, Confidence: 1},
	}
	if _, err := store.AddMessages(ctx, fixtures); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListMessages(ctx, driver.MessageQuery{ConversationID: "conversation-1", Latest: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != "message-3" || listed[1].ID != "message-4" {
		t.Fatalf("latest matching window = %#v, want message-3 then message-4", listed)
	}
	if listed[0].Sequence >= listed[1].Sequence {
		t.Fatalf("latest window is not in ascending sequence order: %#v", listed)
	}

	listed, err = store.ListMessages(ctx, driver.MessageQuery{
		ConversationID: "conversation-1", Before: base.Add(3 * time.Minute), Latest: true, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != "message-1" || listed[1].ID != "message-2" {
		t.Fatalf("latest filtered window = %#v, want message-1 then message-2", listed)
	}
}

func TestStoreBindsIndexToAccount(t *testing.T) {
	firstPath := indexTestPath(t, "account-1")
	store, err := Open(firstPath, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	secondPath := indexTestPath(t, "account-2")
	contents, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(secondPath, "account-2"); err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("wrong-account index open error = %v", err)
	}
	reopened, err := Open(firstPath, "account-1")
	if err != nil {
		t.Fatalf("correct account could not reopen its index: %v", err)
	}
	defer reopened.Close()
}

func TestStoreAdoptsRecognizedLegacyIndexOnlyExplicitly(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	legacy, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	conversation := driver.Conversation{
		ID: "conversation-legacy", ExternalID: "external-legacy", Title: "Legacy", Kind: "group", Complete: true, Source: "fixture",
	}
	if err := legacy.UpsertConversations(ctx, []driver.Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.AddMessages(ctx, []driver.Message{{
		ID: "message-legacy", ExternalID: "external-message-legacy", ConversationID: conversation.ID,
		SentAt: time.Now().UTC(), Kind: "text", Text: "preserved", Source: "fixture", Complete: true, Confidence: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, `DROP TABLE wechatcopilot_metadata`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, "account-1"); !errors.Is(err, ErrLegacyIndexUnbound) {
		t.Fatalf("ordinary Open legacy error = %v, want ErrLegacyIndexUnbound", err)
	}

	migrated, err := AdoptLegacy(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	messages, err := migrated.ListMessages(ctx, driver.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "message-legacy" || messages[0].Text != "preserved" {
		t.Fatalf("legacy migration lost messages: %#v", messages)
	}
	if err := migrated.validateMetadata(ctx); err != nil {
		t.Fatalf("legacy migration did not persist ownership metadata: %v", err)
	}
}

func TestStoreRejectsMissingOrSymlinkedAccountPaths(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "account-1")
		path := filepath.Join(parent, "index.sqlite3")
		if _, err := Open(path, "account-1"); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("missing parent error = %v", err)
		}
		if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing parent was recreated: %v", err)
		}
	})

	t.Run("symlinked database", func(t *testing.T) {
		path := indexTestPath(t, "account-1")
		target := filepath.Join(t.TempDir(), "target.sqlite3")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "account-1"); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlinked database error = %v", err)
		}
	})

	t.Run("symlinked parent", func(t *testing.T) {
		root := t.TempDir()
		realParent := filepath.Join(root, "real")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(root, "account-1")
		if err := os.Symlink(realParent, linkedParent); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(linkedParent, "index.sqlite3"), "account-1"); err == nil || !strings.Contains(err.Error(), "without symlinks") {
			t.Fatalf("symlinked parent error = %v", err)
		}
	})
}

func TestStoreAllowsOnlyNewOrEmptyDatabaseWithoutAdoption(t *testing.T) {
	path := indexTestPath(t, "account-1")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, "account-1")
	if err != nil {
		t.Fatalf("open empty database: %v", err)
	}
	defer store.Close()
	if err := store.validateMetadata(context.Background()); err != nil {
		t.Fatalf("empty database was not bound: %v", err)
	}
	if _, err := AdoptLegacy(path, "account-1"); err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("adopt initialized database error = %v", err)
	}
}

func TestAdoptLegacyDoesNotCreateMissingIndex(t *testing.T) {
	path := indexTestPath(t, "account-1")
	if _, err := AdoptLegacy(path, "account-1"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing legacy adoption error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy adoption created a missing index: %v", err)
	}
}

func TestStoreAutomaticallyBindsRecognizedRowEmptyLegacyIndex(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	legacy, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, `DROP TABLE wechatcopilot_metadata`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path, "account-1")
	if err != nil {
		t.Fatalf("recognized row-empty legacy index should bind automatically: %v", err)
	}
	defer migrated.Close()
	if err := migrated.validateMetadata(ctx); err != nil {
		t.Fatalf("row-empty legacy index was not bound: %v", err)
	}
	if _, err := AdoptLegacy(path, "account-1"); err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("explicit adoption after automatic empty migration error = %v", err)
	}
}

func TestStoreMigratesMessageGapColumnAndDefaultsExistingRows(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	store, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	conversation := driver.Conversation{
		ID: "conversation-legacy-gap", ExternalID: "external-legacy-gap", Title: "Legacy", Kind: "group", Complete: true, Source: "fixture",
	}
	if err := store.UpsertConversations(ctx, []driver.Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessages(ctx, []driver.Message{{
		ID: "message-before-gap", ExternalID: "external-before-gap", ConversationID: conversation.ID,
		SentAt: time.Now().UTC(), Kind: "text", Text: "preserved", Source: "fixture", Complete: true, Confidence: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE messages DROP COLUMN gap_before`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path, "account-1")
	if err != nil {
		t.Fatalf("reopen pre-gap schema: %v", err)
	}
	defer migrated.Close()
	messages, err := migrated.ListMessages(ctx, driver.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "message-before-gap" || messages[0].GapBefore {
		t.Fatalf("migrated messages = %#v, want preserved row with gap_before=false", messages)
	}
	columns, err := migrated.tableColumns(ctx, "messages")
	if err != nil {
		t.Fatal(err)
	}
	if columns[len(columns)-1] != "gap_before" {
		t.Fatalf("migrated columns = %#v", columns)
	}
}

func TestStoreRejectsUnknownMessageColumn(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	store, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN unexpected_payload TEXT`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "account-1"); err == nil || !strings.Contains(err.Error(), "unexpected columns") {
		t.Fatalf("unknown message column error = %v", err)
	}
}

func TestStoreNeverAutomaticallyBindsLegacySchemaWithExtraObjects(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
	}{
		{name: "table", statement: `CREATE TABLE hidden_payload(value TEXT)`},
		{name: "view", statement: `CREATE VIEW hidden_view AS SELECT id FROM conversations`},
		{name: "trigger", statement: `CREATE TRIGGER hidden_trigger AFTER INSERT ON conversations BEGIN SELECT 1; END`},
		{name: "index", statement: `CREATE INDEX hidden_index ON conversations(title)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := indexTestPath(t, "account-1")
			legacy, err := Open(path, "account-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.db.ExecContext(ctx, `DROP TABLE wechatcopilot_metadata`); err != nil {
				t.Fatal(err)
			}
			if _, err := legacy.db.ExecContext(ctx, test.statement); err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := Open(path, "account-1"); !errors.Is(err, ErrLegacyIndexUnbound) {
				t.Fatalf("ordinary Open extra %s error = %v", test.name, err)
			}
			if _, err := AdoptLegacy(path, "account-1"); err == nil || !strings.Contains(err.Error(), "unexpected schema object") {
				t.Fatalf("explicit adoption extra %s error = %v", test.name, err)
			}
		})
	}
}

func TestStoreRejectsUnrecognizedLegacySchema(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	store, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE wechatcopilot_metadata`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE conversations`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "account-1"); !errors.Is(err, ErrLegacyIndexUnbound) {
		t.Fatalf("ordinary Open error = %v", err)
	}
	if _, err := AdoptLegacy(path, "account-1"); err == nil || !strings.Contains(err.Error(), "recognized") {
		t.Fatalf("unrecognized legacy adoption error = %v", err)
	}
}

func TestStoreVerifiesMetadataOnEveryOpen(t *testing.T) {
	ctx := context.Background()
	path := indexTestPath(t, "account-1")
	store, err := Open(path, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE wechatcopilot_metadata SET schema_version=999`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "account-1"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("tampered metadata error = %v", err)
	}
}

func TestStoreRejectsInodeAndDirectoryExchangeDuringSQLiteOpen(t *testing.T) {
	t.Run("index inode", func(t *testing.T) {
		path := indexTestPath(t, "account-1")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := func() error {
			store, openErr := open(path, "account-1", false, func() error {
				if err := os.Rename(path, path+".original"); err != nil {
					return err
				}
				return os.WriteFile(path, nil, 0o600)
			})
			if store != nil {
				_ = store.Close()
			}
			return openErr
		}()
		if err == nil || (!strings.Contains(err.Error(), "pinned") && !strings.Contains(err.Error(), "inode changed")) {
			t.Fatalf("exchanged index error = %v", err)
		}
	})

	t.Run("account directory", func(t *testing.T) {
		path := indexTestPath(t, "account-1")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		accountDir := filepath.Dir(path)
		err := func() error {
			store, openErr := open(path, "account-1", false, func() error {
				if err := os.Rename(accountDir, accountDir+".original"); err != nil {
					return err
				}
				if err := os.Mkdir(accountDir, 0o700); err != nil {
					return err
				}
				return os.WriteFile(path, nil, 0o600)
			})
			if store != nil {
				_ = store.Close()
			}
			return openErr
		}()
		if err == nil || (!strings.Contains(err.Error(), "directory changed") && !strings.Contains(err.Error(), "pinned")) {
			t.Fatalf("exchanged directory error = %v", err)
		}
	})
}

func TestStoreRejectsLinkedOrUnsafeSQLiteFiles(t *testing.T) {
	t.Run("hard-linked index", func(t *testing.T) {
		path := indexTestPath(t, "account-1")
		target := filepath.Join(t.TempDir(), "target.sqlite3")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "account-1"); err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("hard-linked index error = %v", err)
		}
	})

	t.Run("symlinked WAL", func(t *testing.T) {
		path := indexTestPath(t, "account-1")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "wal"), path+"-wal"); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "account-1"); err == nil || !strings.Contains(err.Error(), "sidecar") {
			t.Fatalf("symlinked WAL error = %v", err)
		}
	})
}

func indexTestPath(t *testing.T, accountID string) string {
	t.Helper()
	accountDir := filepath.Join(t.TempDir(), accountID)
	if err := os.Mkdir(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(accountDir, indexFilename)
}
