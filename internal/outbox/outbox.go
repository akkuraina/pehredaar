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

// Notifier sends one alert to one chat. *notifier.Bot implements it.
type Notifier interface {
	Send(ctx context.Context, chatID int64, a notifier.Alert) error
}

// Config holds the processor's dependencies.
type Config struct {
	Store      store.Store
	Summarizer summarizer.Summarizer
	// Fallback is used once Summarizer has failed MaxAttempts times for a
	// change; default summarizer.Template{}. A late templated alert beats
	// a missed date change.
	Fallback   summarizer.Summarizer
	Notifier   Notifier
	Categories []config.Category // alert titles
	Poll       time.Duration     // how often to look for work; default 15s
	Logger     *slog.Logger
}

// Processor drains unfinished changes. Run it in one goroutine.
type Processor struct {
	store    store.Store
	sum      summarizer.Summarizer
	fallback summarizer.Summarizer
	notify   Notifier
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
		notify:   cfg.Notifier,
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
	all, err := p.store.SubscribersFor(ctx, src)
	if err != nil {
		return err
	}
	subs := route(all, src)
	if len(subs) == 0 {
		// Nobody to tell. Skip the summarizer (an LLM call costs money) and
		// close it; people who subscribe later don't get a backlog.
		return p.finish(ctx, ch, store.StatusNotified)
	}

	// One summary per language actually needed, cached in the store so a
	// retry after a crash doesn't pay for the LLM twice.
	texts := make(map[store.Language]string)
	for _, lang := range languages(subs) {
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
	for _, sub := range subs {
		// Mark, then send: if the process dies between the two, this
		// subscriber misses the alert, but nobody ever gets it twice.
		first, err := p.store.MarkDelivered(ctx, ch.ID, sub.ID)
		if err != nil {
			return err
		}
		if !first {
			continue // sent before a restart
		}
		err = p.notify.Send(ctx, sub.ChatID, notifier.Alert{
			Title: title, Text: texts[sub.Language], Links: links, Lang: sub.Language,
		})
		switch {
		case err == nil:
			sent++
		case errors.Is(err, notifier.ErrBlocked):
			p.log.Info("outbox: subscriber blocked the bot, deactivating", "subscriber", sub.ID)
			if err := p.store.SetActive(ctx, sub.TelegramID, false); err != nil {
				p.log.Error("outbox: deactivating subscriber", "subscriber", sub.ID, "err", err)
			}
		case ctx.Err() != nil:
			return ctx.Err() // shutting down; the rest are sent after restart
		default:
			p.log.Error("outbox: alert lost", "change", ch.ID, "subscriber", sub.ID, "err", err)
		}
	}
	p.log.Info("outbox: alert sent", "change", ch.ID, "source", src.ID, "title", title,
		"subscribers", len(subs), "delivered", sent)
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
func route(subs []store.Subscriber, src store.Source) []store.Subscriber {
	if src.Language != store.LangHindi {
		return subs
	}
	return slices.DeleteFunc(slices.Clone(subs), func(s store.Subscriber) bool {
		return s.Language != store.LangHindi
	})
}

// languages lists the distinct languages of subs, English first.
func languages(subs []store.Subscriber) []store.Language {
	var out []store.Language
	for _, l := range []store.Language{store.LangEnglish, store.LangHindi} {
		if slices.ContainsFunc(subs, func(s store.Subscriber) bool { return s.Language == l }) {
			out = append(out, l)
		}
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
