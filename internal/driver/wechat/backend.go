package wechat

import (
	"context"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

type ProbeResult struct {
	State         shared.RuntimeState
	Identity      *shared.Identity
	Reason        string
	ClientVersion string
	AuthKind      shared.AuthKind
	Prompt        string
	CanSubmitCode bool
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
}

type BackendAction struct {
	Action  shared.Action
	Locator string
}

type BackendSurface struct {
	Kind         string
	Title        string
	URL          string
	AppID        string
	SemanticText string
	Screenshot   []byte
	Actions      []BackendAction
}

// Backend is the narrow privileged boundary around the official desktop
// client. Implementations must accept semantic targets, never raw coordinates.
type Backend interface {
	Start(context.Context, Profile) error
	Stop(context.Context) error
	Probe(context.Context) (ProbeResult, error)
	Screenshot(context.Context) ([]byte, error)
	SubmitAuthCode(context.Context, string) error
	ListVisibleConversations(context.Context) ([]VisibleConversation, error)
	ReadVisibleMessages(context.Context, string, string) (VisibleMessages, error)
	Send(context.Context, UISendRequest) error
	OpenSurface(context.Context, SurfaceTarget) (BackendSurface, error)
	SnapshotSurface(context.Context) (BackendSurface, error)
	ActSurface(context.Context, string, string) (BackendSurface, error)
	CloseSurface(context.Context) error
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
