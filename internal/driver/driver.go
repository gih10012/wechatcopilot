// Package driver defines the platform-neutral boundary between the daemon and
// the official-client automation backends.
package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Platform string

const (
	PlatformWeChat Platform = "wechat"
	PlatformWeCom  Platform = "wecom"
)

type RuntimeState string

const (
	StateStopped      RuntimeState = "STOPPED"
	StateStarting     RuntimeState = "STARTING"
	StateAuthRequired RuntimeState = "AUTH_REQUIRED"
	StateOnline       RuntimeState = "ONLINE"
	StateDegraded     RuntimeState = "DEGRADED"
	StateOffline      RuntimeState = "OFFLINE"
)

type Support string

const (
	SupportStable       Support = "stable"
	SupportBeta         Support = "beta"
	SupportExperimental Support = "experimental"
	SupportUnsupported  Support = "unsupported"
)

type FailureKind string

const (
	FailureInvalidArgument      FailureKind = "invalid_argument"
	FailureAuthRequired         FailureKind = "auth_required"
	FailureClientIncompatible   FailureKind = "client_incompatible"
	FailureTargetAmbiguous      FailureKind = "target_ambiguous"
	FailureUnsupported          FailureKind = "unsupported"
	FailureConfirmationRequired FailureKind = "confirmation_required"
	FailureUserActionRequired   FailureKind = "user_action_required"
	FailureNotFound             FailureKind = "not_found"
	FailureStale                FailureKind = "stale"
	FailureDriverUnavailable    FailureKind = "driver_unavailable"
	FailureSendUncertain        FailureKind = "send_uncertain"
)

// Failure gives the service a stable classification without coupling it to a
// concrete UI driver package. Drivers may wrap a shared sentinel with %w.
type Failure struct {
	Kind    FailureKind
	Message string
}

func (e *Failure) Error() string { return e.Message }

func NewFailure(kind FailureKind, message string) *Failure {
	return &Failure{Kind: kind, Message: message}
}

func ClassifyFailure(err error) (FailureKind, bool) {
	var failure *Failure
	if !errors.As(err, &failure) {
		return "", false
	}
	return failure.Kind, true
}

type AccountRuntime struct {
	AccountID  string            `json:"account_id"`
	Alias      string            `json:"alias"`
	StateDir   string            `json:"state_dir"`
	RuntimeDir string            `json:"runtime_dir"`
	Options    map[string]string `json:"options,omitempty"`
}

