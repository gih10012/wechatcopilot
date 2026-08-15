package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/driver"
)

func TestStoreIndexesMessagesAndSendResults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "account", "index.sqlite3")
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
		Source: "fixture", Complete: true, Confidence: 1,
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
	if len(listed) != 1 || listed[0].Text != "项目进度正常" || listed[0].Raw != nil {
		t.Fatalf("unexpected listed messages: %#v", listed)
	}
	searched, err := store.SearchMessages(ctx, "项目", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(searched) != 1 || searched[0].ID != "message-1" {
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
	store, err := Open(filepath.Join(t.TempDir(), "index.sqlite3"), "account-1")
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

func TestStoreListsLatestMatchingWindowInAscendingSequenceOrder(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "index.sqlite3"), "account-1")
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
