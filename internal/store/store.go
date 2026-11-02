// Package store defines Pehredaar's domain types and the Store interface
// that persists them. Concrete implementations live in subpackages
// (store/sqlite today, store/postgres later) so that the rest of the code
// depends only on this package and never on a specific database.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a looked-up row does not exist. Callers test
// for it with errors.Is, which also works if an implementation wraps it.
var ErrNotFound = errors.New("store: not found")

// Kind is what sort of thing a source is, which decides how it is normalized.
type Kind string

const (
	KindFeed Kind = "feed" // JSON listing of notices (e.g. SSC notice board)
	KindPDF  Kind = "pdf"  // a single document, usually discovered from a feed
	KindHTML Kind = "html" // a plain web page (unused for SSC)
)

// Language is the language of a source's content, or a subscriber's
// preferred alert language.
type Language string

const (
	LangEnglish Language = "en"
	LangHindi   Language = "hi"
)

// Source is one URL Pehredaar watches, plus the HTTP state needed to poll it
// politely. The curated fields (URL..Enabled) come from sources.yaml or from
// feed discovery; the rest is updated by the scheduler after each check.
type Source struct {
	ID            int64
	URL           string
	Kind          Kind
	Category      string // subscription unit, e.g. "ssc:notice-board"
	Language      Language
	Label         string
	ParentID      int64 // feed this PDF was discovered from; 0 for curated sources
	CheckInterval time.Duration
	Enabled       bool

	ETag                string // HTTP validators for conditional GET
	LastModified        string
	LastCheckedAt       time.Time // zero if never checked
	NextCheckAt         time.Time // zero means "due now"
	ConsecutiveFailures int
	LastError           string
	CreatedAt           time.Time
}

// CheckResult is what the scheduler records after polling a source.
type CheckResult struct {
	CheckedAt   time.Time
	NextCheckAt time.Time
	// Err is non-empty when the check failed. On failure the HTTP validators
	// are left untouched and ConsecutiveFailures is incremented; on success
	// the validators are replaced and the failure count resets.
	Err          string
	ETag         string
	LastModified string
}

// Snapshot is the normalized text of a source at one point in time. A new
// snapshot is stored only when the content hash changes.
type Snapshot struct {
	ID          int64
	SourceID    int64
	ContentHash string
	Text        string
	FetchedAt   time.Time
}

// ChangeKind describes what happened between two snapshots.
type ChangeKind string

const (
	ChangeAdded   ChangeKind = "added"
	ChangeEdited  ChangeKind = "edited"
	ChangeRemoved ChangeKind = "removed"
)

// Importance is the summarizer's classification of a change.
type Importance string

const (
	ImportanceUnknown     Importance = "" // not yet classified
	ImportanceDate        Importance = "date"
	ImportanceEligibility Importance = "eligibility"
	ImportanceFee         Importance = "fee"
	ImportanceOther       Importance = "other"
	ImportanceCosmetic    Importance = "cosmetic"
)

// ChangeStatus tracks a change through the summarize → notify stages.
type ChangeStatus string

const (
	StatusPending    ChangeStatus = "pending"    // detected, not yet summarized
	StatusSummarized ChangeStatus = "summarized" // summarized, not yet fully notified
	StatusNotified   ChangeStatus = "notified"
	StatusSuppressed ChangeStatus = "suppressed" // cosmetic; never alerted
	StatusFailed     ChangeStatus = "failed"
)

// Change is a detected, meaningful difference between two snapshots.
type Change struct {
	ID            int64
	SourceID      int64
	OldSnapshotID int64 // 0 if there was no previous snapshot
	NewSnapshotID int64
	Kind          ChangeKind
	Diff          string
	Importance    Importance
	Status        ChangeStatus
	Attempts      int
	CreatedAt     time.Time
}

// Subscriber is a Telegram user who has talked to the bot.
type Subscriber struct {
	ID         int64
	TelegramID int64
	ChatID     int64
	Language   Language // alert language
	Active     bool     // false once the user blocks the bot
	CreatedAt  time.Time
}

// Target is what a subscription points at: exactly one of a single source
// or a whole category. v1 subscriptions are category-level.
type Target struct {
	SourceID int64
	Category string
}

// Validate reports whether exactly one field of t is set.
func (t Target) Validate() error {
	if (t.SourceID == 0) == (t.Category == "") {
		return fmt.Errorf("store: target must set exactly one of SourceID or Category, got %+v", t)
	}
	return nil
}

