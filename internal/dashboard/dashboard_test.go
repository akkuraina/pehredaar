package dashboard

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/scheduler"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

type fakeChecker struct {
	checked []int64
	events  []scheduler.Event
}

func (f *fakeChecker) CheckNow(_ context.Context, id int64) (scheduler.Event, error) {
	f.checked = append(f.checked, id)
	return scheduler.Event{SourceID: id, Outcome: scheduler.OutcomeNotModified, Duration: 60 * time.Millisecond}, nil
}
func (f *fakeChecker) Events() []scheduler.Event { return f.events }
func (f *fakeChecker) Running() map[int64]bool   { return nil }

type fixture struct {
	srv              *httptest.Server
	checker          *fakeChecker
	feed, pdf        store.Source
	snapID, changeID int64
}

func setup(t *testing.T) fixture {
	t.Helper()
	ctx := t.Context()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	feed, err := st.UpsertSource(ctx, store.Source{URL: "https://example.test/feed", Kind: store.KindFeed,
		Category: "ssc:test", Language: store.LangEnglish, Label: "Test Notice Board",
		CheckInterval: 15 * time.Minute, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := st.UpsertSource(ctx, store.Source{URL: "https://example.test/a.pdf", Kind: store.KindPDF,
		Category: "ssc:test", Language: store.LangHindi, Label: "परीक्षा सूचना", ParentID: feed.ID,
		CheckInterval: 6 * time.Hour, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheck(ctx, pdf.ID, store.CheckResult{CheckedAt: time.Now(), NextCheckAt: time.Now().Add(time.Hour),
		Err: "fetch https://example.test/a.pdf: status 503"}); err != nil {
		t.Fatal(err)
	}
	base, _, err := st.SaveSnapshot(ctx, store.Snapshot{SourceID: feed.ID, ContentHash: "aaa", Text: "a | 2026 | Old notice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, ch, err := st.SaveSnapshot(ctx,
		store.Snapshot{SourceID: feed.ID, ContentHash: "bbb", Text: "b | 2026 | New notice <script>\na | 2026 | Old notice"},
		&store.Change{OldSnapshotID: base.ID, Kind: store.ChangeAdded, Diff: "+ b | 2026 | New notice <script>"})
	if err != nil {
		t.Fatal(err)
	}

	checker := &fakeChecker{events: []scheduler.Event{
		{Time: time.Now(), SourceID: feed.ID, Label: feed.Label, Outcome: scheduler.OutcomeChanged, ChangeID: ch.ID, Detail: "+ b"},
		{Time: time.Now(), SourceID: pdf.ID, Label: pdf.Label, Outcome: scheduler.OutcomeFailed, Detail: "status 503"},
	}}
	h, err := New(st, checker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return fixture{srv: srv, checker: checker, feed: feed, pdf: pdf, snapID: snap.ID, changeID: ch.ID}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestPages(t *testing.T) {
	f := setup(t)
	pages := map[string][]string{
		"/": {"Test Notice Board", "परीक्षा सूचना", "failing", "status 503",
			fmt.Sprintf(`href="/changes/%d"`, f.changeID), `http-equiv="refresh"`},
		fmt.Sprintf("/sources/%d", f.feed.ID): {"Check now", "https://example.test/feed", "परीक्षा सूचना",
			"Old notice", fmt.Sprintf(`href="/snapshots/%d"`, f.snapID)},
		fmt.Sprintf("/sources/%d", f.pdf.ID):   {"Found in", "Test Notice Board", "status 503"},
		fmt.Sprintf("/changes/%d", f.changeID): {`<span class="add">`, "not classified yet"},
		fmt.Sprintf("/snapshots/%d", f.snapID): {"bbb", "2 lines"},
	}
	for path, wants := range pages {
		t.Run(path, func(t *testing.T) {
			code, body := get(t, f.srv.URL+path)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			for _, w := range wants {
				if !strings.Contains(body, w) {
					t.Errorf("page is missing %q", w)
				}
			}
			// html/template must escape stored text; a notice containing
			// markup must never become markup.
			if strings.Contains(body, "<script>") {
				t.Error("unescaped <script> in output")
			}
		})
	}
}

func TestNotFound(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/sources/999", "/changes/999", "/snapshots/999", "/sources/abc", "/nope"} {
		if code, _ := get(t, f.srv.URL+path); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
}

func TestCheckNow(t *testing.T) {
	f := setup(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	url := fmt.Sprintf("%s/sources/%d/check", f.srv.URL, f.feed.ID)

	resp, err := client.Post(url, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "not-modified") {
		t.Errorf("status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(f.checker.checked) != 1 || f.checker.checked[0] != f.feed.ID {
		t.Errorf("checked = %v", f.checker.checked)
	}

	// A POST from another site is refused before reaching the handler.
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(f.checker.checked) != 1 {
		t.Errorf("cross-site POST: status %d, checks %d", resp.StatusCode, len(f.checker.checked))
	}
}
