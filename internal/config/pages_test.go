package config

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akkuraina/pehredaar/internal/normalize"
)

// TestRepoPageSources runs the page-reader rules exactly as sources.yaml
// writes them against saved copies of the real pages (fetched 2026-11-18,
// scripts and styles stripped). A selector edit that breaks a source
// fails here, not silently in production.
func TestRepoPageSources(t *testing.T) {
	s, err := LoadSources(filepath.Join("..", "..", "sources.yaml"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		fixture string
		items   int
		first   string // newest headline
		never   string // must be filtered out ("" = no check)
	}
	wants := map[string]want{
		"jee:nta-notices": {"nta_home.html", 15,
			"Clarification regarding submission of a fresh Online Application Form for JEE (Main) – 2027 (Session 1)", "NCHM"},
		"neet:nta-notices": {"nta_home.html", 15,
			"Claims regarding OMR answer sheets of NEET (UG) 2026 being circulated on social media.", "JEE"},
		"ibps:crp-updates": {"ibps_crp_updates.html", 18,
			"Provisional Allotment under Reserve List for CRP-PO/MTs-XV", ""},
		"cisce:notifications": {"cisce_notifications.html", 12,
			"ICSE (Class X) Year 2027 Examination Timetable and Instructions for Candidates", "RFP"},
	}

	n := normalize.New()
	seen := 0
	for _, src := range s.List {
		if src.Rules.Format != normalize.FormatHTML {
			continue
		}
		w, ok := wants[src.Category]
		if !ok {
			t.Errorf("%s: page source without a fixture test", src.Category)
			continue
		}
		seen++
		t.Run(src.Category, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "normalize", "testdata", w.fixture))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := n.Normalize(context.Background(), src.Source, body, src.Rules)
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Items) != w.items {
				t.Errorf("%d items, want %d:\n%s", len(doc.Items), w.items, doc.Text)
			}
			if got := doc.Items[0].Headline; got != w.first {
				t.Errorf("newest = %q, want %q", got, w.first)
			}
			host := mustHost(t, src.URL)
			for _, it := range doc.Items {
				if it.CreatedAt == "" {
					t.Errorf("no date: %q", it.Headline)
				}
				if w.never != "" && strings.Contains(it.Headline, w.never) {
					t.Errorf("should be filtered out: %q", it.Headline)
				}
				// Discovery only follows attachments on the source's own
				// host; these sites keep their PDFs at home.
				for _, a := range it.Attachments {
					if mustHost(t, a.URL) != host {
						t.Errorf("attachment on another host: %s", a.URL)
					}
				}
			}
		})
	}
	if seen != len(wants) {
		t.Errorf("tested %d page sources, want %d", seen, len(wants))
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
