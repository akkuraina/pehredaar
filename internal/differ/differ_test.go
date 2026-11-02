package differ

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akkuraina/pehredaar/internal/store"
)

// Run `go test ./internal/differ -update` to rewrite golden files after an
// intentional change to the rendering, then review the diff in git.
var update = flag.Bool("update", false, "rewrite golden files")

// Fixtures are canonical texts produced by package normalize from real SSC
// responses (see internal/normalize/testdata): feed_en.txt is the notice
// board golden, corrigendum_en.txt the cleaned text of the corrigendum PDF.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func fixtureLines(t *testing.T, name string) []string {
	return strings.Split(fixture(t, name), "\n")
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s; run with -update if the change is intended.\n--- got ---\n%s", path, got)
	}
}

// joined builds a feed text from parts, each a line or a slice of lines.
func joined(parts ...any) string {
	var out []string
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			out = append(out, v)
		case []string:
			out = append(out, v...)
		}
	}
	return strings.Join(out, "\n")
}

const (
	// A notice newer than everything in the fixture.
	freshLine = "zzfresh000000000000000 | 2026-11-01T10:00:00.000Z | Notice of a brand new examination | att: uploads/masterData/NoticeBoards/new.pdf (1000 B)"
	// A notice older than everything on page 1: what scrolls up from page 2.
	page2Line = "zzpage2000000000000000 | 2026-01-05T10:00:00.000Z | An old notice from page 2"
)

// --- feeds -----------------------------------------------------------------

func TestFeedIdentical(t *testing.T) {
	feed := fixture(t, "feed_en.txt")
	if d := Feed(feed, feed, true); !d.Empty() || d.Scrolled != 0 {
		t.Errorf("identical feeds gave %+v", d)
	}
}

func TestFeedScrolling(t *testing.T) {
	l := fixtureLines(t, "feed_en.txt")
	if len(l) != 10 {
		t.Fatalf("fixture has %d lines, want 10", len(l))
	}
	old := joined(l)

	tests := []struct {
		name     string
		new      string
		windowed bool
		added    []string
		removed  []string
		scrolled int
	}{
		{
			// The common case: a notice is posted and the oldest one moves to
			// page 2. Only the new notice is news.
			name:     "new notice pushes oldest off the page",
			new:      joined(freshLine, l[:9]),
			windowed: true,
			added:    []string{freshLine},
			scrolled: 1,
		},
		{
			// The same text for a complete list (the calendar) means a real
			// deletion.
			name:     "same, but feed is a complete list",
			new:      joined(freshLine, l[:9]),
			windowed: false,
			added:    []string{freshLine},
			removed:  []string{l[9]},
		},
		{
			name:     "notice deleted mid-page, one scrolls up from page 2",
			new:      joined(l[:4], l[5:], page2Line),
			windowed: true,
			removed:  []string{l[4]},
			scrolled: 1,
		},
		{
			// Add and delete in the same interval leave the count unchanged,
			// which fooled the count-based rule we first planned.
			name:     "one added and one deleted between checks",
			new:      joined(freshLine, l[:4], l[5:]),
			windowed: true,
			added:    []string{freshLine},
			removed:  []string{l[4]},
		},
		{
			// The bottom item is deleted and replaced from page 2: the
			// replacement is older, so the bottom item did not just scroll.
			name:     "bottom notice deleted",
			new:      joined(l[:9], page2Line),
			windowed: true,
			removed:  []string{l[9]},
			scrolled: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Feed(old, tt.new, tt.windowed)
			if !slices.Equal(d.Added, tt.added) {
				t.Errorf("added = %q, want %q", d.Added, tt.added)
			}
			if !slices.Equal(d.Removed, tt.removed) {
				t.Errorf("removed = %q, want %q", d.Removed, tt.removed)
			}
			if len(d.Edited) != 0 {
				t.Errorf("unexpected edits %+v", d.Edited)
			}
			if d.Scrolled != tt.scrolled {
				t.Errorf("scrolled = %d, want %d", d.Scrolled, tt.scrolled)
			}
		})
	}
}

