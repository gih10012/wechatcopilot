package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/driver"
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
	openTool := tools["surfaces_open"]
	if openTool == nil || openTool.Annotations == nil || openTool.Annotations.IdempotentHint {
		t.Fatalf("surfaces_open must be explicitly non-idempotent: %#v", openTool)
	}
	actTool := tools["surfaces_act"]
	if actTool == nil || actTool.InputSchema == nil {
		t.Fatal("surfaces_act schema is missing")
	}
	var actSchema map[string]any
	if err := copyJSON(&actSchema, actTool.InputSchema); err != nil {
		t.Fatal(err)
	}
	for _, required := range actSchema["required"].([]any) {
		if required == "text" {
			t.Fatalf("surfaces_act schema incorrectly requires text: %#v", actSchema)
		}
	}
	if exportTool := tools["surfaces_export"]; exportTool == nil || exportTool.Annotations == nil || !exportTool.Annotations.ReadOnlyHint {
		t.Fatalf("surfaces_export must be read-only: %#v", exportTool)
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

	frame := []byte("frame bytes delivered separately from structured content")
	frameDigest := sha256.Sum256(frame)
	daemon.setPostValue(surfaceDaemonOutput{
		Surface:          driver.Surface{ID: "surface-1", Kind: "miniprogram", ScreenshotSHA256: hex.EncodeToString(frameDigest[:])},
		ScreenshotBase64: base64.StdEncoding.EncodeToString(frame),
	})
	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "surfaces_open", Arguments: map[string]any{
		"account_id": "account-1", "mini_program": "校园瞄",
	}})
	if err != nil || result.IsError || len(result.Content) != 2 {
		t.Fatalf("surfaces_open result=%#v err=%v", result, err)
	}
	imageContent, ok := result.Content[1].(*mcp.ImageContent)
	if !ok || string(imageContent.Data) != string(frame) || imageContent.MIMEType != "image/png" {
		t.Fatalf("surfaces_open image content = %#v", result.Content[1])
	}
	metadataText, ok := result.Content[0].(*mcp.TextContent)
	if !ok || strings.Contains(metadataText.Text, base64.StdEncoding.EncodeToString(frame)) || strings.Contains(metadataText.Text, "screenshot_base64") {
		t.Fatalf("surface metadata leaked inline image data: %#v", result.Content[0])
	}
	path, input = daemon.lastPost()
	if path != "/v1/surfaces/open" || input["mini_program"] != "校园瞄" {
		t.Fatalf("surfaces_open daemon request path=%q input=%#v", path, input)
	}
	daemon.setPostValue(surfaceDaemonOutput{
		Surface:          driver.Surface{ID: "surface-1", Kind: "miniprogram", ScreenshotSHA256: hex.EncodeToString(frameDigest[:])},
		ScreenshotBase64: base64.StdEncoding.EncodeToString(frame),
	})
	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "surfaces_act", Arguments: map[string]any{
		"account_id": "account-1", "surface_id": "surface-1", "action_id": "canvas-input",
		"text": "宿舍", "confirmed": true,
	}})
	if err != nil || result.IsError || len(result.Content) != 2 {
		t.Fatalf("surfaces_act result=%#v err=%v", result, err)
	}
	path, input = daemon.lastPost()
	if path != "/v1/surfaces/act" || input["action_id"] != "canvas-input" ||
		input["text"] != "宿舍" || input["confirmed"] != true {
		t.Fatalf("surfaces_act daemon request path=%q input=%#v", path, input)
	}
	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "surfaces_act", Arguments: map[string]any{
		"account_id": "account-1", "surface_id": "surface-1", "action_id": "read-only-action",
	}})
	if err != nil || result.IsError {
		t.Fatalf("surfaces_act without text result=%#v err=%v", result, err)
	}
	path, input = daemon.lastPost()
	if path != "/v1/surfaces/act" {
		t.Fatalf("surfaces_act without text path=%q", path)
	}
	if _, ok := input["text"]; ok {
		t.Fatalf("surfaces_act without text sent a text field: %#v", input)
	}
	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "surfaces_act", Arguments: map[string]any{
		"account_id": "account-1", "surface_id": "surface-1", "action_id": "canvas-input", "text": "",
	}})
	if err != nil || result.IsError {
		t.Fatalf("surfaces_act with explicit empty text result=%#v err=%v", result, err)
	}
	path, input = daemon.lastPost()
	if text, ok := input["text"]; path != "/v1/surfaces/act" || !ok || text != "" {
		t.Fatalf("surfaces_act did not preserve explicit empty text: path=%q input=%#v", path, input)
	}
	assetData := []byte("rendered png fixture")
	assetDigest := sha256.Sum256(assetData)
	daemon.setPostValue(surfaceExportDaemonOutput{
		Asset: driver.SurfaceAssetExport{
			SurfaceID: "surface-1", AssetID: "asset-1", Fidelity: "rendered", MediaType: "image/png",
			SHA256: hex.EncodeToString(assetDigest[:]), Bytes: int64(len(assetData)),
		},
		DataBase64: base64.StdEncoding.EncodeToString(assetData),
	})
	result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "surfaces_export", Arguments: map[string]any{
		"account_id": "account-1", "surface_id": "surface-1", "asset_token": "short-token",
	}})
	if err != nil || result.IsError || len(result.Content) != 2 {
		t.Fatalf("surfaces_export result=%#v err=%v", result, err)
	}
	assetImage, ok := result.Content[1].(*mcp.ImageContent)
	if !ok || string(assetImage.Data) != string(assetData) {
		t.Fatalf("surfaces_export image = %#v", result.Content[1])
	}
	assetText, ok := result.Content[0].(*mcp.TextContent)
	if !ok || strings.Contains(assetText.Text, base64.StdEncoding.EncodeToString(assetData)) || strings.Contains(assetText.Text, "data_base64") {
		t.Fatalf("asset metadata leaked inline image data: %#v", result.Content[0])
	}
}

type stubDaemon struct {
	mu        sync.Mutex
	getValue  any
	getErr    error
	postErr   error
	postValue any
	postPath  string
	postBody  map[string]any
}

func (s *stubDaemon) Get(_ context.Context, _ string, output any) error {
	if s.getErr != nil {
		return s.getErr
	}
	return copyJSON(output, s.getValue)
}

func (s *stubDaemon) Post(_ context.Context, path string, input, output any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postPath = path
	s.postBody = make(map[string]any)
	if err := copyJSON(&s.postBody, input); err != nil {
		return err
	}
	if s.postValue != nil {
		if err := copyJSON(output, s.postValue); err != nil {
			return err
		}
	}
	return s.postErr
}

func (s *stubDaemon) setPostValue(value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postValue = value
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
