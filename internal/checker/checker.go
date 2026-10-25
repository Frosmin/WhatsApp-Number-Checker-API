package checker

import (
	"context"
	"errors"
	"time"
)

// Checker separates HTTP handling from the WhatsApp connection.
type Checker interface {
	Ready() bool
	Check(context.Context, string) (bool, error)
}

var (
	ErrUnavailable     = errors.New("whatsapp unavailable")
	ErrInvalidResponse = errors.New("incomplete or ambiguous whatsapp response")
)

// RateLimitError carries an upstream wait, without retrying the query.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return "whatsapp rate limited" }
