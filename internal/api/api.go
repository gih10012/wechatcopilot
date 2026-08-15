package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const SchemaVersion = "1"

const (
	CodeInvalidArgument       = "INVALID_ARGUMENT"
	CodeNotFound              = "NOT_FOUND"
	CodeConflict              = "CONFLICT"
	CodeDaemonUnavailable     = "DAEMON_UNAVAILABLE"
	CodeAuthRequired          = "AUTH_REQUIRED"
	CodeAuthExpired           = "AUTH_EXPIRED"
	CodeAccountInactive       = "ACCOUNT_INACTIVE"
	CodeDriverUnavailable     = "DRIVER_UNAVAILABLE"
	CodeClientIncompatible    = "CLIENT_INCOMPATIBLE"
	CodeTargetAmbiguous       = "TARGET_AMBIGUOUS"
	CodeUnsupportedCapability = "UNSUPPORTED_CAPABILITY"
	CodeConfirmationRequired  = "CONFIRMATION_REQUIRED"
	CodeSendUncertain         = "SEND_UNCERTAIN"
	CodePartialFailure        = "PARTIAL_FAILURE"
	CodeUserActionRequired    = "USER_ACTION_REQUIRED"
	CodeInternal              = "INTERNAL"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type AppError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
	Cause   error
}

func (e *AppError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

func (e *AppError) Unwrap() error { return e.Cause }

func NewError(status int, code, message string) *AppError {
	return &AppError{Status: status, Code: code, Message: message}
}

func WrapError(status int, code, message string, cause error) *AppError {
	return &AppError{Status: status, Code: code, Message: message, Cause: cause}
}

type Response struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Data          any    `json:"data,omitempty"`
	Error         *Error `json:"error,omitempty"`
}

func Success(data any) Response {
	return Response{SchemaVersion: SchemaVersion, OK: true, Data: data}
}

func Failure(err error) (Response, int) {
	var appErr *AppError
	if errors.As(err, &appErr) {
		status := appErr.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return Response{
			SchemaVersion: SchemaVersion,
			OK:            false,
			Error: &Error{
				Code:    appErr.Code,
				Message: appErr.Message,
				Details: appErr.Details,
			},
		}, status
	}
	return Response{
		SchemaVersion: SchemaVersion,
		OK:            false,
		Error:         &Error{Code: CodeInternal, Message: "internal error"},
	}, http.StatusInternalServerError
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func DecodeJSON(r *http.Request, dst any) error {
	const maximumBodyBytes = 2 << 20
	limited := &io.LimitedReader{R: r.Body, N: maximumBodyBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return WrapError(http.StatusBadRequest, CodeInvalidArgument, "invalid JSON body", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return WrapError(http.StatusBadRequest, CodeInvalidArgument, "JSON body must contain exactly one value", err)
	}
	if limited.N == 0 {
		return NewError(http.StatusRequestEntityTooLarge, CodeInvalidArgument, "JSON body exceeds 2 MiB")
	}
	return nil
}
