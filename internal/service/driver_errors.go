package service

import (
	"net/http"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/driver"
)

func classifiedDriverError(err error) *api.AppError {
	kind, ok := driver.ClassifyFailure(err)
	if !ok {
		return nil
	}
	status := http.StatusConflict
	code := api.CodeConflict
	message := "official client operation could not continue"
	switch kind {
	case driver.FailureAuthRequired:
		code = api.CodeAuthRequired
		message = "official client authentication is required"
	case driver.FailureClientIncompatible:
		code = api.CodeClientIncompatible
		message = "official client version or UI is incompatible"
	case driver.FailureTargetAmbiguous:
		code = api.CodeTargetAmbiguous
		message = "official client target is ambiguous"
	case driver.FailureUnsupported:
		code = api.CodeUnsupportedCapability
		message = "the active driver does not support this capability"
	case driver.FailureUserActionRequired:
		code = api.CodeUserActionRequired
		message = "this step requires direct user interaction"
	case driver.FailureNotFound:
		status = http.StatusNotFound
		code = api.CodeNotFound
		message = "official client target was not found"
	case driver.FailureStale:
		message = "official client state or action is stale; take a new snapshot"
	case driver.FailureDriverUnavailable:
		status = http.StatusServiceUnavailable
		code = api.CodeDriverUnavailable
		message = "official client driver is unavailable"
	case driver.FailureSendUncertain:
		code = api.CodeSendUncertain
		message = "official client could not verify the sent message"
	}
	return api.WrapError(status, code, message, err)
}
