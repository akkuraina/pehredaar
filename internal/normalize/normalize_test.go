package normalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/akkuraina/pehredaar/internal/store"
)

// Run `go test ./internal/normalize -update` to rewrite golden files after an
// intentional change to the canonical format, then review the diff in git.
var update = flag.Bool("update", false, "rewrite golden files")

const feedURL = "https://ssc.gov.in/api/general-website/portal/notice-boards?page=1&limit=10"

var (
	feedSource = store.Source{Kind: store.KindFeed, URL: feedURL}
	pdfSource  = store.Source{Kind: store.KindPDF, URL: "https://ssc.gov.in/api/attachment/x.pdf"}
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
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

func normalize(t *testing.T, src store.Source, body []byte) Doc {
	t.Helper()
	doc, err := New().Normalize(t.Context(), src, body, Rules{})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// --- feeds -----------------------------------------------------------------

func TestSSCFeedGolden(t *testing.T) {
	for _, name := range []string{"ssc_notice_board_en", "ssc_notice_board_hi"} {
		t.Run(name, func(t *testing.T) {
			doc := normalize(t, feedSource, readFixture(t, name+".json"))
			if len(doc.Items) != 10 {
				t.Errorf("got %d items, want 10", len(doc.Items))
			}
			if got := strings.Count(doc.Text, "\n") + 1; got != len(doc.Items) {
				t.Errorf("text has %d lines for %d items", got, len(doc.Items))
			}
			checkGolden(t, name, doc.Text)
		})
	}
}

func TestSSCCalendar(t *testing.T) {
	body := readFixture(t, "ssc_calendar_en.json")
	doc := normalize(t, feedSource, body)
	if len(doc.Items) != 13 {
		t.Errorf("got %d items, want 13", len(doc.Items))
	}
	checkGolden(t, "ssc_calendar_en", doc.Text)

	// The calendar's `content` field is the exam schedule: the single most
	// important thing to notice changing. Guard against dropping it.
	moved := bytes.Replace(body, []byte(`"content":"May 2026"`), []byte(`"content":"June 2026"`), 1)
	if bytes.Equal(moved, body) {
		t.Fatal("fixture no longer contains the expected calendar entry")
	}
	if normalize(t, feedSource, moved).Hash == doc.Hash {
		t.Error("calendar schedule change did not change the hash")
	}
}

func TestSSCFeedAttachmentURL(t *testing.T) {
	doc := normalize(t, feedSource, readFixture(t, "ssc_notice_board_en.json"))
	var urls []string
	for _, it := range doc.Items {
		for _, a := range it.Attachments {
			urls = append(urls, a.URL)
		}
	}
	// This exact URL was downloaded successfully on 2026-10-30.
	want := "https://ssc.gov.in/api/attachment/uploads/masterData/NoticeBoards/Important%20notice_revised_dated_26_10_2026.pdf"
	if !slices.Contains(urls, want) {
		t.Errorf("attachment URLs %v do not include %s", urls, want)
	}
}

// mutateFeed decodes the English fixture into generic JSON, applies fn, and
// re-encodes it, so tests can derive variants of a real response.
func mutateFeed(t *testing.T, fn func(env map[string]any, items []any)) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(readFixture(t, "ssc_notice_board_en.json"), &env); err != nil {
		t.Fatal(err)
	}
	fn(env, env["data"].([]any))
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSSCFeedIgnoresNoise(t *testing.T) {
	base := normalize(t, feedSource, readFixture(t, "ssc_notice_board_en.json"))

	noisy := map[string]func(env map[string]any, items []any){
		"cache flag and pagination": func(env map[string]any, _ []any) {
			env["isCache"] = false
			env["paginate"] = map[string]any{"totalRecords": 717, "currentPage": 1}
		},
		"item order": func(_ map[string]any, items []any) {
			slices.Reverse(items)
		},
		"headline whitespace": func(_ map[string]any, items []any) {
			it := items[0].(map[string]any)
			it["headline"] = "  " + strings.ReplaceAll(it["headline"].(string), " ", " \n\u00a0 ") + "\t"
		},
		"internal attachment id": func(_ map[string]any, items []any) {
			att := items[0].(map[string]any)["attachments"].([]any)[0].(map[string]any)
			att["id"] = "regenerated-id"
		},
		"unknown new field": func(_ map[string]any, items []any) {
			items[0].(map[string]any)["viewCount"] = 12345
		},
		"null vs empty date": func(_ map[string]any, items []any) {
			items[0].(map[string]any)["startDate"] = ""
		},
	}
	for name, fn := range noisy {
		t.Run(name, func(t *testing.T) {
			doc := normalize(t, feedSource, mutateFeed(t, fn))
			if doc.Hash != base.Hash {
				t.Errorf("hash changed for pure noise.\n--- base ---\n%s\n--- got ---\n%s", base.Text, doc.Text)
			}
		})
	}
}

func TestSSCFeedDetectsRealChanges(t *testing.T) {
	base := normalize(t, feedSource, readFixture(t, "ssc_notice_board_en.json"))

	real := map[string]func(env map[string]any, items []any){
		"headline edited": func(_ map[string]any, items []any) {
			it := items[1].(map[string]any)
			it["headline"] = strings.Replace(it["headline"].(string), "Revised dates", "Re-revised dates", 1)
		},
		"pdf replaced (size changed)": func(_ map[string]any, items []any) {
			att := items[1].(map[string]any)["attachments"].([]any)[0].(map[string]any)
			att["size"] = 380001.0
		},
		"end date set": func(_ map[string]any, items []any) {
			items[1].(map[string]any)["endDate"] = "2026-11-20T00:00:00.000Z"
		},
		"item removed": func(env map[string]any, items []any) {
			env["data"] = items[1:]
		},
	}
	for name, fn := range real {
		t.Run(name, func(t *testing.T) {
			doc := normalize(t, feedSource, mutateFeed(t, fn))
			if doc.Hash == base.Hash {
				t.Error("hash unchanged for a real change")
			}
		})
	}
}

func TestSSCFeedRejectsNonContent(t *testing.T) {
	bodies := map[string]string{
		"api error":  `{"statusCode":"203","statusMessage":"Error Occurred!","error":"Query params are missing"}`,
		"empty data": `{"statusCode":"200","data":[]}`,
		"html page":  `<html><body>502 Bad Gateway</body></html>`,
		"empty body": ``,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, err := New().Normalize(t.Context(), feedSource, []byte(body), Rules{})
			if !errors.Is(err, ErrNotContent) {
				t.Errorf("err = %v, want ErrNotContent", err)
			}
		})
	}
}