// Subscription links a subscriber to a target.
type Subscription struct {
	ID           int64
	SubscriberID int64
	Target       Target
	CreatedAt    time.Time
}

// Store persists everything Pehredaar knows. Implementations must be safe
// for concurrent use by multiple goroutines.
//
// Go note: idiomatic Go usually prefers small interfaces declared by their
// consumers. This one is deliberately broad because it is the seam for
// swapping SQLite for Postgres; consumers that need only a slice of it (the
// bot, say) can declare their own narrower interface, and *sqlite.Store will
// satisfy it automatically.
type Store interface {
	// Sources.

	// UpsertSource inserts a source, or updates the curated fields of the
	// existing source with the same URL. Polling state is never overwritten.
	UpsertSource(ctx context.Context, s Source) (Source, error)
	Source(ctx context.Context, id int64) (Source, error)
	// Sources lists every source, enabled or not, ordered by id (so curated
	// sources come before the PDFs discovered from them).
	Sources(ctx context.Context) ([]Source, error)
	// DueSources returns enabled sources whose next check is at or before now,
	// most overdue first.
	DueSources(ctx context.Context, now time.Time, limit int) ([]Source, error)
	RecordCheck(ctx context.Context, sourceID int64, r CheckResult) error
	// DisableCuratedExcept disables every curated (non-discovered) source whose
	// URL is not in keepURLs, used when syncing sources.yaml. It returns how
	// many sources were disabled.
	DisableCuratedExcept(ctx context.Context, keepURLs []string) (int64, error)
	// Categories lists the distinct categories of enabled sources, sorted.
	Categories(ctx context.Context) ([]string, error)

	// Snapshots and changes.

	// LatestSnapshot returns ErrNotFound if the source has no snapshots yet.
	LatestSnapshot(ctx context.Context, sourceID int64) (Snapshot, error)
	Snapshot(ctx context.Context, id int64) (Snapshot, error)
	// Snapshots lists a source's snapshots newest first. Text is left empty:
	// listings don't need it and a PDF's text can be large.
	Snapshots(ctx context.Context, sourceID int64, limit int) ([]Snapshot, error)
	// SaveSnapshot stores snap and, if change is non-nil, a change pointing
	// at it, in a single transaction: either both are saved or neither is.
	// The returned change (nil if none was given) has its ID and
	// NewSnapshotID filled in.
	SaveSnapshot(ctx context.Context, snap Snapshot, change *Change) (Snapshot, *Change, error)
	// UnfinishedChanges returns changes still pending or summarized, oldest first.
	UnfinishedChanges(ctx context.Context, limit int) ([]Change, error)
	Change(ctx context.Context, id int64) (Change, error)
	// RecentChanges returns changes newest first; sourceID 0 means all sources.
	RecentChanges(ctx context.Context, sourceID int64, limit int) ([]Change, error)
	// UpdateChange saves a change's importance, status and attempts.
	UpdateChange(ctx context.Context, c Change) error
	SaveSummary(ctx context.Context, changeID int64, lang Language, text string) error
	// Summary returns ErrNotFound if no summary exists for that language yet.
	Summary(ctx context.Context, changeID int64, lang Language) (string, error)

	// Subscribers and subscriptions.

	// UpsertSubscriber registers a Telegram user, or reactivates and updates
	// the chat ID of an existing one.
	UpsertSubscriber(ctx context.Context, telegramID, chatID int64) (Subscriber, error)
	SetLanguage(ctx context.Context, telegramID int64, lang Language) error
	SetActive(ctx context.Context, telegramID int64, active bool) error
	// Subscribe is idempotent: subscribing twice to the same target is not an
	// error. It returns ErrNotFound if the subscriber does not exist.
	Subscribe(ctx context.Context, telegramID int64, t Target) error
	// Unsubscribe reports whether a subscription was actually removed.
	Unsubscribe(ctx context.Context, telegramID int64, t Target) (bool, error)
	Subscriptions(ctx context.Context, telegramID int64) ([]Subscription, error)
	// SubscribersFor returns active subscribers whose subscriptions cover src:
	// its category, the source itself, or (for discovered PDFs) its parent.
	// Language routing is the caller's job; the store does not filter on it.
	SubscribersFor(ctx context.Context, src Source) ([]Subscriber, error)

	// Deliveries.

	// MarkDelivered records that a change was sent to a subscriber. It
	// returns false if it had already been recorded, so callers send only
	// when it returns true.
	MarkDelivered(ctx context.Context, changeID, subscriberID int64) (bool, error)

	Close() error
}
