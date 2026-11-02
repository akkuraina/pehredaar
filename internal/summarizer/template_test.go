package summarizer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akkuraina/pehredaar/internal/store"
)

func summarize(t *testing.T, src store.Source, diff string, lang store.Language) string {
	t.Helper()
	s, err := Template{}.Summarize(t.Context(), Input{
		Change: store.Change{Diff: diff},
		Source: src,
		Lang:   lang,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Importance != store.ImportanceOther {
		t.Errorf("importance = %q, want other (templates never suppress)", s.Importance)
	}
	return s.Text
}

var (
	feedEN = store.Source{Kind: store.KindFeed, Language: store.LangEnglish, Label: "SSC Notice Board"}
	pdfEN  = store.Source{Kind: store.KindPDF, Language: store.LangEnglish, Label: "Important Notice - Revised dates"}
	pdfHI  = store.Source{Kind: store.KindPDF, Language: store.LangHindi, Label: "महत्वपूर्ण सूचना"}
)

func readDiff(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "differ", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFeedGolden(t *testing.T) {
	got := summarize(t, feedEN, readDiff(t, "feed_diff.golden.txt"), store.LangEnglish)
	for _, want := range []string{
		"New notice: Notice of a brand new examination",
		"Notice updated: Important Notice - Revised dates for Junior Engineer Examination",
		"• Attached file replaced",
		"Notice removed: Constable (GD)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	// File paths and ids are not for humans.
	for _, bad := range []string{"uploads/", "zzfresh", " B)"} {
		if strings.Contains(got, bad) {
			t.Errorf("summary contains %q:\n%s", bad, got)
		}
	}
}

func TestFeedEditPairsFieldsAndFormatsDates(t *testing.T) {
	diff := "~ cal1 | Combined Graduate Level Examination, 2026\n" +
		"    - content: May – June 2026\n" +
		"    - start: 2026-05-01T00:00:00.000Z\n" +
		"    + content: July 2026\n" +
		"    + start: 2026-06-30T18:30:00.000Z"
	got := summarize(t, feedEN, diff, store.LangEnglish)
	want := "Notice updated: Combined Graduate Level Examination, 2026\n" +
		"• Schedule: May – June 2026 → July 2026\n" +
		// 18:30 UTC on 30 June is 1 July in India.
		"• Start date: 1 May 2026 → 1 Jul 2026"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFeedHeadlineEdit(t *testing.T) {
	diff := "~ n1 | Result of JE 2026 (revised)\n" +
		"    - Result of JE 2026\n" +
		"    + Result of JE 2026 (revised)"
	got := summarize(t, feedEN, diff, store.LangEnglish)
	if !strings.Contains(got, "• Title: Result of JE 2026 → Result of JE 2026 (revised)") {
		t.Errorf("got:\n%s", got)
	}
}

func TestFeedCapsBlocks(t *testing.T) {
	var lines []string
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		lines = append(lines, "+ "+id+" | 2026-11-01T00:00:00Z | Notice "+id)
	}
	lines = append(lines, "... 3 more not shown")
	got := summarize(t, feedEN, strings.Join(lines, "\n"), store.LangEnglish)
	if strings.Count(got, "New notice:") != maxBlocks {
		t.Errorf("want %d items described:\n%s", maxBlocks, got)
	}
	// 2 cut by the template, 3 by the differ's cap.
	if !strings.HasSuffix(got, "…and 5 more changes.") {
		t.Errorf("got:\n%s", got)
	}
}

func TestPDFEnglishQuotesLines(t *testing.T) {
	got := summarize(t, pdfEN, readDiff(t, "text_diff.golden.txt"), store.LangEnglish)
	for _, want := range []string{
		"Document updated: Important Notice - Revised dates",
		"• Was: 1. Junior Engineer Examination, 2026 Paper-I 02nd to 06th November, 2026",
		"• Now: 1. Junior Engineer Examination, 2026 Paper-I 09th to 13th November, 2026",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

func TestPDFHindiNeverQuotesText(t *testing.T) {
	diff := "@@ -3 +3 @@\n- garbled ि◌ text 12.11.2026\n+ garbled ि◌ text 20.11.2026"
	got := summarize(t, pdfHI, diff, store.LangHindi)
	want := "दस्तावेज़ में बदलाव: महत्वपूर्ण सूचना\nक्या बदला, यह देखने के लिए PDF खोलें।"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestHindiPhrasesForEnglishSource(t *testing.T) {
	// A Hindi subscriber gets English-feed alerts with Hindi framing.
	got := summarize(t, feedEN, "+ x | 2026-11-01T00:00:00Z | Notice of CGL 2026", store.LangHindi)
	if got != "नई सूचना: Notice of CGL 2026" {
		t.Errorf("got %q", got)
	}
}