// --- shared text cleaning -------------------------------------------------

func TestCleanText(t *testing.T) {
	in := "\ufeff  Last  date:\u00a0 12.11.2026  \r\n\r\n\f" +
		"Page 1 of 3\n" +
		"Visitors: 98213\n" +
		"Fee: Rs.\u00ad100\n"
	drop := []*regexp.Regexp{
		regexp.MustCompile(`^Visitors: \d+$`),
		regexp.MustCompile(`(?i)^page \d+ of \d+$`),
	}
	want := "Last date: 12.11.2026\nFee: Rs.100"
	if got := CleanText(in, drop); got != want {
		t.Errorf("CleanText = %q, want %q", got, want)
	}
}

func TestCleanTextUnicodeNFC(t *testing.T) {
	// U+00E9 (precomposed e-acute) vs "e" + U+0301 (combining acute).
	if CleanText("caf\u00e9", nil) != CleanText("cafe\u0301", nil) {
		t.Error("NFC normalization not applied")
	}
	// Devanagari: U+0958 (qa) as one code point vs U+0915 (ka) + U+093C (nukta).
	if CleanText("\u0958", nil) != CleanText("क\u093c", nil) {
		t.Error("Devanagari nukta forms not unified")
	}
}

// --- PDFs ------------------------------------------------------------------

func requirePDFToText(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
}

// PDF output depends on the pdftotext build (xpdf vs poppler), so instead
// of golden files these tests assert on content that any build must
// produce, plus determinism.

func TestPDFEnglishCorrigendum(t *testing.T) {
	requirePDFToText(t)
	body := readFixture(t, "ssc_corrigendum_en.pdf")
	doc := normalize(t, pdfSource, body)

	for _, want := range []string{
		"CORRIGENDUM TO IMPORTANT NOTICE DATED 01.10.2026",
		"Junior Engineer Examination, 2026",
		"02nd to 06th November, 2026",
		"23rd to 28th November, 2026",
	} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("text is missing %q\n--- text ---\n%s", want, doc.Text)
		}
	}
	if strings.Contains(doc.Text, "  ") || strings.Contains(doc.Text, "\n\n") {
		t.Error("whitespace was not collapsed")
	}
	if again := normalize(t, pdfSource, body); again.Hash != doc.Hash {
		t.Error("extraction is not deterministic")
	}
}

func TestPDFHindiIsStable(t *testing.T) {
	requirePDFToText(t)
	body := readFixture(t, "ssc_notice_hi.pdf")
	doc := normalize(t, pdfSource, body)

	// The Devanagari comes out garbled (the PDF's font encoding is broken;
	// see docs/ARCHITECTURE.md §2), but dates survive, and that's what we
	// most need to diff.
	for _, want := range []string{"22.09.2026", "17.09.2026", "HQ-C-3019/1/2026-C-3"} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("text is missing %q", want)
		}
	}
	if again := normalize(t, pdfSource, body); again.Hash != doc.Hash {
		t.Error("extraction is not deterministic")
	}
}

func TestPDFRejectsNonPDF(t *testing.T) {
	_, err := New().Normalize(t.Context(), pdfSource, []byte("<html>Service Unavailable</html>"), Rules{})
	if !errors.Is(err, ErrNotContent) {
		t.Errorf("err = %v, want ErrNotContent", err)
	}
}

func TestPDFToolMissing(t *testing.T) {
	n := &Normalizer{PDFToText: "definitely-not-pdftotext"}
	_, err := n.Normalize(t.Context(), pdfSource, []byte("%PDF-1.4 ..."), Rules{})
	if err == nil || errors.Is(err, ErrNotContent) {
		t.Errorf("err = %v, want a tool error (not ErrNotContent)", err)
	}
}
