// Package mcpserver exposes the daemon's semantic API over MCP stdio. It has
// no direct access to client profiles, Docker, X11, or ADB.
package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/client"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type daemonClient interface {
	Get(context.Context, string, any) error
	Post(context.Context, string, any, any) error
}

type Server struct {
	server *mcp.Server
	client daemonClient
}

func New(socket, version string) *Server {
	return NewWithClient(client.New(socket), version)
}

func NewWithClient(daemon daemonClient, version string) *Server {
	if version == "" {
		version = "dev"
	}
	s := &Server{
		server: mcp.NewServer(&mcp.Implementation{Name: "wechatcopilot", Version: version}, nil),
		client: daemon,
	}
	s.addTools()
	return s
}

func (s *Server) Run(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) MCPServer() *mcp.Server { return s.server }

type emptyInput struct{}

type accountInput struct {
	AccountID string `json:"account_id" jsonschema:"Opaque account ID returned by accounts_list."`
}

type accountAddInput struct {
	Alias    string          `json:"alias" jsonschema:"Unique lowercase alias used by the human operator."`
	Platform driver.Platform `json:"platform" jsonschema:"Platform: wechat or wecom."`
}

type accountRemoveInput struct {
	AccountID string `json:"account_id" jsonschema:"Exact opaque account ID."`
	Purge     bool   `json:"purge" jsonschema:"Must be true together with confirmed; permanently deletes the local profile, runtime, and message index."`
	Confirmed bool   `json:"confirmed" jsonschema:"Must be true together with purge, and only after the user authorizes permanent removal of this exact account."`
}

type conversationsInput struct {
	AccountID string `json:"account_id"`
	Search    string `json:"search,omitempty"`
	Unread    bool   `json:"unread,omitempty"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum results, from 1 to 500."`
}

