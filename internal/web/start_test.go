package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/akkuraina/pehredaar/internal/launch"
	"github.com/akkuraina/pehredaar/internal/store"
)

func TestStartWatchingLink(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	sub, err := e.st.CreateSubscriber(t.Context(), store.LangEnglish)
	if err != nil {
		t.Fatal(err)
	}
	// "upsc" isn't live in this test's catalog; it is ignored.
	path := launch.Path(e.signer, sub.ID, []string{"jee", "upsc"})

	// Opening the link only asks; a mail scanner fetching it changes nothing.
	resp, body := e.get(c, path)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Start watching JEE?") ||
		!strings.Contains(body, `action="`+path+`"`) {
		t.Fatalf("GET %d:\n%s", resp.StatusCode, body)
	}
	if subs, _ := e.st.Subscriptions(t.Context(), sub.ID); len(subs) != 0 {
		t.Fatalf("GET subscribed: %+v", subs)
	}

	resp, body = e.post(c, path, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "watching JEE for you") {
		t.Fatalf("POST %d:\n%s", resp.StatusCode, body)
	}
	subs, _ := e.st.Subscriptions(t.Context(), sub.ID)
	if len(subs) != 1 || subs[0].Target.Category != "jee:nta-notices" {
		t.Errorf("subscriptions = %+v, want the JEE feed", subs)
	}
	// Again: harmless.
	if resp, _ := e.post(c, path, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("second POST: %d", resp.StatusCode)
	}

	// A link edited to other exams, or forged, is a 404.
	signed := path[strings.LastIndex(path, "/")+1:]
	for _, bad := range []string{
		"/start/jee,ssc/" + signed,
		"/start/jee/" + signed,
		"/start/jee,upsc/1.forged",
	} {
		if resp, _ := e.get(c, bad); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", bad, resp.StatusCode)
		}
	}
	// Only non-live exams in a valid link: 404 too.
	if resp, _ := e.get(c, launch.Path(e.signer, sub.ID, []string{"upsc"})); resp.StatusCode != http.StatusNotFound {
		t.Errorf("link with no live exam = %d, want 404", resp.StatusCode)
	}
}
