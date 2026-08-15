package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestNotificationRecordsSatisfySharedIndexContract(t *testing.T) {
	token := "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	event := CompanionEvent{
		Sequence: 7, Kind: "notification", PackageName: DefaultWeComPackage,
		ConversationKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Conversation:    "Project", Sender: "Alice", Text: "status update", Openable: true, PostedAt: time.Now().UTC(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
		page := EventPage{Complete: false}
		if after < event.Sequence {
			page.Events = []CompanionEvent{event}
			page.NextCursor = event.Sequence
		} else {
			page.Complete = true
		}
		_ = json.NewEncoder(writer).Encode(page)
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	runtime := &Runtime{
		config: config, companion: client, running: true,
		account: core.AccountRuntime{AccountID: "account-1", Alias: "work"},
	}
	driver := &Driver{
		runtime: runtime, account: runtime.account,
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	conversations, err := driver.ListConversations(context.Background(), core.ConversationQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID == "" || conversations[0].ExternalID == "" {
		t.Fatalf("conversation violates core index contract: %#v", conversations)
	}
	messages, err := driver.ReadMessages(context.Background(), core.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID == "" || messages[0].ExternalID == "" || messages[0].ConversationID != conversations[0].ID {
		t.Fatalf("message violates core index contract: %#v", messages)
	}
}

func TestCapabilitiesUseSharedCompleteContract(t *testing.T) {
	values := capabilities()
	if err := core.ValidateCapabilities(values); err != nil {
		t.Fatal(err)
	}
	if values[core.CapabilityMessagesSend] != core.SupportExperimental {
		t.Fatalf("text-send capability = %q", values[core.CapabilityMessagesSend])
	}
	if values[core.CapabilityMessagesHistory] != core.SupportUnsupported {
		t.Fatalf("history capability = %q", values[core.CapabilityMessagesHistory])
	}
}

func TestSameTitleNotificationKeysRemainDistinctConversations(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	events := []CompanionEvent{
		{Sequence: 7, Kind: "notification", PackageName: DefaultWeComPackage, ConversationKey: strings.Repeat("a", 64), Conversation: "同名群", Openable: true, PostedAt: time.Now().UTC()},
		{Sequence: 8, Kind: "notification", PackageName: DefaultWeComPackage, ConversationKey: strings.Repeat("b", 64), Conversation: "同名群", Openable: true, PostedAt: time.Now().UTC().Add(time.Second)},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
		page := EventPage{Complete: true, NextCursor: after}
		for _, event := range events {
			if event.Sequence > after {
				page.Events = append(page.Events, event)
				page.NextCursor = event.Sequence
			}
		}
		_ = json.NewEncoder(writer).Encode(page)
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		account: core.AccountRuntime{AccountID: "account-1"},
	}
	driver := &Driver{runtime: runtime, account: runtime.account, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	conversations, err := driver.ListConversations(context.Background(), core.ConversationQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 || conversations[0].ID == conversations[1].ID || conversations[0].Title != conversations[1].Title {
		t.Fatalf("same-title targets were merged or lost provenance: %#v", conversations)
	}
}

func TestConversationKeyMappingMultipleTitlesFailsClosed(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	key := strings.Repeat("c", 64)
	events := []CompanionEvent{
		{Sequence: 7, PackageName: DefaultWeComPackage, ConversationKey: key, Conversation: "Alpha", Openable: true},
		{Sequence: 8, PackageName: DefaultWeComPackage, ConversationKey: key, Conversation: "Beta", Openable: true},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(EventPage{Events: events, NextCursor: 8, Complete: true})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: DefaultConfig(), companion: client, running: true, account: core.AccountRuntime{AccountID: "account-1"}}
	driver := &Driver{runtime: runtime, account: runtime.account, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err = driver.resolveConversationEvent(context.Background(), conversationID("account-1", key))
	if !errors.Is(err, ErrTargetAmbiguous) {
		t.Fatalf("expected ambiguous stable-key mapping, got %v", err)
	}
}

func TestHighRiskSnapshotDisablesAllMutatingSurfaceActions(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nsynthetic")
	runtime := &Runtime{
		running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic", Executor: &recordingExecutor{output: png},
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	surface, err := driver.updateSurface(context.Background(), "surface-risk", UISnapshot{
		Sequence: 9, PackageName: DefaultWeComPackage, WindowTitle: "订单支付",
		Nodes: []Node{
			{ID: "0/confirm", Text: "确认", Clickable: true, Enabled: true},
			{ID: "0/note", Text: "备注", Editable: true, Enabled: true},
			{ID: "0/list", Text: "详情", Scrollable: true, Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range surface.Actions {
		if action.Kind == ActionClick || action.Kind == ActionSetText {
			if !action.Disabled || action.Risk != "high" {
				t.Fatalf("high-risk mutating action remained enabled: %#v", action)
			}
		}
	}
	if len(driver.surfaces["surface-risk"].actions) != 2 {
		t.Fatalf("only bounded scroll actions should remain enabled: %#v", driver.surfaces["surface-risk"].actions)
	}
}

func TestOpenSurfaceRejectsGenericUILabelReference(t *testing.T) {
	driver := &Driver{runtime: &Runtime{}, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err := driver.OpenSurface(context.Background(), "打开任意当前按钮")
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("expected generic label reference to be rejected, got %v", err)
	}
}
