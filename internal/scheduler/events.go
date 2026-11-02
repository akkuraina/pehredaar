package scheduler

import (
	"slices"
	"time"
)

// Outcome is what one check concluded.
type Outcome string

const (
	OutcomeNotModified Outcome = "not-modified" // server said 304
	OutcomeUnchanged   Outcome = "unchanged"    // fetched, same canonical hash
	OutcomeBaseline    Outcome = "baseline"     // first snapshot; never alerts
	OutcomeNoise       Outcome = "noise"        // text differed, but only as noise
	OutcomeChanged     Outcome = "changed"      // a change was recorded
	OutcomeFailed      Outcome = "failed"
	OutcomeCancelled   Outcome = "cancelled" // interrupted by shutdown
)

// Event describes one check, for the dashboard's activity log and for logs.
// Events live in memory only: they are a window into what the scheduler
// is doing, not a record anyone depends on.
type Event struct {
	Time     time.Time
	SourceID int64
	Label    string
	Outcome  Outcome
	Detail   string
	ChangeID int64 // set when Outcome is OutcomeChanged
	Duration time.Duration
}

func (s *Scheduler) record(ev Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	if len(s.events) > maxEvents {
		// Drop the oldest. Copying 300 small structs now and then is cheaper
		// to reason about than a true circular buffer.
		s.events = slices.Delete(s.events, 0, len(s.events)-maxEvents)
	}
	s.mu.Unlock()

	attrs := []any{"source", ev.SourceID, "label", ev.Label, "outcome", ev.Outcome,
		"took", ev.Duration.Round(time.Millisecond)}
	if ev.Detail != "" {
		attrs = append(attrs, "detail", ev.Detail)
	}
	if ev.Outcome == OutcomeFailed {
		s.log.Warn("check", attrs...)
	} else {
		s.log.Info("check", attrs...)
	}
}

// Events returns recent checks, newest first. The slice is a copy, so the
// caller may keep it while checks continue.
func (s *Scheduler) Events() []Event {
	s.mu.Lock()
	out := slices.Clone(s.events)
	s.mu.Unlock()
	slices.Reverse(out)
	return out
}

// Running returns the IDs of sources being checked right now.
func (s *Scheduler) Running() map[int64]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]bool, len(s.inFlight))
	for id := range s.inFlight {
		out[id] = true
	}
	return out
}
