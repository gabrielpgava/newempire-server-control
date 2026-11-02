// Package updates verifies official releases and manages independent panel updates.
package updates

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

var (
	ErrInvalid      = errors.New("invalid update request")
	ErrConflict     = errors.New("update conflict")
	ErrVerification = errors.New("release verification failed")
	ErrUpstream     = errors.New("release service unavailable")
	ErrUnavailable  = errors.New("updater unavailable")
	ErrRecovery     = errors.New("manual panel recovery required")
)

func problem(kind error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", kind, fmt.Sprintf(format, args...))
}

// HTTPStatus classifies domain errors without depending on an HTTP handler.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrVerification), errors.Is(err, ErrUpstream):
		return http.StatusBadGateway
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
