// Package outbox is the second stage of the pipeline: it turns stored
// changes into alerts. The scheduler only writes changes (status pending);
// this package summarizes them, finds who to tell, and sends.
//
// This is the "outbox pattern" (docs/ARCHITECTURE.md §7): because a change
// is committed before any LLM or Telegram call, a crash or an outage only
// delays an alert, and the deliveries table makes sure a restart never
// sends one twice.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/differ"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/summarizer"
)

// Sender delivers one alert to one contact on its channel. *notifier.Bot
// (Telegram) and *email.Sender implement it. A sender returns an error
// wrapping notifier.ErrUnreachable when the contact can never be reached
// again, and the processor deactivates it.
type Sender interface {
	Deliver(ctx context.Context, to store.Recipient, a notifier.Alert) error
}

// Config holds the processor's dependencies.
type Config struct {
	Store      store.Store
	Summarizer summarizer.Summarizer
	// Fallback is used once Summarizer has failed MaxAttempts times for a
	// change; default summarizer.Template{}. A late templated alert beats
	// a missed date change.
	Fallback summarizer.Summarizer
	// Senders by channel. A channel without a sender is skipped, so alerts
	// still flow on the others (say, email without a Resend key).
	Senders    map[store.Channel]Sender
	Categories []config.Category // alert titles
	Poll       time.Duration     // how often to look for work; default 15s
	Logger     *slog.Logger
}

// Processor drains unfinished changes. Run it in one goroutine.
type Processor struct {
	store    store.Store
	sum      summarizer.Summarizer
	fallback summarizer.Summarizer
	senders  map[store.Channel]Sender
	titles   map[string]string // category → label
	poll     time.Duration
	log      *slog.Logger
	now      func() time.Time
}

const (
	// MaxAttempts is how often the summarizer may fail on one change
	// before the fallback is used.
	MaxAttempts = 3

	// MaxAge: a change that waited longer than this to be processed (say
	// the bot was off for a week) is closed without alerting, so turning
	// the bot back on doesn't flood users with stale news.
	MaxAge = 72 * time.Hour

	batchSize = 50
	maxLinks  = 3
)

