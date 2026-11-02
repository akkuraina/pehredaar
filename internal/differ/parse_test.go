package differ

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseFeedGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "feed_diff.golden.txt"))
	if err != nil {
		t.Fatal(err)
	}
	blocks, more := ParseFeed(string(b))
	if more != 0 || len(blocks) != 3 {
		t.Fatalf("got %d blocks, more=%d; want 3, 0", len(blocks), more)
	}

	add := blocks[0]
	if add.Op != '+' || add.ID != "zzfresh000000000000000" || add.Headline != "Notice of a brand new examination" ||
		!slices.Equal(add.Fields, []string{"att: uploads/masterData/NoticeBoards/new.pdf (1000 B)"}) {
		t.Errorf("added = %+v", add)
	}

	ed := blocks[1]
	if ed.Op != '~' || ed.ID != "5jfpb0tey9le4bsgd207x" || len(ed.Removed) != 1 || len(ed.Added) != 1 ||
		ed.Added[0] != "att: uploads/masterData/NoticeBoards/Important notice_revised_dated_26_10_2026.pdf (380001 B)" {
		t.Errorf("edited = %+v", ed)
	}
	if ed.Headline == "" || ed.Headline[:17] != "Important Notice " {
		t.Errorf("edited headline = %q", ed.Headline)
	}

	if rm := blocks[2]; rm.Op != '-' || rm.ID != "hv72inihlv3c3rp1ni034" || len(rm.Fields) != 1 {
		t.Errorf("removed = %+v", rm)
	}
}

func TestParseFeedCapped(t *testing.T) {
	text := "+ a | 2026-11-01 | One\n... 4 more not shown"
	blocks, more := ParseFeed(text)
	if len(blocks) != 1 || more != 4 {
		t.Errorf("blocks=%+v more=%d", blocks, more)
	}
}

func TestParseTextGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "text_diff.golden.txt"))
	if err != nil {
		t.Fatal(err)
	}
	removed, added, _ := ParseText(string(b))
	if len(removed) == 0 || len(added) == 0 {
		t.Fatalf("removed=%q added=%q", removed, added)
	}
	if removed[0] != "1. Junior Engineer Examination, 2026 Paper-I 02nd to 06th November, 2026" ||
		added[0] != "1. Junior Engineer Examination, 2026 Paper-I 09th to 13th November, 2026" {
		t.Errorf("first pair = %q → %q", removed[0], added[0])
	}
}

// The renderer and parser must agree: whatever Feed renders, ParseFeed
// reads back with the same ids and changed fields.
func TestParseFeedRoundTrip(t *testing.T) {
	old := "b | 2026-10-02 | Bravo | start: 1 Nov\na | 2026-10-01 | Alpha"
	new := "c | 2026-10-03 | Charlie\nb | 2026-10-02 | Bravo | start: 9 Nov"
	r, err := Diff("feed", old, new, Options{})
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := ParseFeed(r.Text)
	var ops []byte
	for _, b := range blocks {
		ops = append(ops, b.Op)
	}
	if string(ops) != "+~-" {
		t.Fatalf("ops = %q, diff:\n%s", ops, r.Text)
	}
	if e := blocks[1]; e.Headline != "Bravo" || !slices.Equal(e.Removed, []string{"start: 1 Nov"}) ||
		!slices.Equal(e.Added, []string{"start: 9 Nov"}) {
		t.Errorf("edit = %+v", e)
	}
}
