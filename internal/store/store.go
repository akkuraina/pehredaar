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

// DisabledReason records why a source is no longer polled. Disabled
// sources are kept, not deleted, so their snapshots and changes survive.
type DisabledReason string

const (
	// DisabledRemoved: a curated source no longer in sources.yaml.
	DisabledRemoved DisabledReason = "removed"
	// DisabledGone: the URL answered 404/410 too many times in a row.
	DisabledGone DisabledReason = "gone"
	// DisabledNeedsOCR: a PDF with no extractable text (a scan). Polling it
	// again would only fail again.
	DisabledNeedsOCR DisabledReason = "needs-ocr"
	// DisabledRetired: a discovered PDF past its watch period.
	DisabledRetired DisabledReason = "retired"
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
	// DisabledReason says why a source stopped being polled. It is empty
	// while Enabled is true.
	DisabledReason DisabledReason

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

// Subscriber is a person. They are reached through one or more Contacts
// and their subscriptions apply to all of them.
type Subscriber struct {
	ID        int64
	Language  Language // alert and site language
	CreatedAt time.Time
}

// Channel is a way of reaching a subscriber.
type Channel string

const (
	ChannelTelegram Channel = "telegram"
	ChannelEmail    Channel = "email"
)

// Contact is one way to reach a subscriber: a Telegram account or an email
// address. Alerts go to every contact that is both verified and active.
type Contact struct {
	ID           int64
	SubscriberID int64
	Channel      Channel
	// Address is the Telegram user id (as text) or the lower-cased email.
	Address    string
	ChatID     int64     // Telegram only
	VerifiedAt time.Time // zero until an email address is confirmed
	// Active is false after the user blocked the bot, unsubscribed, or
	// the address bounced.
	Active    bool
	CreatedAt time.Time
}

// Verified reports whether the contact has been confirmed.
func (c Contact) Verified() bool { return !c.VerifiedAt.IsZero() }

// Recipient is a contact that should get an alert, with the language of
// the person behind it.
type Recipient struct {
	Contact
	Language Language
}

// TokenPurpose is what a one-time token is for.
type TokenPurpose string

const (
	TokenVerifyEmail  TokenPurpose = "verify-email"  // confirm an address, then sign in
	TokenLogin        TokenPurpose = "login"         // sign in from an emailed or Telegram link
	TokenLinkTelegram TokenPurpose = "link-telegram" // attach a Telegram account (bot deep link)
)

// Token is a one-time token. The store only ever sees its hash.
type Token struct {
	Hash         string
	Purpose      TokenPurpose
	SubscriberID int64
	ContactID    int64 // the contact being verified, if any
	ExpiresAt    time.Time
}

// FeedQuery selects changes for the website feed. Cosmetic, suppressed
// and failed changes are never included.
type FeedQuery struct {
	Categories []string // empty: all categories
	AfterID    int64    // only changes with a larger id (live updates)
	BeforeID   int64    // only changes with a smaller id (paging); 0: no limit
	Lang       Language // summary language; falls back to English
	Limit      int
}

// FeedEntry is one change as the website shows it.
type FeedEntry struct {
	Change  Change
	Source  Source
	Summary string // empty while the change is still pending
}

// Stats are the public numbers shown on the landing page.
type Stats struct {
	Sources       int       // enabled sources, feeds and PDFs
	Feeds         int       // enabled curated sources
	ChangesCaught int       // changes that weren't cosmetic
	LastCheckAt   time.Time // most recent check of any source
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
	// Enabling a curated source clears its DisabledReason (re-syncing
	// sources.yaml retries it), but a discovered source the scheduler
	// disabled stays disabled when its feed lists it again; otherwise
	// discovery would undo retirement every time the feed changed.
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
	// DisableSource stops polling one source and records why.
	DisableSource(ctx context.Context, id int64, reason DisabledReason) error
	// RetireDiscovered disables discovered (parent_id set) sources created
	// before cutoff and returns how many it disabled.
	RetireDiscovered(ctx context.Context, cutoff time.Time) (int64, error)
	// Reschedule sets when a source is next due, leaving all else alone.
	Reschedule(ctx context.Context, id int64, next time.Time) error
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

	// FeedChanges returns changes for the website, newest first.
	FeedChanges(ctx context.Context, q FeedQuery) ([]FeedEntry, error)
	Stats(ctx context.Context) (Stats, error)

	// Subscribers and contacts.

	// CreateSubscriber adds a person with no contacts yet (web sign-up).
	CreateSubscriber(ctx context.Context, lang Language) (Subscriber, error)
	Subscriber(ctx context.Context, id int64) (Subscriber, error)
	// TelegramSubscriber returns the subscriber behind a Telegram account,
	// creating both if it is new. It reactivates the contact and updates
	// its chat ID, since talking to the bot undoes a block.
	TelegramSubscriber(ctx context.Context, telegramID, chatID int64) (Subscriber, error)
	// LinkTelegram attaches a Telegram account to subscriberID. If that
	// account already belonged to another subscriber (they used the bot
	// before signing up on the web), that subscriber is merged into this
	// one: contacts, subscriptions and interests move over.
	LinkTelegram(ctx context.Context, subscriberID, telegramID, chatID int64) error
	// AddEmail adds an unverified email contact. If the address is already
	// known it returns the existing contact, whoever it belongs to.
	AddEmail(ctx context.Context, subscriberID int64, email string) (Contact, error)
	Contact(ctx context.Context, id int64) (Contact, error)
	// ContactByAddress returns ErrNotFound if nobody has that address.
	ContactByAddress(ctx context.Context, ch Channel, address string) (Contact, error)
	Contacts(ctx context.Context, subscriberID int64) ([]Contact, error)
	VerifyContact(ctx context.Context, contactID int64) error
	SetContactActive(ctx context.Context, contactID int64, active bool) error
	SetLanguage(ctx context.Context, subscriberID int64, lang Language) error
	// DeleteSubscriber removes a person and everything about them:
	// contacts, subscriptions, interests, tokens and delivery records.
	DeleteSubscriber(ctx context.Context, id int64) error

	// Subscriptions and interests.

	// Subscribe is idempotent: subscribing twice to the same target is not an
	// error. It returns ErrNotFound if the subscriber does not exist.
	Subscribe(ctx context.Context, subscriberID int64, t Target) error
	// Unsubscribe reports whether a subscription was actually removed.
	Unsubscribe(ctx context.Context, subscriberID int64, t Target) (bool, error)
	Subscriptions(ctx context.Context, subscriberID int64) ([]Subscription, error)
	// SetInterests replaces the exams a subscriber wants watched.
	SetInterests(ctx context.Context, subscriberID int64, exams []string) error
	Interests(ctx context.Context, subscriberID int64) ([]string, error)
	// RecipientsFor returns the verified, active contacts of subscribers
	// whose subscriptions cover src: its category, the source itself, or
	// (for discovered PDFs) its parent. Language routing is the caller's job.
	RecipientsFor(ctx context.Context, src Source) ([]Recipient, error)

	// Tokens and settings.

	CreateToken(ctx context.Context, t Token) error
	// ConsumeToken marks an unexpired, unused token as used and returns it.
	// It returns ErrNotFound for unknown, expired, used or wrong-purpose
	// tokens, without saying which: the caller shouldn't either.
	ConsumeToken(ctx context.Context, hash string, purpose TokenPurpose, now time.Time) (Token, error)
	// Setting returns a stored value, storing init first if the key is new.
	// Concurrent callers all get the same value.
	Setting(ctx context.Context, key, init string) (string, error)

	// Deliveries.

	// MarkDelivered records that a change was sent to a contact. It
	// returns false if it had already been recorded, so callers send only
	// when it returns true.
	MarkDelivered(ctx context.Context, changeID, contactID int64) (bool, error)

	Close() error
}
