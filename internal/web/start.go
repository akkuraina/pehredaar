package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/akkuraina/pehredaar/internal/exams"
	"github.com/akkuraina/pehredaar/internal/launch"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
)

// The "start watching" page, linked from the email that tells someone an
// exam they asked about is now live (package launch). GET only asks: mail
// scanners open every link in a message, so a bare link must not change
// anyone's subscriptions. The button POSTs back to the same URL.
//
// The link carries a signature over the subscriber id and the exact exam
// list, so it can't be edited to subscribe someone else or to other exams.
// It doesn't sign anyone in: it can only add these exams' feeds.

type startData struct {
	base
	Names  string // "JEE and NEET"
	Action string
	Done   bool
}

func (s *Server) startPage(w http.ResponseWriter, r *http.Request) {
	_, exs, ok := s.startTarget(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "start", startData{
		base:   s.base(r, "Start watching · Pehredaar", "start"),
		Names:  examNames(exs),
		Action: r.URL.Path,
	})
}

func (s *Server) startWatching(w http.ResponseWriter, r *http.Request) {
	subID, exs, ok := s.startTarget(w, r)
	if !ok {
		return
	}
	live := s.categoryIDs()
	for _, e := range exs {
		for _, cat := range e.Categories(live) {
			if err := s.st.Subscribe(r.Context(), subID, store.Target{Category: cat}); err != nil {
				s.oops(w, r, err)
				return
			}
		}
	}
	s.log.Info("web: started watching newly live exams", "subscriber", subID, "exams", r.PathValue("exams"))
	s.render(w, r, http.StatusOK, "start", startData{
		base:  s.base(r, "Watching · Pehredaar", "start"),
		Names: examNames(exs),
		Done:  true,
	})
}

// startTarget checks the link and returns whose it is and which of its
// exams are live. Anything wrong is a plain 404: a forged or truncated
// link learns nothing.
func (s *Server) startTarget(w http.ResponseWriter, r *http.Request) (int64, []exams.Exam, bool) {
	list := r.PathValue("exams")
	id, err := s.cfg.Signer.VerifyID(launch.Purpose(list), r.PathValue("signed"))
	if err != nil {
		s.notFound(w, r)
		return 0, nil, false
	}
	if _, err := s.st.Subscriber(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r) // deleted their account since
		} else {
			s.oops(w, r, err)
		}
		return 0, nil, false
	}
	live := s.categoryIDs()
	var exs []exams.Exam
	for _, eid := range strings.Split(list, ",") {
		if e, ok := exams.ByID(eid); ok && e.Live(live) {
			exs = append(exs, e)
		}
	}
	if len(exs) == 0 {
		s.notFound(w, r) // the exams were taken off since
		return 0, nil, false
	}
	return id, exs, true
}

func examNames(exs []exams.Exam) string {
	names := make([]string, len(exs))
	for i, e := range exs {
		names[i] = e.Name
	}
	return notifier.ListNames(names, store.LangEnglish)
}
