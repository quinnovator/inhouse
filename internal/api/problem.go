// Package api exposes the engine over HTTPS on the control node: a small
// JSON API under /v1 (used by the inhouse CLI) and an MCP server at /mcp
// (used by agents). Both identify the caller with WhoIs and call the same
// engine methods, so behavior and permissions are identical.
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/quinnovator/inhouse/internal/authz"
	"github.com/quinnovator/inhouse/internal/engine"
	"github.com/quinnovator/inhouse/internal/store"
)

// Problem is the error body of every failed call.
type Problem struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Hint        string `json:"hint,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	status      int
}

func problem(err error) Problem {
	var conflict *store.Conflict
	switch {
	case errors.Is(err, authz.ErrForbidden):
		return Problem{Code: "permission_denied", Message: err.Error(), Hint: "Call whoami to see your grants; a grant in the tailnet policy file must cover this service, exposure and TTL.", status: http.StatusForbidden}
	case errors.Is(err, store.ErrNotFound):
		return Problem{Code: "not_found", Message: "not found", Hint: "Call list_services to see what exists.", status: http.StatusNotFound}
	case errors.As(err, &conflict) && conflict.OperationID != "":
		return Problem{Code: "service_busy", Message: conflict.Message, Hint: "Wait for operation " + conflict.OperationID + " (wait_for_operation), then retry.", OperationID: conflict.OperationID, status: http.StatusConflict}
	case errors.As(err, &conflict):
		return Problem{Code: "conflict", Message: conflict.Message, Hint: "Call get_service for current state.", status: http.StatusConflict}
	case engine.IsInvalid(err):
		return Problem{Code: "invalid_request", Message: err.Error(), Hint: "Fix the request and retry.", status: http.StatusBadRequest}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Problem{Code: "timeout", Message: "request timed out", status: http.StatusGatewayTimeout}
	default:
		return Problem{Code: "internal", Message: err.Error(), Hint: "Call get_events for this service to see what the platform recorded.", status: http.StatusInternalServerError}
	}
}