type Status struct {
	State         RuntimeState       `json:"state"`
	Identity      *Identity          `json:"identity,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	ClientVersion string             `json:"client_version,omitempty"`
	Capabilities  map[string]Support `json:"capabilities,omitempty"`
	ObservedAt    time.Time          `json:"observed_at"`
}

type Identity struct {
	PlatformID  string `json:"platform_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

type AuthKind string

const (
	AuthQR           AuthKind = "qr"
	AuthSMS          AuthKind = "sms"
	AuthPhoneConfirm AuthKind = "phone_confirmation"
)

type AuthSnapshot struct {
	Kind          AuthKind     `json:"kind"`
	State         RuntimeState `json:"state"`
	Prompt        string       `json:"prompt,omitempty"`
	QRCodePNG     []byte       `json:"-"`
	ScreenshotPNG []byte       `json:"-"`
	CanSubmitCode bool         `json:"can_submit_code"`
	Actions       []AuthAction `json:"actions,omitempty"`
	ObservedAt    time.Time    `json:"observed_at"`
}

// AuthAction is a narrowly advertised operation on the current official login
// screen. It never carries coordinates or a backend node identifier.
type AuthAction struct {
	ID                   string `json:"id"`
	Label                string `json:"label"`
	Risk                 string `json:"risk,omitempty"`
	Confirmation         string `json:"confirmation,omitempty"`
	RequiresConfirmation bool   `json:"requires_confirmation,omitempty"`
	ImageBound           bool   `json:"image_bound,omitempty"`
	// ReplayKey groups changing generation-bound IDs for one logical action.
	// It is challenge-local manager metadata and must never cross the API.
	ReplayKey string `json:"-"`
}

type AuthActionRequest struct {
	ActionID  string
	Confirmed bool
}

// AuthActionOutcomeError reports that an authentication action reached the
// official client even though its post-action verification failed. Callers
// must preserve the underlying error while treating the action as consumed so
// a retry cannot repeat or reverse the user-confirmed operation.
type AuthActionOutcomeError interface {
	error
	AuthActionConsumed() bool
}

type consumedAuthActionError struct{ err error }

func (e consumedAuthActionError) Error() string            { return e.err.Error() }
func (e consumedAuthActionError) Unwrap() error            { return e.err }
func (e consumedAuthActionError) AuthActionConsumed() bool { return true }

// MarkAuthActionConsumed preserves err's errors.Is/errors.As chain while
// marking the associated authentication action as unsafe to retry.
func MarkAuthActionConsumed(err error) error {
	if err == nil || AuthActionWasConsumed(err) {
		return err
	}
	return consumedAuthActionError{err: err}
}

// AuthActionWasConsumed detects a dispatched authentication action through
// arbitrarily wrapped errors.
func AuthActionWasConsumed(err error) bool {
	var outcome AuthActionOutcomeError
	return errors.As(err, &outcome) && outcome.AuthActionConsumed()
}

type Conversation struct {
	ID            string    `json:"id"`
	ExternalID    string    `json:"-"`
	Title         string    `json:"title"`
	Kind          string    `json:"kind"`
	UnreadCount   int       `json:"unread_count,omitempty"`
	LastMessageAt time.Time `json:"last_message_at,omitempty"`
	Complete      bool      `json:"complete"`
	Source        string    `json:"source"`
}

type Attachment struct {
	ID        string `json:"id,omitempty"`
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
}

type Message struct {
	ID             string `json:"id"`
	ExternalID     string `json:"-"`
	ConversationID string `json:"conversation_id"`
	// GapBefore marks a conversation-scoped journal boundary: continuity to an
	// earlier message in this same conversation is not proven.
	GapBefore   bool            `json:"gap_before,omitempty"`
	SenderID    string          `json:"sender_id,omitempty"`
	SenderName  string          `json:"sender_name,omitempty"`
	SentAt      time.Time       `json:"sent_at"`
	Kind        string          `json:"kind"`
	Text        string          `json:"text,omitempty"`
	Attachments []Attachment    `json:"attachments,omitempty"`
	ReplyTo     string          `json:"reply_to,omitempty"`
	SurfaceRef  string          `json:"surface_ref,omitempty"`
	Source      string          `json:"source"`
	Complete    bool            `json:"complete"`
	Confidence  float64         `json:"confidence"`
	Raw         json.RawMessage `json:"-"`
	Sequence    int64           `json:"sequence,omitempty"`
}

type ConversationQuery struct {
	Search string `json:"search,omitempty"`
	Unread bool   `json:"unread,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type MessageQuery struct {
	ConversationID string    `json:"conversation_id,omitempty"`
	AfterSequence  int64     `json:"after_sequence,omitempty"`
	Before         time.Time `json:"before,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	Latest         bool      `json:"latest,omitempty"`
}

type SendRequest struct {
	ConversationID string       `json:"conversation_id"`
	Text           string       `json:"text,omitempty"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	ShareSurfaceID string       `json:"share_surface_id,omitempty"`
	IdempotencyKey string       `json:"idempotency_key"`
}

type SendResult struct {
	MessageID string `json:"message_id,omitempty"`
	Verified  bool   `json:"verified"`
	Uncertain bool   `json:"uncertain"`
	Detail    string `json:"detail,omitempty"`
}

type Surface struct {
	ID               string           `json:"id"`
	Kind             string           `json:"kind"`
	Title            string           `json:"title,omitempty"`
	URL              string           `json:"url,omitempty"`
	AppID            string           `json:"app_id,omitempty"`
	Generation       string           `json:"generation,omitempty"`
	Screenshot       []byte           `json:"-"`
	ScreenshotSHA256 string           `json:"screenshot_sha256,omitempty"`
	OCRText          string           `json:"ocr_text,omitempty"`
	Elements         []SurfaceElement `json:"elements,omitempty"`
	Assets           []SurfaceAsset   `json:"assets,omitempty"`
	Viewport         *SurfaceViewport `json:"viewport,omitempty"`
	Actions          []Action         `json:"actions,omitempty"`
	ObservedAt       time.Time        `json:"observed_at"`
}

// Bounds is always rendered as an object in the public API. Its decoder also
// accepts the compact [x,y,width,height] form emitted by the UI companion.
type Bounds struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (b *Bounds) UnmarshalJSON(data []byte) error {
	type boundsAlias Bounds
	if len(data) > 0 && data[0] == '[' {
		var values []int
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		if len(values) != 4 {
			return fmt.Errorf("surface bounds require exactly four integers")
		}
		*b = Bounds{X: values[0], Y: values[1], Width: values[2], Height: values[3]}
		return nil
	}
	var value boundsAlias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*b = Bounds(value)
	return nil
}

type SurfaceElement struct {
	ID          string   `json:"id"`
	TargetID    string   `json:"target_id,omitempty"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Role        string   `json:"role,omitempty"`
	Bounds      Bounds   `json:"bounds"`
	Source      string   `json:"source"`
	Confidence  float64  `json:"confidence"`
	ActionID    string   `json:"action_id,omitempty"`
	ActionIDs   []string `json:"action_ids,omitempty"`
}

type SurfaceAsset struct {
	ID         string    `json:"id"`
	Token      string    `json:"token"`
	Kind       string    `json:"kind"`
	Label      string    `json:"label,omitempty"`
	Bounds     Bounds    `json:"bounds"`
	Source     string    `json:"source"`
	Confidence float64   `json:"confidence"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type SurfaceViewport struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type NamedSurface struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type SurfaceAssetExport struct {
	SurfaceID string `json:"surface_id"`
	AssetID   string `json:"asset_id"`
	Fidelity  string `json:"fidelity"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Data      []byte `json:"-"`
}

type Action struct {
	ID       string `json:"id"`
	TargetID string `json:"target_id,omitempty"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Risk     string `json:"risk,omitempty"`
	Effect   string `json:"effect,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type SurfaceAction struct {
	ActionID     string `json:"action_id"`
	Text         string `json:"text,omitempty"`
	TextProvided bool   `json:"-"`
	Confirmed    bool   `json:"confirmed,omitempty"`
}

// Driver owns exactly one active official-client runtime. The manager creates
// a new instance whenever a saved account is activated.
type Driver interface {
	Platform() Platform
	Start(context.Context, AccountRuntime) error
	Stop(context.Context) error
	Status(context.Context) (Status, error)
	AuthSnapshot(context.Context) (AuthSnapshot, error)
	SubmitAuthCode(context.Context, string) error
	ListConversations(context.Context, ConversationQuery) ([]Conversation, error)
	ReadMessages(context.Context, MessageQuery) ([]Message, error)
	Send(context.Context, SendRequest) (SendResult, error)
	OpenSurface(context.Context, string) (Surface, error)
	SnapshotSurface(context.Context, string) (Surface, error)
	ActSurface(context.Context, string, SurfaceAction) (Surface, error)
	CloseSurface(context.Context, string) error
}

// AuthActionDriver is deliberately separate from Driver. Only a user-held,
// one-time login page may invoke these tightly scoped onboarding operations;
// they are not part of the daemon API, CLI, or MCP surface.
type AuthActionDriver interface {
	PerformAuthAction(context.Context, AuthActionRequest) error
}

type Factory func(AccountRuntime) (Driver, error)

// AccountPurger is optional. Real drivers implement it to remove only the
// stopped, label-verified runtime that belongs to one saved account.
type AccountPurger interface {
	Purge(context.Context, AccountRuntime) error
}

// NamedSurfaceOpener is optional because only personal WeChat currently has a
// semantic launcher for a mini program that is not backed by a message card.
type NamedSurfaceOpener interface {
	OpenNamedSurface(context.Context, NamedSurface) (Surface, error)
}

// SurfaceAssetExporter exports a rendered crop from the exact latest surface
// snapshot. The opaque token is intentionally short-lived and generation-bound.
type SurfaceAssetExporter interface {
	ExportSurfaceAsset(context.Context, string, string) (SurfaceAssetExport, error)
}
