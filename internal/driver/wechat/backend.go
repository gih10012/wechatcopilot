package wechat

import (
	"context"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

const (
	continueSavedAccountLoginOperation = "continue_saved_account_login"
	savedAccountLoginActionPrefix      = continueSavedAccountLoginOperation + "."
)

func savedAccountLoginAction(generation string) shared.AuthAction {
	return shared.AuthAction{
		ID:                   savedAccountLoginActionPrefix + generation,
		Label:                "\u767b\u5f55\u5f53\u524d\u5fae\u4fe1\u8d26\u53f7",
		Risk:                 "high",
		Confirmation:         "\u8bf7\u786e\u8ba4\u4f7f\u7528\u5b98\u65b9\u5fae\u4fe1\u5ba2\u6237\u7aef\u663e\u793a\u7684\u5f53\u524d\u8d26\u53f7\u7ee7\u7eed\u767b\u5f55\u3002",
		RequiresConfirmation: true,
		ImageBound:           true,
		ReplayKey:            continueSavedAccountLoginOperation,
	}
}

type ProbeResult struct {
	State         shared.RuntimeState
	Identity      *shared.Identity
	Reason        string
	ClientVersion string
	AuthKind      shared.AuthKind
	Prompt        string
	CanSubmitCode bool
	Actions       []shared.AuthAction
	ScreenshotPNG []byte
	ObservedAt    time.Time
	QRBounds      *Rectangle
}

type Rectangle struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type UISendRequest struct {
	ConversationID string
	Title          string
	Locator        string
	Text           string
	Attachments    []string
	ShareLocator   string
}

type VisibleConversation struct {
	Title     string
	Kind      string
	Unread    int
	Locator   string
	Ambiguous bool
}

type VisibleMessage struct {
	Text            string
	Kind            string
	SenderName      string
	Outgoing        bool
	AccessibleLabel string
	SurfaceKind     string
	SurfaceLocator  string
	Confidence      float64
}

type VisibleMessages struct {
	ConversationTitle   string
	ConversationLocator string
	Messages            []VisibleMessage
}

type SurfaceTarget struct {
	Reference           string
	ConversationID      string
	ConversationTitle   string
	ConversationLocator string
	AccessibleLabel     string
	Kind                string
	SurfaceLocator      string
}

type BackendAction struct {
	Action   shared.Action
	ReplayID string
	Locator  string
}

type BackendSurface struct {
	Kind             string
	Title            string
	URL              string
	AppID            string
	Generation       string
	ScreenshotSHA256 string
	WindowIdentity   string
	SemanticText     string
	Screenshot       []byte
	Elements         []shared.SurfaceElement
	Assets           []shared.SurfaceAsset
	Viewport         *shared.SurfaceViewport
	Actions          []BackendAction
}

// Backend is the narrow privileged boundary around the official desktop
// client. Implementations must accept semantic targets, never raw coordinates.
type Backend interface {
	Start(context.Context, Profile) error
	Stop(context.Context) error
	Probe(context.Context) (ProbeResult, error)
	Screenshot(context.Context) ([]byte, error)
	SubmitAuthCode(context.Context, string) error
	ContinueSavedAccountLogin(context.Context, string) error
	ListVisibleConversations(context.Context) ([]VisibleConversation, error)
	ReadVisibleMessages(context.Context, string, string) (VisibleMessages, error)
	Send(context.Context, UISendRequest) error
	OpenSurface(context.Context, SurfaceTarget) (BackendSurface, error)
	OpenNamedSurface(context.Context, string, string) (BackendSurface, error)
	SnapshotSurface(context.Context, string) (BackendSurface, error)
	ActSurface(context.Context, string, string, string) (BackendSurface, error)
	CloseSurface(context.Context, string) error
}

// MessageIndex is populated by a version-specific, read-only WCDB adapter.
// Keeping that adapter outside UI automation makes unsupported client versions
// fail closed while login and diagnostics remain available.
type MessageIndex interface {
	Available(context.Context) bool
	ListConversations(context.Context, shared.ConversationQuery) ([]shared.Conversation, error)
	ReadMessages(context.Context, shared.MessageQuery) ([]shared.Message, error)
	Conversation(context.Context, string) (shared.Conversation, error)
	ResolveSurface(context.Context, string) (SurfaceTarget, error)
	WaitOutgoing(context.Context, OutgoingMatch) (shared.Message, error)
}

type OutgoingMatch struct {
	ConversationID  string
	Text            string
	AttachmentCount int
	NotBefore       time.Time
	Timeout         time.Duration
}

// OutgoingBaselinePreparer lets UI-derived indexes snapshot visible messages
// before a mutating send. WCDB-backed indexes do not need this hook.
type OutgoingBaselinePreparer interface {
	PrepareOutgoing(context.Context, OutgoingMatch) error
}

type UnavailableIndex struct{}

func (UnavailableIndex) Available(context.Context) bool { return false }
func (UnavailableIndex) ListConversations(context.Context, shared.ConversationQuery) ([]shared.Conversation, error) {
	return nil, ErrIndexUnavailable
}
func (UnavailableIndex) ReadMessages(context.Context, shared.MessageQuery) ([]shared.Message, error) {
	return nil, ErrIndexUnavailable
}
func (UnavailableIndex) Conversation(context.Context, string) (shared.Conversation, error) {
	return shared.Conversation{}, ErrIndexUnavailable
}
func (UnavailableIndex) ResolveSurface(context.Context, string) (SurfaceTarget, error) {
	return SurfaceTarget{}, ErrIndexUnavailable
}
func (UnavailableIndex) WaitOutgoing(context.Context, OutgoingMatch) (shared.Message, error) {
	return shared.Message{}, ErrIndexUnavailable
}
