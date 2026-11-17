package web

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/email"
	"github.com/akkuraina/pehredaar/internal/exams"
	"github.com/akkuraina/pehredaar/internal/store"
)

const (
	confirmTTL = 48 * time.Hour
	loginTTL   = 30 * time.Minute
	linkTTL    = 24 * time.Hour
)

// joinData drives the survey: four steps (exams, feeds, how to be told,
// language) in one form. With JS it shows one step at a time; without,
// it's one long form.
type joinData struct {
	base
	Exams        []examView
	FeedGroups   []feedGroup
	Sel          selection
	Errors       []string
	Edit         bool // signed in: changing settings, not signing up
	EmailEnabled bool
	TGEnabled    bool
	HasEmail     string // edit mode: the confirmed/pending address, if any
	HasTelegram  bool
}

type feedGroup struct {
	Exam  exams.Exam
	Feeds []config.Category
}

type selection struct {
	Exams map[string]bool
	Feeds map[string]bool
	Via   string // "telegram", "email" or "both"
	Email string
	Lang  string
}

func (s *Server) feedGroups() []feedGroup {
	var out []feedGroup
	for _, e := range exams.Catalog {
		g := feedGroup{Exam: e}
		for _, c := range s.cfg.Categories {
			if e.CategoryPrefix != "" && strings.HasPrefix(c.ID, e.CategoryPrefix) {
				g.Feeds = append(g.Feeds, c)
			}
		}
		if len(g.Feeds) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func (s *Server) joinData(r *http.Request) joinData {
	title := "Join · Pehredaar"
	d := joinData{
		base:         s.base(r, title, "join"),
		Exams:        s.examViews(),
		FeedGroups:   s.feedGroups(),
		EmailEnabled: s.cfg.Mailer != nil,
		TGEnabled:    s.cfg.BotUsername() != "",
		Sel:          selection{Exams: map[string]bool{}, Feeds: map[string]bool{}, Lang: "en"},
	}
	switch {
	case d.TGEnabled && d.EmailEnabled:
		d.Sel.Via = "both"
	case d.EmailEnabled:
		d.Sel.Via = "email"
	default:
		d.Sel.Via = "telegram"
	}
	return d
}

func (s *Server) joinForm(w http.ResponseWriter, r *http.Request) {
	d := s.joinData(r)
	if sub, ok := s.current(r); ok {
		if err := s.prefill(r.Context(), &d, sub); err != nil {
			s.oops(w, r, err)
			return
		}
	} else if e := r.URL.Query().Get("exam"); e != "" {
		d.Sel.Exams[e] = true // from an exam chip on the landing page
	}
	s.render(w, r, http.StatusOK, "join", d)
}

// prefill loads a signed-in person's current settings into the survey.
func (s *Server) prefill(ctx context.Context, d *joinData, sub store.Subscriber) error {
	d.Edit = true
	d.Title = "What I watch for you · Pehredaar"
	d.Sel.Lang = string(sub.Language)
	interests, err := s.st.Interests(ctx, sub.ID)
	if err != nil {
		return err
	}
	for _, e := range interests {
		d.Sel.Exams[e] = true
	}
	subs, err := s.st.Subscriptions(ctx, sub.ID)
	if err != nil {
		return err
	}
	for _, x := range subs {
		if x.Target.Category != "" {
			d.Sel.Feeds[x.Target.Category] = true
		}
	}
	contacts, err := s.st.Contacts(ctx, sub.ID)
	if err != nil {
		return err
	}
	for _, c := range contacts {
		switch c.Channel {
		case store.ChannelEmail:
			d.HasEmail = c.Address
			d.Sel.Email = c.Address
		case store.ChannelTelegram:
			d.HasTelegram = true
		}
	}
	switch {
	case d.HasEmail != "" && d.HasTelegram:
		d.Sel.Via = "both"
	case d.HasEmail != "":
		d.Sel.Via = "email"
	case d.HasTelegram:
		d.Sel.Via = "telegram"
	}
	return nil
}

// parseSelection reads and validates the survey. It returns the
// selection (to redisplay) and any problems, in plain words.
func (s *Server) parseSelection(r *http.Request, d *joinData) []string {
	sel := selection{Exams: map[string]bool{}, Feeds: map[string]bool{}}
	live := s.categoryIDs()
	var errs []string

	for _, id := range r.PostForm["exam"] {
		if _, ok := exams.ByID(id); ok {
			sel.Exams[id] = true
		}
	}
	for _, id := range r.PostForm["feed"] {
		if slices.Contains(live, id) {
			sel.Feeds[id] = true
		}
	}
	if len(sel.Exams) == 0 {
		errs = append(errs, "Pick at least one exam for me to watch.")
	}
	// A live exam chosen with none of its feeds ticked means "all of them":
	// that's what someone who skipped the feeds step expects.
	for id := range sel.Exams {
		e, _ := exams.ByID(id)
		cats := e.Categories(live)
		if len(cats) > 0 && !slices.ContainsFunc(cats, func(c string) bool { return sel.Feeds[c] }) {
			for _, c := range cats {
				sel.Feeds[c] = true
			}
		}
	}
	// Feeds of exams that aren't selected are dropped.
	for c := range sel.Feeds {
		if e, ok := exams.ForCategory(c); !ok || !sel.Exams[e.ID] {
			delete(sel.Feeds, c)
		}
	}

	sel.Via = r.PostForm.Get("via")
	wantEmail := sel.Via == "email" || sel.Via == "both"
	wantTG := sel.Via == "telegram" || sel.Via == "both"
	switch {
	case !wantEmail && !wantTG:
		errs = append(errs, "Tell me how to reach you: Telegram, email, or both.")
	case wantEmail && !d.EmailEnabled:
		errs = append(errs, "Email alerts aren't switched on yet. Choose Telegram for now.")
	case wantTG && !d.TGEnabled:
		errs = append(errs, "The Telegram bot isn't running right now. Choose email for now.")
	}
	sel.Email = strings.TrimSpace(r.PostForm.Get("email"))
	if wantEmail && d.EmailEnabled {
		if addr, err := mail.ParseAddress(sel.Email); err != nil || addr.Address != sel.Email || len(sel.Email) > 254 {
			errs = append(errs, "That email address doesn't look right.")
		}
	}
	sel.Lang = r.PostForm.Get("lang")
	if sel.Lang != string(store.LangHindi) {
		sel.Lang = string(store.LangEnglish)
	}
	d.Sel = sel
	return errs
}

func (s *Server) joinSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	d := s.joinData(r)
	sub, signedIn := s.current(r)
	if signedIn {
		d.Edit = true
		if err := s.prefill(r.Context(), &d, sub); err != nil {
			s.oops(w, r, err)
			return
		}
	}
	// The honeypot: a field people never see (hidden by CSS). Bots that
	// fill every field get the success page and nothing else.
	if r.PostForm.Get("website") != "" {
		s.render(w, r, http.StatusOK, "message", checkInbox())
		return
	}
	if errs := s.parseSelection(r, &d); len(errs) > 0 {
		d.Errors = errs
		s.render(w, r, http.StatusUnprocessableEntity, "join", d)
		return
	}
	ctx := r.Context()
	sel := d.Sel
	wantEmail := sel.Via == "email" || sel.Via == "both"
	wantTG := sel.Via == "telegram" || sel.Via == "both"

	if !signedIn && wantEmail {
		// Someone who already has this address gets a sign-in link
		// instead. Their settings aren't touched from an unauthenticated
		// form, and the page looks the same either way, so the form can't
		// be used to learn who has signed up.
		if c, err := s.st.ContactByAddress(ctx, store.ChannelEmail, sel.Email); err == nil {
			s.sendSignInOrConfirm(ctx, c, store.Language(sel.Lang))
			s.render(w, r, http.StatusOK, "message", checkInbox())
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			s.oops(w, r, err)
			return
		}
	}

	if !signedIn {
		var err error
		if sub, err = s.st.CreateSubscriber(ctx, store.Language(sel.Lang)); err != nil {
			s.oops(w, r, err)
			return
		}
		// The person who just created this (empty) record may see it.
		// Their email still needs confirming before any alert goes there.
		s.setSession(w, sub.ID)
	}
	if err := s.applySelection(ctx, sub.ID, sel); err != nil {
		s.oops(w, r, err)
		return
	}

	next := "/feed?m=saved"
	if wantEmail && !strings.EqualFold(d.HasEmail, sel.Email) {
		c, err := s.st.AddEmail(ctx, sub.ID, sel.Email)
		if err != nil {
			s.oops(w, r, err)
			return
		}
		if c.SubscriberID == sub.ID {
			if err := s.sendConfirm(ctx, c, store.Language(sel.Lang)); err != nil {
				s.oops(w, r, err)
				return
			}
			next = "/feed?m=check-email"
		} else {
			// Signed-in person typed an address that belongs to someone
			// else. Don't say so; send that address a sign-in link.
			s.sendSignInOrConfirm(ctx, c, store.Language(sel.Lang))
		}
	}
	if wantTG && !d.HasTelegram {
		token, hash := auth.NewToken()
		if err := s.st.CreateToken(ctx, store.Token{
			Hash: hash, Purpose: store.TokenLinkTelegram, SubscriberID: sub.ID, ExpiresAt: s.now().Add(linkTTL),
		}); err != nil {
			s.oops(w, r, err)
			return
		}
		s.render(w, r, http.StatusOK, "telegram", telegramData{
			base:     s.base(r, "One last tap · Pehredaar", "telegram"),
			Link:     s.botLink(token),
			EmailToo: next == "/feed?m=check-email",
			FeedURL:  "/feed",
			SignedIn: signedIn,
		})
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

type telegramData struct {
	base
	Link     string
	EmailToo bool
	FeedURL  string
	SignedIn bool
}

// applySelection makes the subscriber's language, interests and feed
// subscriptions match the survey.
func (s *Server) applySelection(ctx context.Context, subID int64, sel selection) error {
	if err := s.st.SetLanguage(ctx, subID, store.Language(sel.Lang)); err != nil {
		return err
	}
	var ids []string
	for _, e := range exams.Catalog { // catalog order, for stable storage
		if sel.Exams[e.ID] {
			ids = append(ids, e.ID)
		}
	}
	if err := s.st.SetInterests(ctx, subID, ids); err != nil {
		return err
	}
	current, err := s.st.Subscriptions(ctx, subID)
	if err != nil {
		return err
	}
	for _, x := range current {
		if x.Target.Category != "" && !sel.Feeds[x.Target.Category] {
			if _, err := s.st.Unsubscribe(ctx, subID, x.Target); err != nil {
				return err
			}
		}
	}
	for c := range sel.Feeds {
		if err := s.st.Subscribe(ctx, subID, store.Target{Category: c}); err != nil {
			return err
		}
	}
	return nil
}

// sendConfirm emails a confirmation link for an unverified address.
func (s *Server) sendConfirm(ctx context.Context, c store.Contact, lang store.Language) error {
	token, hash := auth.NewToken()
	if err := s.st.CreateToken(ctx, store.Token{
		Hash: hash, Purpose: store.TokenVerifyEmail, SubscriberID: c.SubscriberID, ContactID: c.ID,
		ExpiresAt: s.now().Add(confirmTTL),
	}); err != nil {
		return err
	}
	return s.mailOrLog(ctx, func(ctx context.Context) error {
		return s.cfg.Mailer.SendConfirm(ctx, c.Address, lang, s.cfg.BaseURL+"/confirm/"+token)
	})
}

// sendSignInOrConfirm handles an address we already know: a confirmed one
// gets a sign-in link, an unconfirmed one a fresh confirmation.
func (s *Server) sendSignInOrConfirm(ctx context.Context, c store.Contact, lang store.Language) {
	var err error
	if c.Verified() {
		err = s.sendLogin(ctx, c, lang)
	} else {
		err = s.sendConfirm(ctx, c, lang)
	}
	if err != nil {
		s.log.Error("web: emailing a known address", "contact", c.ID, "err", err)
	}
}

func (s *Server) sendLogin(ctx context.Context, c store.Contact, lang store.Language) error {
	token, hash := auth.NewToken()
	if err := s.st.CreateToken(ctx, store.Token{
		Hash: hash, Purpose: store.TokenLogin, SubscriberID: c.SubscriberID, ExpiresAt: s.now().Add(loginTTL),
	}); err != nil {
		return err
	}
	return s.mailOrLog(ctx, func(ctx context.Context) error {
		return s.cfg.Mailer.SendLogin(ctx, c.Address, lang, s.cfg.BaseURL+"/l/"+token)
	})
}

// mailOrLog sends an email without making the visitor wait on it or see
// its failure: Resend errors are logged for the operator. Sending in the
// background also keeps response times the same whether or not an
// address is known (no timing hint). The context is detached from the
// request, which ends as soon as the page is sent.
func (s *Server) mailOrLog(ctx context.Context, send func(context.Context) error) error {
	if s.cfg.Mailer == nil {
		return nil
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := send(ctx); err != nil {
			if email.IsTestingRestriction(err) {
				s.log.Warn("web: Resend only delivers to your own address until a domain is verified", "err", err)
				return
			}
			s.log.Error("web: sending email", "err", err)
		}
	}()
	return nil
}

func checkInbox() pageData {
	return pageData{
		Title:   "Check your inbox · Pehredaar",
		Heading: "Check your inbox 📬",
		Mood:    "happy",
		Body:    "If that address can get mail from me, a link is on its way. It might take a minute, and it might hide in Promotions or Spam.",
		Action:  &link{"/", "Back home"},
	}
}

// confirmEmail verifies an address from its emailed link and signs in.
func (s *Server) confirmEmail(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.ConsumeToken(r.Context(), auth.HashToken(r.PathValue("token")), store.TokenVerifyEmail, s.now())
	if err != nil {
		s.expired(w, r, err)
		return
	}
	if err := s.st.VerifyContact(r.Context(), t.ContactID); err != nil {
		s.expired(w, r, err)
		return
	}
	s.setSession(w, t.SubscriberID)
	http.Redirect(w, r, "/feed?m=confirmed", http.StatusSeeOther)
}

func (s *Server) expired(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, store.ErrNotFound) {
		s.oops(w, r, err)
		return
	}
	s.render(w, r, http.StatusGone, "message", pageData{
		Title: "Link expired · Pehredaar", Heading: "That link has flown 🦉", Mood: "sleepy",
		Body:   "Links from my emails work once, and only for a while. Ask for a fresh one.",
		Action: &link{"/login", "Send me a new link"},
	})
}
