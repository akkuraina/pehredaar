// Package launch tells people when an exam they asked about in the survey
// goes live, i.e. gets its first curated sources. The survey promises
// "I'll tell you when the others go live"; this keeps that promise, once.
//
// Who is told: people who chose the exam before it went live (the time is
// kept in exam_launches; SSC counts as live from the start), have a
// verified, active contact, and don't already watch any of its feeds.
// Someone who picks a live exam is subscribed by the survey instead.
//
// Each person gets one message covering every newly live exam, on each of
// their channels. Like alert deliveries, a notice is recorded before it
// is sent: a crash mid-send loses a message rather than repeating it.
package launch

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/exams"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Sender delivers a launch message on one channel. *notifier.Bot and
// *email.Sender implement it.
type Sender interface {
	Announce(ctx context.Context, to store.Recipient, l notifier.Launch) error
}

// Purpose binds a signed "start watching" link to exactly the exams in it,
// so a link for JEE can't be edited into one for NEET. examList is the
// comma-separated list as it appears in the URL ("jee,neet").
func Purpose(examList string) string { return "launch:" + examList }

// Path is the website path of the page that starts watching examIDs:
// /start/jee,neet/<signed subscriber id>.
func Path(signer *auth.Signer, subscriberID int64, examIDs []string) string {
	list := strings.Join(examIDs, ",")
	return "/start/" + list + "/" + signer.SignID(Purpose(list), subscriberID)
}

// Config wires an Announcer.
type Config struct {
	Store store.Store
	// Categories are the categories of enabled sources (sources.yaml):
	// an exam is live when one of them starts with its prefix.
	Categories []string
	Senders    map[store.Channel]Sender
	Signer     *auth.Signer
	BaseURL    string        // for the email's link
	Interval   time.Duration // how often to look; default 1h
	Logger     *slog.Logger
	Now        func() time.Time
}

// Announcer finds and tells the people waiting for an exam.
type Announcer struct {
	cfg Config
}

// New returns an Announcer.
func New(cfg Config) *Announcer {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Announcer{cfg: cfg}
}

// Run announces once at startup (a deploy is when exams go live), then
// every Interval: someone who confirms their email later is told then.
// It returns when ctx is done.
func (a *Announcer) Run(ctx context.Context) error {
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	for {
		if n, err := a.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.cfg.Logger.Error("launch: announcing", "err", err)
		} else if n > 0 {
			a.cfg.Logger.Info("launch: told people about newly live exams", "messages", n)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// RunOnce tells everyone currently waiting and returns how many messages
// went out.
func (a *Announcer) RunOnce(ctx context.Context) (int, error) {
	st := a.cfg.Store
	waiting := map[int64][]exams.Exam{}
	for _, e := range exams.Catalog {
		if !e.Live(a.cfg.Categories) {
			continue
		}
		liveAt, err := st.ExamLive(ctx, e.ID, a.cfg.Now())
		if err != nil {
			return 0, err
		}
		ids, err := st.LaunchCandidates(ctx, e.ID, liveAt, e.CategoryPrefix)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			waiting[id] = append(waiting[id], e)
		}
	}

	people := make([]int64, 0, len(waiting))
	for id := range waiting {
		people = append(people, id)
	}
	slices.Sort(people) // deterministic order: logs and tests read better

	sent := 0
	for _, id := range people {
		n, err := a.tell(ctx, id, waiting[id])
		sent += n
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// tell sends one person one message about exs on each reachable channel.
func (a *Announcer) tell(ctx context.Context, subID int64, exs []exams.Exam) (int, error) {
	st := a.cfg.Store
	sub, err := st.Subscriber(ctx, subID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil // deleted their account a moment ago
	}
	if err != nil {
		return 0, err
	}
	all, err := st.Contacts(ctx, subID)
	if err != nil {
		return 0, err
	}
	var to []store.Contact
	for _, c := range all {
		if c.Verified() && c.Active && a.cfg.Senders[c.Channel] != nil {
			to = append(to, c)
		}
	}
	if len(to) == 0 {
		// e.g. email only, but email isn't configured on this server.
		// Not marked: they're told once a channel is available.
		return 0, nil
	}

	var l notifier.Launch
	for _, e := range exs {
		first, err := st.MarkLaunchNotice(ctx, subID, e.ID)
		if err != nil {
			return 0, err
		}
		if first {
			l.ExamIDs = append(l.ExamIDs, e.ID)
			l.Names = append(l.Names, e.Name)
		}
	}
	if len(l.ExamIDs) == 0 {
		return 0, nil // another instance got there first
	}
	l.Lang = sub.Language
	l.Link = a.cfg.BaseURL + Path(a.cfg.Signer, subID, l.ExamIDs)

	sent := 0
	for _, c := range to {
		err := a.cfg.Senders[c.Channel].Announce(ctx, store.Recipient{Contact: c, Language: sub.Language}, l)
		switch {
		case err == nil:
			sent++
		case ctx.Err() != nil:
			return sent, ctx.Err()
		case errors.Is(err, notifier.ErrUnreachable):
			a.cfg.Logger.Info("launch: contact unreachable, deactivating", "contact", c.ID, "err", err)
			if err := st.SetContactActive(ctx, c.ID, false); err != nil {
				return sent, err
			}
		default:
			// Already marked, so not retried: one lost "now live" message
			// is better than a person getting it every hour.
			a.cfg.Logger.Warn("launch: sending", "contact", c.ID, "channel", c.Channel, "err", err)
		}
	}
	return sent, nil
}
