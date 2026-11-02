// Package summarizer turns a stored change into the short text sent to
// subscribers, plus an importance class.
//
// Template is the implementation used until the LLM summarizer exists
// (milestone 5), and stays afterwards as the fallback when the LLM is down:
// users must never miss an alert because a provider had an outage.
package summarizer

import (
	"context"

	"github.com/akkuraina/pehredaar/internal/store"
)

// Input is everything a summarizer needs about one change.
type Input struct {
	Change store.Change
	Source store.Source
	Lang   store.Language // language to write the summary in
}

// Summary is a summarizer's output.
type Summary struct {
	Text       string // plain text, no markup; the notifier escapes it
	Importance store.Importance
}

// Summarizer summarizes changes. Implementations must be safe for
// concurrent use.
type Summarizer interface {
	Summarize(ctx context.Context, in Input) (Summary, error)
}
