package mcpserver

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolsExcludeAuthenticationSecretsAndAnnotateWrites(t *testing.T) {
	daemon := &stubDaemon{getValue: []account.Account{}}
	server := NewWithClient(daemon, "test")
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	protocolClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := protocolClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	tools := make(map[string]*mcp.Tool)
	for tool, err := range clientSession.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = tool
	}
	if _, ok := tools["auth_begin"]; ok {
		t.Fatal("MCP exposes auth_begin and could leak the one-time bearer URL")
	}
	if _, ok := tools["auth_status"]; ok {
		t.Fatal("MCP exposes auth_status")
	}
	commit := tools["messages_commit_send"]
	if commit == nil || commit.Annotations == nil {
		t.Fatal("messages_commit_send annotations are missing")
	}
	if commit.Annotations.ReadOnlyHint || !commit.Annotations.IdempotentHint || commit.Annotations.OpenWorldHint == nil || !*commit.Annotations.OpenWorldHint {
		t.Fatalf("unsafe commit annotations: %#v", commit.Annotations)
	}
	list := tools["accounts_list"]
	if list == nil || list.Annotations == nil || !list.Annotations.ReadOnlyHint {
		t.Fatalf("accounts_list is not read-only: %#v", list)
	}
	messagesList := tools["messages_list"]
	if messagesList == nil || messagesList.InputSchema == nil {
		t.Fatal("messages_list schema is missing")
	}
	var schema map[string]any
	if err := copyJSON(&schema, messagesList.InputSchema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["latest"]; !ok {
		t.Fatalf("messages_list schema has no latest input: %#v", schema)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "accounts_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("accounts_list result: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("accounts_list content type: %T", result.Content[0])
	}
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
	}
	if err := json.Unmarshal([]byte(text.Text), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.SchemaVersion != "1" {
		t.Fatalf("unexpected MCP envelope: %#v", envelope)
	}

	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "messages_list", Arguments: map[string]any{
		"account_id": "account-1", "conversation_id": "conversation-1", "limit": 25, "latest": true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("messages_list result=%#v err=%v", result, err)
	}
	path, input := daemon.lastPost()
	if path != "/v1/messages/list" || input["latest"] != true {
		t.Fatalf("messages_list daemon request path=%q input=%#v", path, input)
	}
}

type stubDaemon struct {
	mu       sync.Mutex
	getValue any
	getErr   error
	postErr  error
	postPath string
	postBody map[string]any
}

func (s *stubDaemon) Get(_ context.Context, _ string, output any) error {
	if s.getErr != nil {
		return s.getErr
	}
	return copyJSON(output, s.getValue)
}

func (s *stubDaemon) Post(_ context.Context, path string, input, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postPath = path
	s.postBody = make(map[string]any)
	if err := copyJSON(&s.postBody, input); err != nil {
		return err
	}
	return s.postErr
}

func (s *stubDaemon) lastPost() (string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body := make(map[string]any, len(s.postBody))
	for key, value := range s.postBody {
		body[key] = value
	}
	return s.postPath, body
}

func copyJSON(destination, source any) error {
	encoded, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, destination)
}
