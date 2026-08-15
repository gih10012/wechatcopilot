package wechat

import (
	"errors"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

var (
	ErrNotStarted          = errors.New("wechat driver is not started")
	ErrAlreadyStarted      = errors.New("wechat driver is already started")
	ErrAuthRequired        = shared.NewFailure(shared.FailureAuthRequired, "wechat authentication is required")
	ErrClientIncompatible  = shared.NewFailure(shared.FailureClientIncompatible, "wechat client is incompatible")
	ErrIndexUnavailable    = errors.New("wechat local message index is unavailable")
	ErrConversationMissing = shared.NewFailure(shared.FailureNotFound, "wechat conversation was not found")
	ErrTargetAmbiguous     = shared.NewFailure(shared.FailureTargetAmbiguous, "wechat target is ambiguous")
	ErrSendUncertain       = shared.NewFailure(shared.FailureSendUncertain, "wechat send result could not be verified")
	ErrSurfaceMissing      = shared.NewFailure(shared.FailureNotFound, "wechat surface was not found")
	ErrActionStale         = shared.NewFailure(shared.FailureStale, "wechat surface action is stale or unknown")
	ErrUserActionRequired  = shared.NewFailure(shared.FailureUserActionRequired, "wechat surface action requires direct user interaction")
)
