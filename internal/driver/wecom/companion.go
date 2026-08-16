package wecom

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type Bounds struct {
	Left   int `json:"left"`
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
}

type Node struct {
	ID                 string `json:"id"`
	ParentID           string `json:"parent_id,omitempty"`
	ClassName          string `json:"class_name,omitempty"`
	ViewID             string `json:"view_id,omitempty"`
	Text               string `json:"text,omitempty"`
	ContentDescription string `json:"content_description,omitempty"`
	Bounds             Bounds `json:"bounds"`
	Clickable          bool   `json:"clickable"`
	Checkable          bool   `json:"checkable"`
	Checked            bool   `json:"checked"`
	Editable           bool   `json:"editable"`
	Scrollable         bool   `json:"scrollable"`
	Enabled            bool   `json:"enabled"`
	Focused            bool   `json:"focused"`
	VisibleToUser      bool   `json:"visible_to_user"`
}

type UISnapshot struct {
	Sequence    int64     `json:"sequence"`
	PackageName string    `json:"package_name"`
	WindowTitle string    `json:"window_title,omitempty"`
	WindowClass string    `json:"window_class,omitempty"`
	CapturedAt  time.Time `json:"captured_at"`
	Nodes       []Node    `json:"nodes"`
}

type CompanionEvent struct {
	Sequence        int64     `json:"sequence"`
	Kind            string    `json:"kind"`
	PackageName     string    `json:"package_name"`
	ConversationKey string    `json:"conversation_key"`
	Conversation    string    `json:"conversation,omitempty"`
	Sender          string    `json:"sender,omitempty"`
	Title           string    `json:"title,omitempty"`
	Text            string    `json:"text,omitempty"`
	Openable        bool      `json:"openable"`
	PostedAt        time.Time `json:"posted_at"`
}

type EventPage struct {
	Events     []CompanionEvent `json:"events"`
	NextCursor int64            `json:"next_cursor"`
	Complete   bool             `json:"complete"`
}

type CompanionAction struct {
	Kind             string `json:"kind"`
	NodeID           string `json:"node_id,omitempty"`
	Text             string `json:"text,omitempty"`
	ExpectedSequence int64  `json:"expected_sequence,omitempty"`
}

const (
	ActionClick            = "click"
	ActionCheck            = "check"
	ActionSetText          = "set_text"
	ActionScrollForward    = "scroll_forward"
	ActionScrollBackward   = "scroll_backward"
	ActionGlobalBack       = "global_back"
	ActionOpenNotification = "open_notification"
)

type ActionResult struct {
	Accepted bool   `json:"accepted"`
	Sequence int64  `json:"sequence"`
	Detail   string `json:"detail,omitempty"`
}

// ErrActionOutcomeUncertain means the companion may have executed an action,
// but no explicit, decodable acceptance result reached the daemon. Retrying
// such an action could repeat or reverse a user-confirmed operation.
var ErrActionOutcomeUncertain = errors.New("companion action outcome is uncertain")

type CompanionClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func newContainerCompanionClient(android AndroidContainer, devicePort int, token string, timeout time.Duration) (*CompanionClient, error) {
	if devicePort < 1024 || devicePort > 65535 {
		return nil, errors.New("invalid companion device port")
	}
	if !TokenValid(token) {
		return nil, errors.New("companion token is invalid")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	host := "127.0.0.1:" + strconv.Itoa(devicePort)
	return &CompanionClient{
		baseURL: "http://" + host,
		token:   token,
		client: &http.Client{
			Timeout: timeout,
			Transport: &containerRoundTripper{
				android:    android,
				host:       host,
				devicePort: devicePort,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("companion redirects are not allowed")
			},
		},
	}, nil
}

func newCompanionClientForURL(baseURL, token string, client *http.Client) (*CompanionClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		return nil, errors.New("companion URL must use HTTP over loopback")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("companion URL must resolve directly to loopback")
	}
	if !TokenValid(token) {
		return nil, errors.New("companion token is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &CompanionClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client}, nil
}

type containerRoundTripper struct {
	android    AndroidContainer
	host       string
	devicePort int
}

func (t *containerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "http" || request.URL.Host != t.host || request.Host != "" && request.Host != t.host {
		return nil, errors.New("companion request left the fixed Android loopback endpoint")
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return nil, errors.New("unsupported companion HTTP method")
	}
	var wire bytes.Buffer
	if err := request.Write(&wire); err != nil {
		return nil, fmt.Errorf("encode companion wire request: %w", err)
	}
	if wire.Len() > 128<<10 {
		return nil, errors.New("companion wire request exceeds 128 KiB")
	}
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
		// The opaque container invocation may deliver the request even when it
		// later returns no response, so report dispatch before entering it.
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
	rawResponse, err := t.android.CompanionRequest(request.Context(), t.devicePort, wire.Bytes())
	if err != nil {
		return nil, fmt.Errorf("execute companion request inside Redroid: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(rawResponse)), request)
	if err != nil {
		return nil, fmt.Errorf("parse companion wire response: %w", err)
	}
	if response.ContentLength < 0 || response.ContentLength > 8<<20 {
		response.Body.Close()
		return nil, errors.New("companion response has an invalid content length")
	}
	return response, nil
}

func (c *CompanionClient) Health(ctx context.Context) error {
	var response struct {
		OK bool `json:"ok"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/health", nil, &response); err != nil {
		return err
	}
	if !response.OK {
		return errors.New("companion reported unhealthy")
	}
	return nil
}

func (c *CompanionClient) Snapshot(ctx context.Context) (UISnapshot, error) {
	var snapshot UISnapshot
	err := c.request(ctx, http.MethodGet, "/v1/snapshot", nil, &snapshot)
	return snapshot, err
}

func (c *CompanionClient) Events(ctx context.Context, after int64, limit int) (EventPage, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var page EventPage
	path := "/v1/events?after=" + strconv.FormatInt(after, 10) + "&limit=" + strconv.Itoa(limit)
	err := c.request(ctx, http.MethodGet, path, nil, &page)
	return page, err
}

func (c *CompanionClient) Act(ctx context.Context, action CompanionAction) (ActionResult, error) {
	if err := validateCompanionAction(action); err != nil {
		return ActionResult{}, err
	}
	var requestWritten atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			// Even a partial-write error cannot prove that the small action body
			// was not delivered and acted on.
			requestWritten.Store(true)
		},
	})
	var wireResult struct {
		Accepted *bool  `json:"accepted"`
		Sequence int64  `json:"sequence"`
		Detail   string `json:"detail,omitempty"`
	}
	err := c.request(ctx, http.MethodPost, "/v1/actions", action, &wireResult)
	if err != nil {
		if requestWritten.Load() {
			return ActionResult{}, fmt.Errorf("%w: %w", ErrActionOutcomeUncertain, err)
		}
		return ActionResult{}, err
	}
	if wireResult.Accepted == nil {
		return ActionResult{}, fmt.Errorf("%w: companion response omitted accepted", ErrActionOutcomeUncertain)
	}
	result := ActionResult{
		Accepted: *wireResult.Accepted,
		Sequence: wireResult.Sequence,
		Detail:   wireResult.Detail,
	}
	if !result.Accepted {
		if containsAny(result.Detail, "stale", "missing", "no longer") {
			err = fmt.Errorf("%w: companion rejected action: %s", ErrStale, result.Detail)
		} else {
			err = fmt.Errorf("companion rejected action: %s", result.Detail)
		}
	}
	return result, err
}

func validateCompanionAction(action CompanionAction) error {
	switch action.Kind {
	case ActionClick, ActionCheck, ActionScrollForward, ActionScrollBackward:
		if action.NodeID == "" || action.Text != "" || action.ExpectedSequence <= 0 {
			return errors.New("node action requires node_id, expected_sequence, and no text")
		}
	case ActionSetText:
		if action.NodeID == "" || action.ExpectedSequence <= 0 {
			return errors.New("set_text requires node_id and expected_sequence")
		}
		if len(action.Text) > 32*1024 {
			return errors.New("set_text payload exceeds 32 KiB")
		}
	case ActionGlobalBack:
		if action.NodeID != "" || action.Text != "" || action.ExpectedSequence != 0 {
			return errors.New("global_back does not accept parameters")
		}
	case ActionOpenNotification:
		if action.NodeID == "" || action.Text != "" || action.ExpectedSequence != 0 {
			return errors.New("open_notification requires the event sequence as node_id")
		}
	default:
		return fmt.Errorf("unsupported companion action %q", action.Kind)
	}
	return nil
}

func (c *CompanionClient) request(ctx context.Context, method, path string, body, result any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode companion request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("build companion request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call companion: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("companion returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if result == nil {
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("decode companion response: %w", err)
	}
	return nil
}