func TestFeedEdits(t *testing.T) {
	l := fixtureLines(t, "feed_en.txt")
	old := joined(l)
	oldFields := strings.Split(l[1], feedSep)

	tests := []struct {
		name           string
		edit           func(string) string
		removed, added []string
	}{
		{
			name:    "headline reworded",
			edit:    func(s string) string { return strings.Replace(s, "Revised dates", "Re-revised dates", 1) },
			removed: []string{oldFields[2]},
			added:   []string{strings.Replace(oldFields[2], "Revised dates", "Re-revised dates", 1)},
		},
		{
			name:    "attachment replaced (size changed)",
			edit:    func(s string) string { return strings.Replace(s, "(376378 B)", "(380001 B)", 1) },
			removed: []string{oldFields[3]},
			added:   []string{strings.Replace(oldFields[3], "(376378 B)", "(380001 B)", 1)},
		},
		{
			name:  "end date set",
			edit:  func(s string) string { return s + " | end: 2026-11-20T00:00:00.000Z" },
			added: []string{"end: 2026-11-20T00:00:00.000Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edited := slices.Clone(l)
			edited[1] = tt.edit(l[1])
			d := Feed(old, joined(edited), true)
			if len(d.Added)+len(d.Removed) != 0 || len(d.Edited) != 1 {
				t.Fatalf("got %+v, want exactly one edit", d)
			}
			e := d.Edited[0]
			if e.ID != oldFields[0] {
				t.Errorf("edited id = %q, want %q", e.ID, oldFields[0])
			}
			if !slices.Equal(e.Removed, tt.removed) || !slices.Equal(e.Added, tt.added) {
				t.Errorf("fields -%q +%q, want -%q +%q", e.Removed, e.Added, tt.removed, tt.added)
			}
			if d.Kind() != store.ChangeEdited {
				t.Errorf("kind = %q, want edited", d.Kind())
			}
		})
	}
}

