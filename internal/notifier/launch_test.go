package notifier

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/store"
)

func TestListNames(t *testing.T) {
	cases := []struct {
		names []string
		lang  store.Language
		want  string
	}{
		{nil, store.LangEnglish, ""},
		{[]string{"JEE"}, store.LangEnglish, "JEE"},
		{[]string{"JEE", "NEET"}, store.LangEnglish, "JEE and NEET"},
		{[]string{"JEE", "NEET", "IBPS"}, store.LangEnglish, "JEE, NEET and IBPS"},
		{[]string{"JEE", "NEET"}, store.LangHindi, "JEE और NEET"},
	}
	for _, c := range cases {
		if got := ListNames(c.names, c.lang); got != c.want {
			t.Errorf("ListNames(%v, %s) = %q, want %q", c.names, c.lang, got, c.want)
		}
	}
}

func TestAnnounce(t *testing.T) {
	f := &fakeAPI{}
	b, _ := newTestBot(f)
	err := b.Announce(t.Context(), store.Recipient{Contact: store.Contact{ChatID: 42}},
		Launch{ExamIDs: []string{"jee", "neet"}, Names: []string{"JEE", "NEET"}, Lang: store.LangEnglish})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 {
		t.Fatalf("sent %d messages", len(f.sent))
	}
	p := f.sent[0]
	kb, _ := p.ReplyMarkup.(*models.InlineKeyboardMarkup)
	if p.ChatID != int64(42) || !strings.Contains(p.Text, "<b>JEE and NEET</b>") ||
		strings.Join(buttons(kb), ",") != "👀 Start watching JEE and NEET=live:jee,neet" {
		t.Errorf("message = %q, buttons %v", p.Text, buttons(kb))
	}
}

func TestLaunchButtonSubscribes(t *testing.T) {
	c, st := newTestCommands(t)
	c.cats = []config.Category{
		{ID: "ssc:results", Label: "SSC Results"},
		{ID: "jee:nta-notices", Label: "JEE Main notices (NTA)"},
		{ID: "neet:nta-notices", Label: "NEET UG notices (NTA)"},
	}
	// "upsc" has no feeds yet; "nope" isn't an exam. Both are skipped.
	r := ok(t)(c.callback(t.Context(), alice, cbLive+"jee,upsc,nope,neet"))
	if !strings.Contains(r.text, "JEE and NEET") || r.toast != "Watching JEE and NEET" {
		t.Errorf("reply = %+v", r)
	}
	sub, _ := st.TelegramSubscriber(t.Context(), alice.telegramID, alice.chatID)
	subs, _ := st.Subscriptions(t.Context(), sub.ID)
	var cats []string
	for _, s := range subs {
		cats = append(cats, s.Target.Category)
	}
	if strings.Join(cats, ",") != "jee:nta-notices,neet:nta-notices" {
		t.Errorf("subscriptions = %v", cats)
	}
	// A second tap changes nothing and doesn't fail.
	ok(t)(c.callback(t.Context(), alice, cbLive+"jee,neet"))
	if again, _ := st.Subscriptions(t.Context(), sub.ID); len(again) != 2 {
		t.Errorf("after second tap: %d subscriptions", len(again))
	}

	// Nothing live in the list: back to the /watch menu with a toast.
	r = ok(t)(c.callback(t.Context(), alice, cbLive+"upsc"))
	if r.toast == "" || r.kb == nil {
		t.Errorf("no-feed reply = %+v", r)
	}
}
