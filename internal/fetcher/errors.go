package fetcher

import (
	"errors"
	"fmt"
	"time"
)

// ErrorKind tells the scheduler how to react to a failed fetch.
type ErrorKind int

const (
	// Temporary failures (timeouts, 5xx, 429) should be retried with backoff.
	Temporary ErrorKind = iota + 1
	// Permanent failures (404, 410, oversized body) won't fix themselves soon;
	// repeated ones should disable the source.
	Permanent
	// Blocked means the site refused us (401/403). Back off for a long time
	// and tell a human; never hammer a site that is blocking us.
	Blocked
	// Disallowed means robots.txt forbids the URL. We did not send the request.
	Disallowed
)

func (k ErrorKind) String() string {
	switch k {
	case Temporary:
		return "temporary"
	case Permanent:
		return "permanent"
	case Blocked:
		return "blocked"
	case Disallowed:
		return "disallowed"
	}
	return fmt.Sprintf("ErrorKind(%d)", int(k))
}

// Error is returned for every failed fetch except context cancellation,
// which is returned unwrapped so shutdown is easy to recognize.
type Error struct {
	Kind       ErrorKind
	URL        string
	StatusCode int           // 0 if no HTTP response was received
	RetryAfter time.Duration // server-requested wait, if any
	Err        error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("fetch %s: %s", e.URL, e.Kind)
	if e.StatusCode != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(", retry after %v", e.RetryAfter)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap lets errors.Is / errors.As see the underlying cause.
func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the ErrorKind of err, or 0 if err is not a *fetcher.Error.
//
// Go note: errors.As walks the chain of wrapped errors looking for one of the
// target's type, so this works even if a caller wrapped our error further.
func KindOf(err error) ErrorKind {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Kind
	}
	return 0
}