type messagesInput struct {
	AccountID      string `json:"account_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	AfterSequence  int64  `json:"after_sequence,omitempty"`
	Limit          int    `json:"limit,omitempty" jsonschema:"Maximum results, from 1 to 1000."`
	Latest         bool   `json:"latest,omitempty" jsonschema:"Return the latest matching messages, still ordered by ascending local sequence."`
}

type messagesPollInput struct {
	AccountID     string `json:"account_id"`
	AfterSequence int64  `json:"after_sequence,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty" jsonschema:"Bounded wait in milliseconds, at most 30000."`
	Limit         int    `json:"limit,omitempty"`
}

type messagesSearchInput struct {
	AccountID string `json:"account_id"`
	Text      string `json:"text"`
	Limit     int    `json:"limit,omitempty"`
}

type prepareAttachmentInput struct {
	LocalPath string `json:"local_path" jsonschema:"Absolute path to a regular, non-symlink file on the daemon host. Name, size, and SHA-256 are derived by the daemon."`
}

type prepareSendInput struct {
	AccountID      string                   `json:"account_id"`
	ConversationID string                   `json:"conversation_id"`
	Text           string                   `json:"text,omitempty"`
	Attachments    []prepareAttachmentInput `json:"attachments,omitempty"`
	ShareSurfaceID string                   `json:"share_surface_id,omitempty"`
}

type commitSendInput struct {
	TransactionID  string `json:"transaction_id"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"Unique key for this exact logical send."`
	Confirmed      bool   `json:"confirmed" jsonschema:"True only after the exact prepared preview is authorized."`
}

type surfaceOpenInput struct {
	AccountID   string `json:"account_id"`
	Reference   string `json:"reference,omitempty" jsonschema:"Message-backed URL or mini-program reference returned by the message API; mutually exclusive with mini_program."`
	MiniProgram string `json:"mini_program,omitempty" jsonschema:"Exact mini-program display name; mutually exclusive with reference."`
}

type surfaceInput struct {
	AccountID string `json:"account_id"`
	SurfaceID string `json:"surface_id"`
}

type surfaceActInput struct {
	AccountID string  `json:"account_id"`
	SurfaceID string  `json:"surface_id"`
	ActionID  string  `json:"action_id" jsonschema:"Semantic action ID from the latest surface snapshot."`
	Text      *string `json:"text,omitempty" jsonschema:"Replacement text for an advertised kind=input action; explicitly pass an empty string to clear it. At most 4096 Unicode code points and no NUL. OCR input must resolve to one focused editable target and pass value readback."`
	Confirmed bool    `json:"confirmed,omitempty" jsonschema:"True only after the user authorizes this exact current action; required for medium or unknown risk and external-write actions, and never overrides a high-risk block."`
}

type surfaceExportInput struct {
	AccountID  string `json:"account_id"`
	SurfaceID  string `json:"surface_id"`
	AssetToken string `json:"asset_token" jsonschema:"Short-lived token from the latest snapshot of this exact surface."`
}

type surfaceDaemonOutput struct {
	Surface          driver.Surface `json:"surface"`
	ScreenshotBase64 string         `json:"screenshot_base64,omitempty"`
}

type surfaceExportDaemonOutput struct {
	Asset      driver.SurfaceAssetExport `json:"asset"`
	DataBase64 string                    `json:"data_base64,omitempty"`
}

func (s *Server) addTools() {
	readOnly := toolAnnotations(true, false, false, false)
	localWrite := toolAnnotations(false, true, false, false)
	externalWrite := toolAnnotations(false, true, false, true)
	destructive := toolAnnotations(false, false, true, false)
	interactive := toolAnnotations(false, false, true, true)

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "accounts_list", Description: "List saved personal WeChat and WeCom accounts and their local runtime state.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
		var output []account.Account
		err := s.client.Get(ctx, "/v1/accounts", &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "account_add", Description: "Create a new isolated saved profile; this never imports an existing desktop login.", Annotations: localWrite,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountAddInput) (*mcp.CallToolResult, any, error) {
		var output account.Account
		err := s.client.Post(ctx, "/v1/accounts/add", map[string]any{"alias": in.Alias, "platform": in.Platform}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "account_status", Description: "Read the official-client runtime and login state for one account.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountInput) (*mcp.CallToolResult, any, error) {
		var output driver.Status
		err := s.client.Post(ctx, "/v1/accounts/status", daemonAccount(in.AccountID), &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "account_activate", Description: "Start this account's isolated official client, stopping another active account on the same platform.", Annotations: localWrite,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountInput) (*mcp.CallToolResult, any, error) {
		var output account.Account
		err := s.client.Post(ctx, "/v1/accounts/activate", daemonAccount(in.AccountID), &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "account_deactivate", Description: "Stop an account runtime while preserving its profile and indexed messages.", Annotations: localWrite,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountInput) (*mcp.CallToolResult, any, error) {
		var output account.Account
		err := s.client.Post(ctx, "/v1/accounts/deactivate", daemonAccount(in.AccountID), &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "account_remove", Description: "Permanently delete a deactivated account and all local state; purge=true and confirmed=true are both required. If deleting=true, only retry this exact removal.", Annotations: destructive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountRemoveInput) (*mcp.CallToolResult, any, error) {
		var output account.Account
		err := s.client.Post(ctx, "/v1/accounts/remove", map[string]any{
			"account": in.AccountID, "purge": in.Purge, "confirmed": in.Confirmed,
		}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "capabilities_get", Description: "Read version-specific support levels before using a platform operation.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accountInput) (*mcp.CallToolResult, any, error) {
		var output map[string]driver.Support
		err := s.client.Post(ctx, "/v1/capabilities", daemonAccount(in.AccountID), &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "conversations_list", Description: "List locally indexed conversations, optionally filtered by title or unread state.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in conversationsInput) (*mcp.CallToolResult, any, error) {
		var output []driver.Conversation
		err := s.client.Post(ctx, "/v1/conversations/list", map[string]any{
			"account": in.AccountID, "search": in.Search, "unread": in.Unread, "limit": in.Limit,
		}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "messages_list", Description: "Read locally indexed messages in ascending sequence order; latest=true selects the newest bounded matching window.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in messagesInput) (*mcp.CallToolResult, any, error) {
		var output []driver.Message
		err := s.client.Post(ctx, "/v1/messages/list", map[string]any{
			"account": in.AccountID, "conversation_id": in.ConversationID,
			"after_sequence": in.AfterSequence, "limit": in.Limit, "latest": in.Latest,
		}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "messages_poll", Description: "Wait up to 30 seconds for indexed messages after a sequence cursor.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in messagesPollInput) (*mcp.CallToolResult, any, error) {
		var output []driver.Message
		err := s.client.Post(ctx, "/v1/messages/watch", map[string]any{
			"account": in.AccountID, "after_sequence": in.AfterSequence,
			"timeout_ms": in.TimeoutMS, "limit": in.Limit,
		}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "messages_search", Description: "Full-text search the selected account's local message index.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in messagesSearchInput) (*mcp.CallToolResult, any, error) {
		var output []driver.Message
		err := s.client.Post(ctx, "/v1/messages/search", map[string]any{"account": in.AccountID, "text": in.Text, "limit": in.Limit}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "messages_prepare_send", Description: "Prepare and preview an exact send without sending it; commit separately after authorization.", Annotations: toolAnnotations(false, false, false, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in prepareSendInput) (*mcp.CallToolResult, any, error) {
		attachments := make([]driver.Attachment, 0, len(in.Attachments))
		for _, attachment := range in.Attachments {
			attachments = append(attachments, driver.Attachment{LocalPath: attachment.LocalPath})
		}
		var output service.PreparedSend
		err := s.client.Post(ctx, "/v1/messages/prepare-send", map[string]any{
			"account": in.AccountID, "conversation_id": in.ConversationID, "text": in.Text,
			"attachments": attachments, "share_surface_id": in.ShareSurfaceID,
		}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "messages_commit_send", Description: "Send one previously prepared transaction through the user's official client.", Annotations: externalWrite,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in commitSendInput) (*mcp.CallToolResult, any, error) {
		var output driver.SendResult
		err := s.client.Post(ctx, "/v1/messages/commit-send", map[string]any{
			"transaction_id": in.TransactionID, "idempotency_key": in.IdempotencyKey, "confirmed": in.Confirmed,
		}, &output)
		return toolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "surfaces_open", Description: "Open exactly one message-backed surface or named mini program in the selected official client.", Annotations: interactive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in surfaceOpenInput) (*mcp.CallToolResult, any, error) {
		var output surfaceDaemonOutput
		err := s.client.Post(ctx, "/v1/surfaces/open", map[string]any{
			"account": in.AccountID, "ref": in.Reference, "mini_program": in.MiniProgram,
		}, &output)
		return surfaceToolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "surfaces_snapshot", Description: "Read the current screenshot, semantic text, and allowed action IDs for an open surface.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in surfaceInput) (*mcp.CallToolResult, any, error) {
		var output surfaceDaemonOutput
		err := s.client.Post(ctx, "/v1/surfaces/snapshot", map[string]any{"account": in.AccountID, "id": in.SurfaceID}, &output)
		return surfaceToolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "surfaces_act", Description: "Use one semantic action from the latest snapshot, optionally supplying replacement text and exact-action confirmation; high-risk actions are blocked.", Annotations: interactive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in surfaceActInput) (*mcp.CallToolResult, any, error) {
		var output surfaceDaemonOutput
		input := map[string]any{
			"account": in.AccountID, "id": in.SurfaceID, "action_id": in.ActionID,
			"confirmed": in.Confirmed,
		}
		if in.Text != nil {
			input["text"] = *in.Text
		}
		err := s.client.Post(ctx, "/v1/surfaces/act", input, &output)
		return surfaceToolResult(output, err)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "surfaces_export", Description: "Export one rendered image crop using an asset token from the latest exact surface snapshot.", Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in surfaceExportInput) (*mcp.CallToolResult, any, error) {
		var output surfaceExportDaemonOutput
		err := s.client.Post(ctx, "/v1/surfaces/export", map[string]any{
			"account": in.AccountID, "id": in.SurfaceID, "asset_token": in.AssetToken,
		}, &output)
		if err != nil {
			return toolResult(output.Asset, err)
		}
		data, decodeErr := base64.StdEncoding.DecodeString(output.DataBase64)
		if decodeErr != nil || len(data) == 0 {
			return toolResult(output.Asset, api.WrapError(http.StatusInternalServerError, api.CodeInternal, "daemon returned invalid rendered image data", errors.Join(decodeErr)))
		}
		digest := sha256.Sum256(data)
		if output.Asset.Fidelity != "rendered" || output.Asset.MediaType != "image/png" ||
			output.Asset.Bytes != int64(len(data)) || output.Asset.SHA256 != hex.EncodeToString(digest[:]) {
			return toolResult(output.Asset, api.NewError(http.StatusInternalServerError, api.CodeInternal, "daemon returned inconsistent rendered image metadata"))
		}
		return toolImageResult(output.Asset, data, output.Asset.MediaType)
	})

	mcp.AddTool(s.server, &mcp.Tool{
		Name: "surfaces_close", Description: "Close an open official-client surface without sending or sharing it.", Annotations: localWrite,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in surfaceInput) (*mcp.CallToolResult, any, error) {
		var output map[string]bool
		err := s.client.Post(ctx, "/v1/surfaces/close", map[string]any{"account": in.AccountID, "id": in.SurfaceID}, &output)
		return toolResult(output, err)
	})
}

func daemonAccount(accountID string) map[string]string {
	return map[string]string{"account": accountID}
}

func toolAnnotations(readOnly, idempotent, destructive, openWorld bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint: readOnly, IdempotentHint: idempotent,
		DestructiveHint: boolPointer(destructive), OpenWorldHint: boolPointer(openWorld),
	}
}

func boolPointer(value bool) *bool { return &value }

func toolResult(data any, err error) (*mcp.CallToolResult, any, error) {
	var envelope api.Response
	if err == nil {
		envelope = api.Success(data)
	} else {
		envelope, _ = api.Failure(err)
	}
	encoded, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		return nil, nil, marshalErr
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		StructuredContent: envelope,
		IsError:           err != nil,
	}, nil, nil
}

func toolImageResult(metadata any, data []byte, mediaType string) (*mcp.CallToolResult, any, error) {
	envelope := api.Success(metadata)
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(encoded)},
			&mcp.ImageContent{Data: data, MIMEType: mediaType},
		},
		StructuredContent: envelope,
	}, nil, nil
}

func surfaceToolResult(output surfaceDaemonOutput, operationErr error) (*mcp.CallToolResult, any, error) {
	if operationErr != nil {
		output.ScreenshotBase64 = ""
		return toolResult(output, operationErr)
	}
	data, err := base64.StdEncoding.DecodeString(output.ScreenshotBase64)
	if err != nil || len(data) == 0 {
		return toolResult(output.Surface, api.WrapError(http.StatusInternalServerError, api.CodeInternal, "daemon returned invalid surface screenshot data", errors.Join(err)))
	}
	digest := sha256.Sum256(data)
	if output.Surface.ScreenshotSHA256 == "" || output.Surface.ScreenshotSHA256 != hex.EncodeToString(digest[:]) {
		return toolResult(output.Surface, api.NewError(http.StatusInternalServerError, api.CodeInternal, "daemon returned a screenshot from another surface frame"))
	}
	output.ScreenshotBase64 = ""
	return toolImageResult(output, data, "image/png")
}
