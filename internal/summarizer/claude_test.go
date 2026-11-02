package summarizer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/akkuraina/pehredaar/internal/store"
)

// fakeAPI is a stand-in for the Anthropic API: it records each request
// body and answers with a canned message. Tests never call the real API.
type fakeAPI struct {
	mu       sync.Mutex
	requests []map[string]any
	answer   string // the model's text (normally JSON)
	stop     string // stop_reason; default end_turn
	status   int    // HTTP status; default 200
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if f.status != 0 && f.status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
		return
	}
	stop := f.stop
	if stop == "" {
		stop = "end_turn"
	}
	resp := map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-haiku-5-5",
		"content":       []map[string]any{{"type": "text", "text": f.answer}},
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 10, "output_tokens": 10},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newFake(t *testing.T, f *fakeAPI) *Claude {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewClaude("test-key", "", nil, option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
}

func answer(summary, importance string) string {
	b, _ := json.Marshal(llmOutput{Summary: summary, Importance: importance})
	return string(b)
}

// A date edit in the calendar feed: 18:30 UTC on 30 June is 1 July in India.
const calEdit = "~ cal1 | Combined Graduate Level Examination, 2026\n" +
	"    - start: 2026-05-01T00:00:00.000Z\n" +
	"    + start: 2026-06-30T18:30:00.000Z"

func TestClaudeSummaryPlusDetails(t *testing.T) {
	f := &fakeAPI{answer: answer("The CGL 2026 exam start date has moved from 1 May 2026 to 1 Jul 2026.", "date")}
	c := newFake(t, f)
	s, err := c.Summarize(t.Context(), Input{Change: store.Change{Diff: calEdit}, Source: feedEN, Lang: store.LangEnglish})
	if err != nil {
		t.Fatal(err)
	}
	if s.Importance != store.ImportanceDate {
		t.Errorf("importance = %q, want date", s.Importance)
	}
	want := "The CGL 2026 exam start date has moved from 1 May 2026 to 1 Jul 2026.\n\n" +
		"Notice updated: Combined Graduate Level Examination, 2026\n" +
		"• Start date: 1 May 2026 → 1 Jul 2026"
	if s.Text != want {
		t.Errorf("text:\n%s\nwant:\n%s", s.Text, want)
	}

	// What the model was sent.
	if len(f.requests) != 1 {
		t.Fatalf("%d requests, want 1", len(f.requests))
	}
	req := f.requests[0]
	if req["model"] != DefaultModel {
		t.Errorf("model = %v, want %s", req["model"], DefaultModel)
	}
	oc, _ := req["output_config"].(map[string]any)
	if oc["effort"] != "low" || oc["format"] == nil {
		t.Errorf("output_config = %v, want low effort and a JSON schema", oc)
	}
	prompt := userText(t, req)
	// The model gets IST dates from the template, never raw UTC timestamps.
	if !strings.Contains(prompt, "1 May 2026 → 1 Jul 2026") || strings.Contains(prompt, "T18:30") {
		t.Errorf("prompt should carry IST dates, not raw timestamps:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Output language: English") {
		t.Errorf("prompt lacks the output language:\n%s", prompt)
	}
}

func userText(t *testing.T, req map[string]any) string {
	t.Helper()
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", req["messages"])
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		if s, ok := c.(map[string]any)["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func TestClaudeHindi(t *testing.T) {
	f := &fakeAPI{answer: answer("CGL 2026 परीक्षा की तारीख 1 May 2026 से बदलकर 1 Jul 2026 हो गई है।", "date")}
	c := newFake(t, f)
	s, err := c.Summarize(t.Context(), Input{Change: store.Change{Diff: calEdit}, Source: feedEN, Lang: store.LangHindi})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(userText(t, f.requests[0]), "Output language: Hindi") {
		t.Error("prompt should ask for Hindi")
	}
	// Details come from the Hindi template.
	if !strings.HasPrefix(s.Text, "CGL 2026 परीक्षा") || strings.Contains(s.Text, "Notice updated") {
		t.Errorf("text:\n%s", s.Text)
	}
}

func TestClaudeRejectsInventedNumbers(t *testing.T) {
	// "15" appears nowhere in the change: the model made it up.
	f := &fakeAPI{answer: answer("The exam moved to 15 July 2026.", "date")}
	c := newFake(t, f)
	s, err := c.Summarize(t.Context(), Input{Change: store.Change{Diff: calEdit}, Source: feedEN, Lang: store.LangEnglish})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Text, "15 July") {
		t.Errorf("an invented number reached the alert:\n%s", s.Text)
	}
	if !strings.HasPrefix(s.Text, "Notice updated:") || s.Importance != store.ImportanceOther {
		t.Errorf("want the plain template with importance other, got %q:\n%s", s.Importance, s.Text)
	}
}

func TestClaudeCosmeticOnlyForEdits(t *testing.T) {
	newNotice := "+ n9 | Notice regarding exam cities\n    link: https://ssc.gov.in/x"
	for _, tc := range []struct {
		name string
		diff string
		want store.Importance
	}{
		{"edit", calEdit, store.ImportanceCosmetic},
		{"new notice", newNotice, store.ImportanceOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFake(t, &fakeAPI{answer: answer("", "cosmetic")})
			s, err := c.Summarize(t.Context(), Input{Change: store.Change{Diff: tc.diff}, Source: feedEN, Lang: store.LangEnglish})
			if err != nil {
				t.Fatal(err)
			}
			if s.Importance != tc.want {
				t.Errorf("importance = %q, want %q", s.Importance, tc.want)
			}
			if s.Text == "" {
				t.Error("empty text")
			}
		})
	}
}

func TestClaudePDFGetsDiffText(t *testing.T) {
	diff := "@@ -1,1 +1,1 @@\n- Last date: 12.11.2026\n+ Last date: 20.11.2026"
	f := &fakeAPI{answer: answer("The last date has moved from 12.11.2026 to 20.11.2026.", "date")}
	c := newFake(t, f)
	s, err := c.Summarize(t.Context(), Input{Change: store.Change{Diff: diff}, Source: pdfEN, Lang: store.LangEnglish})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(userText(t, f.requests[0]), "+ Last date: 20.11.2026") {
		t.Error("PDF diff text not sent to the model")
	}
	if s.Importance != store.ImportanceDate || !strings.HasPrefix(s.Text, "The last date has moved") {
		t.Errorf("got %q:\n%s", s.Importance, s.Text)
	}
}

func TestClaudeSkipsHindiPDF(t *testing.T) {
	f := &fakeAPI{answer: answer("x", "date")}
	c := newFake(t, f)
	s, err := c.Summarize(t.Context(), Input{Change: store.Change{Diff: "@@ -1 +1 @@\n- क\n+ ख"}, Source: pdfHI, Lang: store.LangHindi})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 0 {
		t.Error("garbled Hindi PDF text was sent to the model")
	}
	if s.Importance != store.ImportanceOther {
		t.Errorf("importance = %q, want other", s.Importance)
	}
}

// Errors go back to the outbox, which retries and then falls back to the
// template; the summarizer itself must not invent an answer.
func TestClaudeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    *fakeAPI
	}{
		{"http error", &fakeAPI{status: http.StatusBadRequest}},
		{"not json", &fakeAPI{answer: "The date changed."}},
		{"unknown importance", &fakeAPI{answer: answer("x", "urgent")}},
		{"empty summary", &fakeAPI{answer: answer(" ", "date")}},
		{"too long", &fakeAPI{answer: answer(strings.Repeat("a", maxSummaryLen+1), "other")}},
		{"refusal", &fakeAPI{answer: answer("x", "other"), stop: "refusal"}},
		{"cut off", &fakeAPI{answer: `{"summary": "The da`, stop: "max_tokens"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newFake(t, tc.f).Summarize(t.Context(), Input{Change: store.Change{Diff: calEdit}, Source: feedEN, Lang: store.LangEnglish})
			if err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestCheckNumbers(t *testing.T) {
	src := "Start date: 1 May 2026 → 1 Jul 2026; age 18-27; fee Rs. 100; Tier-II"
	for _, tc := range []struct{ summary, bad string }{
		{"Moved from 1 May 2026 to 1 Jul 2026.", ""},
		{"Moved to 01 Jul 2026.", ""}, // leading zero
		{"Age limit is 18 to 27.", ""},
		{"फीस १०० रुपये है।", ""}, // Devanagari digits
		{"Moved to 2 Jul 2026.", "2"},
		{"Fee is Rs. 1000.", "1000"},
		{"No numbers at all.", ""},
		{"फीस २०० रुपये है।", "200"},
	} {
		if got := checkNumbers(tc.summary, src); got != tc.bad {
			t.Errorf("checkNumbers(%q) = %q, want %q", tc.summary, got, tc.bad)
		}
	}
}