// New returns a Processor with defaults filled in.
func New(cfg Config) *Processor {
	p := &Processor{
		store:    cfg.Store,
		sum:      cfg.Summarizer,
		fallback: cfg.Fallback,
		senders:  cfg.Senders,
		titles:   make(map[string]string, len(cfg.Categories)),
		poll:     cfg.Poll,
		log:      cfg.Logger,
		now:      time.Now,
	}
	for _, c := range cfg.Categories {
		p.titles[c.ID] = c.Label
	}
	if p.sum == nil {
		p.sum = summarizer.Template{}
	}
	if p.fallback == nil {
		p.fallback = summarizer.Template{}
	}
	if p.poll <= 0 {
		p.poll = 15 * time.Second
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

// Run processes changes every Poll until ctx is cancelled.
func (p *Processor) Run(ctx context.Context) error {
	t := time.NewTicker(p.poll)
	defer t.Stop()
	for {
		if err := p.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ProcessOnce handles every unfinished change once, oldest first. A change
// that fails is logged and left for the next round; it doesn't block the
// ones after it.
func (p *Processor) ProcessOnce(ctx context.Context) error {
	changes, err := p.store.UnfinishedChanges(ctx, batchSize)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if ctx.Err() != nil {
			return nil
		}
		if err := p.process(ctx, ch); err != nil && ctx.Err() == nil {
			p.log.Warn("outbox: change not finished, will retry", "change", ch.ID, "err", err)
		}
	}
	return nil
}

func (p *Processor) process(ctx context.Context, ch store.Change) error {
	if age := p.now().Sub(ch.CreatedAt); age > MaxAge {
		p.log.Warn("outbox: change too old to alert, closing", "change", ch.ID, "age", age.Round(time.Hour))
		return p.finish(ctx, ch, store.StatusNotified)
	}
	src, err := p.store.Source(ctx, ch.SourceID)
	if err != nil {
		return err
	}
	all, err := p.store.RecipientsFor(ctx, src)
	if err != nil {
		return err
	}
	recips := route(all, src)

	// One summary per language needed, cached in the store so a retry
	// after a crash doesn't pay for the LLM twice. English is always made,
	// even with nobody to alert: the website's feed shows every change,
	// and the summary is also where "cosmetic" gets decided.
	texts := make(map[store.Language]string)
	for _, lang := range languages(recips) {
		text, err := p.summary(ctx, &ch, src, lang)
		if err != nil {
			return err
		}
		if ch.Importance == store.ImportanceCosmetic {
			p.log.Info("outbox: cosmetic change suppressed", "change", ch.ID)
			return p.finish(ctx, ch, store.StatusSuppressed)
		}
		texts[lang] = text
	}

	// Record "summarized" before sending, so a crash mid-send resumes here
	// rather than re-summarizing.
	if ch.Status != store.StatusSummarized {
		ch.Status = store.StatusSummarized
		if err := p.store.UpdateChange(ctx, ch); err != nil {
			return err
		}
	}

	title := p.titles[src.Category]
	if title == "" {
		title = src.Label
	}
	links := alertLinks(src, ch.Diff)
	sent := 0
	for _, r := range recips {
		send := p.senders[r.Channel]
		if send == nil {
			continue // channel not configured (e.g. no RESEND_API_KEY)
		}
		// Mark, then send: if the process dies between the two, this
		// contact misses the alert, but nobody ever gets it twice.
		first, err := p.store.MarkDelivered(ctx, ch.ID, r.ID)
		if err != nil {
			return err
		}
		if !first {
			continue // sent before a restart
		}
		err = send.Deliver(ctx, r, notifier.Alert{
			ChangeID: ch.ID, Title: title, Text: texts[r.Language], Links: links, Lang: r.Language,
		})
		switch {
		case err == nil:
			sent++
		case errors.Is(err, notifier.ErrUnreachable):
			p.log.Info("outbox: contact unreachable, deactivating", "contact", r.ID, "channel", r.Channel, "err", err)
			if err := p.store.SetContactActive(ctx, r.ID, false); err != nil {
				p.log.Error("outbox: deactivating contact", "contact", r.ID, "err", err)
			}
		case ctx.Err() != nil:
			return ctx.Err() // shutting down; the rest are sent after restart
		default:
			p.log.Error("outbox: alert lost", "change", ch.ID, "contact", r.ID, "channel", r.Channel, "err", err)
		}
	}
	p.log.Info("outbox: change done", "change", ch.ID, "source", src.ID, "title", title,
		"recipients", len(recips), "delivered", sent)
	return p.finish(ctx, ch, store.StatusNotified)
}

// summary returns the change's summary in lang, from the cache or the
// summarizer, and sets ch.Importance from the first summary made.
func (p *Processor) summary(ctx context.Context, ch *store.Change, src store.Source, lang store.Language) (string, error) {
	text, err := p.store.Summary(ctx, ch.ID, lang)
	if err == nil {
		return text, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	in := summarizer.Input{Change: *ch, Source: src, Lang: lang}
	s, err := p.sum.Summarize(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		ch.Attempts++
		if ch.Attempts < MaxAttempts {
			if uerr := p.store.UpdateChange(ctx, *ch); uerr != nil {
				return "", uerr
			}
			return "", fmt.Errorf("summarize (attempt %d of %d): %w", ch.Attempts, MaxAttempts, err)
		}
		p.log.Warn("outbox: summarizer keeps failing, using fallback", "change", ch.ID, "err", err)
		if s, err = p.fallback.Summarize(ctx, in); err != nil {
			return "", fmt.Errorf("fallback summarizer: %w", err)
		}
	}
	if ch.Importance == store.ImportanceUnknown {
		ch.Importance = s.Importance
	}
	if err := p.store.SaveSummary(ctx, ch.ID, lang, s.Text); err != nil {
		return "", err
	}
	return s.Text, nil
}

func (p *Processor) finish(ctx context.Context, ch store.Change, status store.ChangeStatus) error {
	ch.Status = status
	return p.store.UpdateChange(ctx, ch)
}

// route applies the language table in docs/ARCHITECTURE.md §5: English
// subscribers don't get alerts from Hindi sources (the Hindi feeds lag the
// English ones and mostly repeat them); Hindi subscribers get both.
func route(rs []store.Recipient, src store.Source) []store.Recipient {
	if src.Language != store.LangHindi {
		return rs
	}
	return slices.DeleteFunc(slices.Clone(rs), func(r store.Recipient) bool {
		return r.Language != store.LangHindi
	})
}

// languages lists the summary languages needed: always English (the
// website shows it), then Hindi if any recipient reads Hindi.
func languages(rs []store.Recipient) []store.Language {
	out := []store.Language{store.LangEnglish}
	if slices.ContainsFunc(rs, func(r store.Recipient) bool { return r.Language == store.LangHindi }) {
		out = append(out, store.LangHindi)
	}
	return out
}

// alertLinks picks the links shown under an alert: the document itself for
// a PDF change, or the attachments and links of new and edited items for a
// feed change.
func alertLinks(src store.Source, diff string) []notifier.Link {
	if src.Kind != store.KindFeed {
		return []notifier.Link{{URL: src.URL, PDF: src.Kind == store.KindPDF}}
	}
	var out []notifier.Link
	seen := make(map[string]bool)
	add := func(l notifier.Link) {
		if !seen[l.URL] && len(out) < maxLinks {
			seen[l.URL] = true
			out = append(out, l)
		}
	}
	blocks, _ := differ.ParseFeed(diff)
	for _, b := range blocks {
		fields := b.Fields
		if b.Op == '~' {
			fields = b.Added
		} else if b.Op == '-' {
			continue // a removed notice's file is usually gone too
		}
		for _, f := range fields {
			if u, ok := normalize.AttachmentLink(src.URL, f); ok {
				add(notifier.Link{URL: u, PDF: true})
			} else if u, ok := strings.CutPrefix(f, "link: "); ok && isWebURL(u) {
				add(notifier.Link{URL: u})
			}
		}
	}
	return out
}

// isWebURL guards against a malformed redirectUrl: Telegram rejects the
// whole message if a link in it is invalid, and the alert would be lost.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}
