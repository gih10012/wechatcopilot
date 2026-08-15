package wechat

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestUIIndexMarksVisibleDataIncompleteAndStable(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline},
		visibleConversations: []VisibleConversation{
			{Title: "Exact Project Group", Kind: "visible", Unread: 2, Locator: "locator-1"},
		},
		visibleMessages: []VisibleMessage{
			{Text: "incoming fixture", Kind: "text", Confidence: 0.45},
			{Text: "https://example.invalid/article", Kind: "link", Outgoing: true,
				AccessibleLabel: "Example article", SurfaceKind: "web", Confidence: 0.55},
		},
		selectedTitle: "Exact Project Group", selectedLocator: "locator-1",
	}
	clock := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	index := NewUIIndex(backend, "wx-main", func() time.Time { return clock })
	conversations, err := index.ListConversations(context.Background(), shared.ConversationQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 {
		t.Fatalf("visible conversations = %d, want 1", len(conversations))
	}
	conversation := conversations[0]
	if conversation.ID == "" || conversation.Complete || conversation.Source != "ui" || conversation.UnreadCount != 2 {
		t.Fatalf("unsafe conversation metadata: %+v", conversation)
	}
	messages, err := index.ReadMessages(context.Background(), shared.MessageQuery{ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("visible messages = %d, want 2", len(messages))
	}
	for _, message := range messages {
		if message.ID == "" || message.ExternalID == "" || message.Complete || message.Source != "ui" {
			t.Fatalf("unsafe UI message metadata: %+v", message)
		}
		if message.Confidence <= 0 || message.Confidence >= 1 {
			t.Fatalf("UI confidence = %v, want between 0 and 1", message.Confidence)
		}
	}
	firstIDs := []string{messages[0].ID, messages[1].ID}
	messagesAgain, err := index.ReadMessages(context.Background(), shared.MessageQuery{ConversationID: conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	if messagesAgain[0].ID != firstIDs[0] || messagesAgain[1].ID != firstIDs[1] {
		t.Fatalf("visible message IDs changed: first=%v second=%v", firstIDs, []string{messagesAgain[0].ID, messagesAgain[1].ID})
	}
	if messages[1].SurfaceRef == "" {
		t.Fatal("visible link did not expose a bounded surface reference")
	}
	target, err := index.ResolveSurface(context.Background(), messages[1].SurfaceRef)
	if err != nil {
		t.Fatal(err)
	}
	if target.ConversationTitle != conversation.Title || target.ConversationLocator != "locator-1" || target.AccessibleLabel != "Example article" {
		t.Fatalf("unsafe surface target: %+v", target)
	}
}

func TestUIIndexRejectsDuplicateExactTitlesForSend(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline},
		visibleConversations: []VisibleConversation{
			{Title: "Duplicate", Locator: "locator-1"},
			{Title: "Duplicate", Locator: "locator-2"},
		},
	}
	index := NewUIIndex(backend, "wx-main", time.Now)
	conversations, err := index.ListConversations(context.Background(), shared.ConversationQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 {
		t.Fatalf("duplicate title should have one stable UI identity, got %d", len(conversations))
	}
	if _, err := index.Conversation(context.Background(), conversations[0].ID); !errors.Is(err, ErrTargetAmbiguous) {
		t.Fatalf("duplicate title resolution error = %v", err)
	}
}

func TestDriverDefaultsToUIIndexAndVerifiesNewExactBubble(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		visibleConversations: []VisibleConversation{
			{Title: "Exact Project Group", Locator: "locator-1"},
		},
		visibleMessages: []VisibleMessage{
			{Text: "same text", Kind: "text", Outgoing: true, Confidence: uiConfidence},
		},
		selectedTitle: "Exact Project Group", selectedLocator: "locator-1",
		appendTextOnSend: true,
	}
	temporary := t.TempDir()
	driver, err := New(Config{
		Backend: backend, VerificationTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main",
		StateDir: filepath.Join(temporary, "state"), RuntimeDir: filepath.Join(temporary, "runtime"),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	conversations, err := driver.ListConversations(context.Background(), shared.ConversationQuery{})
	if err != nil || len(conversations) != 1 {
		t.Fatalf("fallback conversations=%+v err=%v", conversations, err)
	}
	result, err := driver.Send(context.Background(), shared.SendRequest{
		ConversationID: conversations[0].ID, Text: "same text", IdempotencyKey: "ui-send-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.MessageID == "" || result.Uncertain {
		t.Fatalf("UI fallback did not verify the new bubble: %+v", result)
	}
	if len(backend.sends) != 1 || backend.sends[0].Title != "Exact Project Group" || backend.sends[0].Locator != "locator-1" {
		t.Fatalf("send did not use exact visible target: %#v", backend.sends)
	}
}

func TestDriverUIFallbackNeverSendsDuplicateTitle(t *testing.T) {
	backend := &fakeBackend{
		probe: ProbeResult{State: shared.StateOnline, ObservedAt: time.Now()},
		visibleConversations: []VisibleConversation{
			{Title: "Duplicate", Locator: "locator-1"},
			{Title: "Duplicate", Locator: "locator-2"},
		},
	}
	temporary := t.TempDir()
	driver, err := New(Config{Backend: backend, VerificationTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main",
		StateDir: filepath.Join(temporary, "state"), RuntimeDir: filepath.Join(temporary, "runtime"),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	conversations, err := driver.ListConversations(context.Background(), shared.ConversationQuery{})
	if err != nil || len(conversations) != 1 {
		t.Fatalf("fallback conversations=%+v err=%v", conversations, err)
	}
	_, err = driver.Send(context.Background(), shared.SendRequest{
		ConversationID: conversations[0].ID, Text: "must not send", IdempotencyKey: "duplicate-send",
	})
	if !errors.Is(err, ErrTargetAmbiguous) {
		t.Fatalf("duplicate-title send error = %v", err)
	}
	if len(backend.sends) != 0 {
		t.Fatalf("duplicate-title request reached UI backend: %#v", backend.sends)
	}
}
