package web

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/email"
	"github.com/akkuraina/pehredaar/internal/store"
)

type loginData struct {
	base
	Email        string
	Error        string
	EmailEnabled bool
	BotLink      string
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(r); ok {
		http.Redirect(w, r, "/feed", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", loginData{
		base:         s.base(r, "Sign in · Pehredaar", "login"),
		EmailEnabled: s.cfg.Mailer != nil,
		BotLink:      s.botLink(""),
	})
}

// loginSubmit emails a sign-in link if the address belongs to someone.
// The response is the same either way.
func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimSpace(r.PostFormValue("email"))
	if a, err := mail.ParseAddress(addr); err != nil || a.Address != addr {
		s.render(w, r, http.StatusUnprocessableEntity, "login", loginData{
			base: s.base(r, "Sign in · Pehredaar", "login"), Email: addr,
			Error: "That email address doesn't look right.", EmailEnabled: s.cfg.Mailer != nil,
			BotLink: s.botLink(""),
		})
		return
	}
	ctx := r.Context()
	c, err := s.st.ContactByAddress(ctx, store.ChannelEmail, addr)
	switch {
	case err == nil:
		lang := store.LangEnglish
		if sub, err := s.st.Subscriber(ctx, c.SubscriberID); err == nil {
			lang = sub.Language
		}
		s.sendSignInOrConfirm(ctx, c, lang)
	case !errors.Is(err, store.ErrNotFound):
		s.oops(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "message", checkInbox())
}

// loginLink signs in from an emailed (or Telegram-sent) link.
func (s *Server) loginLink(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.ConsumeToken(r.Context(), auth.HashToken(r.PathValue("token")), store.TokenLogin, s.now())
	if err != nil {
		s.expired(w, r, err)
		return
	}
	s.setSession(w, t.SubscriberID)
	http.Redirect(w, r, "/feed", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- unsubscribe ------------------------------------------------------------

type unsubData struct {
	base
	Signed  string
	Address string
	Done    bool
}

// unsubscribePage asks before unsubscribing: link scanners in some mail
// systems open every link in an email, and a GET that unsubscribed would
// unsubscribe people who never clicked.
func (s *Server) unsubscribePage(w http.ResponseWriter, r *http.Request) {
	c, ok := s.unsubContact(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "unsubscribe", unsubData{
		base:    s.base(r, "Unsubscribe · Pehredaar", "unsubscribe"),
		Signed:  r.PathValue("signed"),
		Address: c.Address,
		Done:    !c.Active,
	})
}

// unsubscribe turns email alerts off for one address. Mail providers call
// it directly for one-click unsubscribe (RFC 8058); the confirm page's
// button posts here too.
func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	c, ok := s.unsubContact(w, r)
	if !ok {
		return
	}
	if err := s.st.SetContactActive(r.Context(), c.ID, false); err != nil {
		s.oops(w, r, err)
		return
	}
	s.log.Info("web: email unsubscribed", "contact", c.ID)
	s.render(w, r, http.StatusOK, "unsubscribe", unsubData{
		base: s.base(r, "Unsubscribed · Pehredaar", "unsubscribe"), Address: c.Address, Done: true,
	})
}

func (s *Server) unsubContact(w http.ResponseWriter, r *http.Request) (store.Contact, bool) {
	id, err := s.cfg.Signer.VerifyID(email.UnsubscribePurpose, r.PathValue("signed"))
	if err != nil {
		s.notFound(w, r)
		return store.Contact{}, false
	}
	c, err := s.st.Contact(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r) // the account was deleted
		} else {
			s.oops(w, r, err)
		}
		return store.Contact{}, false
	}
	return c, true
}

// deleteAccount removes everything Pehredaar knows about the signed-in
// person. The form asks them to type DELETE, so it can't be done by
// accident.
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirm")) != "DELETE" {
		http.Redirect(w, r, "/feed#account", http.StatusSeeOther)
		return
	}
	if err := s.st.DeleteSubscriber(r.Context(), sub.ID); err != nil {
		s.oops(w, r, err)
		return
	}
	s.log.Info("web: account deleted", "subscriber", sub.ID)
	s.clearSession(w)
	s.render(w, r, http.StatusOK, "message", pageData{
		Title: "Deleted · Pehredaar", Heading: "Done. I've forgotten you. 🦉", Mood: "sleepy",
		Body:   "Your email, Telegram link and choices are gone. If you ever want me back, I'm here.",
		Action: &link{"/", "Back home"},
	})
}