func TestFeedKind(t *testing.T) {
	tests := []struct {
		d    FeedDiff
		want store.ChangeKind
	}{
		{FeedDiff{}, ""},
		{FeedDiff{Removed: []string{"x"}}, store.ChangeRemoved},
		{FeedDiff{Removed: []string{"x"}, Edited: []ItemEdit{{}}}, store.ChangeEdited},
		{FeedDiff{Removed: []string{"x"}, Edited: []ItemEdit{{}}, Added: []string{"y"}}, store.ChangeAdded},
	}
	for _, tt := range tests {
		if got := tt.d.Kind(); got != tt.want {
			t.Errorf("%+v: kind = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFeedGolden(t *testing.T) {
	l := fixtureLines(t, "feed_en.txt")
	edited := slices.Clone(l)
	edited[1] = strings.Replace(l[1], "(376378 B)", "(380001 B)", 1)
	// New notice on top, l[4] deleted, l[1]'s PDF replaced, l[9] scrolls off.
	r, err := Diff(store.KindFeed, joined(l), joined(freshLine, edited[:4], edited[5:9]), Options{Windowed: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != store.ChangeAdded || r.Ignored != 1 || r.Truncated {
		t.Errorf("result = %+v", r)
	}
	checkGolden(t, "feed_diff", r.Text)
}

// --- documents -------------------------------------------------------------

func TestTextIdentical(t *testing.T) {
	doc := fixture(t, "corrigendum_en.txt")
	if d := Text(doc, doc); !d.Empty() || d.Ignored != 0 {
		t.Errorf("identical texts gave %+v", d)
	}
}

func TestTextDateChange(t *testing.T) {
	old := fixture(t, "corrigendum_en.txt")
	new := strings.Replace(old, "02nd to 06th November, 2026", "09th to 13th November, 2026", 1)
	d := Text(old, new)
	if len(d.Hunks) != 1 {
		t.Fatalf("got %d hunks, want 1:\n%s", len(d.Hunks), d)
	}
	h := d.Hunks[0]
	if h.OldStart != 8 || h.NewStart != 8 {
		t.Errorf("hunk starts at -%d +%d, want -8 +8", h.OldStart, h.NewStart)
	}
	want := []Line{
		{Equal, "S. Name of Examination Tier/Mode Revised proposed Dates"},
		{Equal, "No."},
		{Delete, "1. Junior Engineer Examination, 2026 Paper-I 02nd to 06th November, 2026"},
		{Insert, "1. Junior Engineer Examination, 2026 Paper-I 09th to 13th November, 2026"},
		{Equal, "(CBE) and 30th November to 01st"},
		{Equal, "December, 2026"},
	}
	if !slices.Equal(h.Lines, want) {
		t.Errorf("hunk =\n%s\nwant lines %v", d, want)
	}
	checkGolden(t, "text_diff", d.String())
}

func TestTextIgnoresCosmetic(t *testing.T) {
	old := fixture(t, "corrigendum_en.txt")
	tests := map[string]func(string) string{
		// pdftotext wrapping a paragraph at different points.
		"paragraph re-wrapped": func(s string) string {
			return strings.Replace(s,
				"Candidates are advised to refer to the Important Notice dated\n"+
					"01.10.2026 uploaded on the Commission’s website. The Commission has decided\n"+
					"to revise the examination dates for the following two examinations:",
				"Candidates are advised to refer to the Important Notice dated 01.10.2026\n"+
					"uploaded on the Commission’s website. The Commission has decided to revise\n"+
					"the examination dates for the following two examinations:", 1)
		},
		"curly quote straightened": func(s string) string {
			return strings.Replace(s, "Commission’s", "Commission's", 1)
		},
		"decoration changed": func(s string) string {
			return strings.Replace(s, "*****", "-----", 1)
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			new := edit(old)
			if new == old {
				t.Fatal("edit did not apply; fixture changed?")
			}
			d := Text(old, new)
			if !d.Empty() || d.Ignored == 0 {
				t.Errorf("want no hunks and some ignored, got ignored=%d\n%s", d.Ignored, d)
			}
		})
	}
}

func TestTextKeepsMeaningfulSmallChanges(t *testing.T) {
	tests := []struct{ name, old, new string }{
		{"decimal point", "Age relaxation 1.5 years", "Age relaxation 15 years"},
		{"date separators", "Last date 12.11.2026", "Last date 1211.2026"},
		{"single digit", "Fee Rs. 100", "Fee Rs. 200"},
		{"devanagari vowel sign", "परीक्षा कि तिथि", "परीक्षा की तिथि"},
		{"word added", "Candidates must apply online", "Candidates must not apply online"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if d := Text(tt.old, tt.new); d.Empty() {
				t.Errorf("change %q -> %q was treated as cosmetic", tt.old, tt.new)
			}
		})
	}
}

func TestTextEdges(t *testing.T) {
	t.Run("line appended at end", func(t *testing.T) {
		d := Text("a\nb", "a\nb\nc")
		want := []Line{{Equal, "a"}, {Equal, "b"}, {Insert, "c"}}
		if len(d.Hunks) != 1 || !slices.Equal(d.Hunks[0].Lines, want) {
			t.Errorf("got\n%s", d)
		}
	})
	t.Run("from empty", func(t *testing.T) {
		d := Text("", "a\nb")
		want := []Line{{Insert, "a"}, {Insert, "b"}}
		if len(d.Hunks) != 1 || !slices.Equal(d.Hunks[0].Lines, want) {
			t.Errorf("got\n%s", d)
		}
	})
}

func TestTextHunkGrouping(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, "line "+string(rune('A'+i)))
	}
	old := strings.Join(lines, "\n")
	change := func(idx ...int) string {
		c := slices.Clone(lines)
		for _, i := range idx {
			c[i] += " changed"
		}
		return strings.Join(c, "\n")
	}

	// 4 unchanged lines between changes: contexts (2+2) just touch, one hunk.
	if d := Text(old, change(5, 10)); len(d.Hunks) != 1 {
		t.Errorf("changes 4 lines apart: %d hunks, want 1\n%s", len(d.Hunks), d)
	}
	// 5 unchanged lines between changes: two hunks.
	d := Text(old, change(5, 11))
	if len(d.Hunks) != 2 {
		t.Fatalf("changes 5 lines apart: %d hunks, want 2\n%s", len(d.Hunks), d)
	}
	if d.Hunks[1].OldStart != 10 || d.Hunks[1].NewStart != 10 {
		t.Errorf("second hunk starts at -%d +%d, want -10 +10", d.Hunks[1].OldStart, d.Hunks[1].NewStart)
	}
}

// --- Diff and capping --------------------------------------------------------

func TestDiffEmptyResult(t *testing.T) {
	old := fixture(t, "corrigendum_en.txt")
	new := strings.Replace(old, "Commission’s", "Commission's", 1)
	r, err := Diff(store.KindPDF, old, new, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Empty() || r.Kind != "" || r.Ignored != 1 {
		t.Errorf("result = %+v, want empty with 1 ignored", r)
	}
}

func TestDiffUnknownKind(t *testing.T) {
	if _, err := Diff("rss", "a", "b", Options{}); err == nil {
		t.Error("want an error for an unknown kind")
	}
}

func TestDiffCap(t *testing.T) {
	// 20 separate hunks of Hindi text (3 bytes per character).
	var old, new []string
	for i := range 200 {
		line := strings.Repeat("परीक्षा", 3) + " " + string(rune('A'+i%26))
		old = append(old, line)
		if i%10 == 0 {
			line += " बदला"
		}
		new = append(new, line)
	}
	r, err := Diff(store.KindPDF, strings.Join(old, "\n"), strings.Join(new, "\n"), Options{MaxBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || !strings.Contains(r.Text, "more not shown") {
		t.Errorf("want a truncated result with a note, got truncated=%v", r.Truncated)
	}
	if len(r.Text) > 1000+40 {
		t.Errorf("text is %d bytes, cap was 1000", len(r.Text))
	}
	if !utf8.ValidString(r.Text) {
		t.Error("text is not valid UTF-8")
	}
	if !strings.HasPrefix(r.Text, "@@ ") {
		t.Errorf("text should start with a whole hunk, got %.40q", r.Text)
	}
}

func TestJoinCutsOversizedFirstBlock(t *testing.T) {
	big := strings.Repeat("तिथि", 100) // 1200 bytes
	got, truncated := join([]string{big, "second"}, 100)
	if !truncated || !utf8.ValidString(got) {
		t.Errorf("truncated=%v valid=%v", truncated, utf8.ValidString(got))
	}
	if !strings.Contains(got, "(cut)") || !strings.Contains(got, "1 more not shown") {
		t.Errorf("missing notes: %q", got)
	}
}
