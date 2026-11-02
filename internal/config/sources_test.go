package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

var testNow = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// TestRepoSourcesFile checks the real sources.yaml, so a bad edit fails CI
// instead of failing at startup.
func TestRepoSourcesFile(t *testing.T) {
	s, err := LoadSources(filepath.Join("..", "..", "sources.yaml"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	// 6 feeds in English; only 3 of them exist in Hindi.
	if len(s.List) != 9 {
		t.Errorf("got %d sources, want 9", len(s.List))
	}
	langs := map[store.Language]int{}
	for _, src := range s.List {
		langs[src.Language]++
		if src.Kind != store.KindFeed || !src.Enabled || !src.DiscoverAttachments {
			t.Errorf("%s: unexpected settings %+v", src.Label, src)
		}
		if strings.Contains(src.Category, "calendar") {
			if src.Windowed || !strings.Contains(src.URL, "year=2026") {
				t.Errorf("calendar: windowed=%v url=%s", src.Windowed, src.URL)
			}
		} else if !src.Windowed {
			t.Errorf("%s should be windowed", src.Label)
		}
	}
	if langs[store.LangEnglish] != 6 || langs[store.LangHindi] != 3 {
		t.Errorf("languages = %v", langs)
	}
}

func TestExpandLanguages(t *testing.T) {
	s, err := ParseSources([]byte(`
sources:
  - label: Board
    kind: feed
    category: x:board
    languages: [en, hi]
    url: https://example.test/feed?language={lang}&year={year}
    interval: 15m
    drop_lines: ['^Visitors: \d+$']
`), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.List) != 2 {
		t.Fatalf("got %d sources, want 2", len(s.List))
	}
	en, hi := s.List[0], s.List[1]
	if en.URL != "https://example.test/feed?language=english&year=2026" || en.Label != "Board" || en.Language != store.LangEnglish {
		t.Errorf("en = %+v", en.Source)
	}
	if hi.URL != "https://example.test/feed?language=hindi&year=2026" || hi.Label != "Board (hindi)" || hi.Language != store.LangHindi {
		t.Errorf("hi = %+v", hi.Source)
	}
	if en.CheckInterval != 15*time.Minute || s.DiscoveredInterval != 6*time.Hour {
		t.Errorf("intervals = %v, %v", en.CheckInterval, s.DiscoveredInterval)
	}
	if len(en.Rules.DropLines) != 1 || !en.Rules.DropLines[0].MatchString("Visitors: 42") {
		t.Errorf("drop_lines not compiled: %v", en.Rules.DropLines)
	}
	if byURL := s.ByURL(); byURL[hi.URL].Label != hi.Label {
		t.Error("ByURL lookup failed")
	}
	// Both languages share one category, named without the "(hindi)" suffix.
	if cats := s.Categories(); len(cats) != 1 || cats[0] != (Category{ID: "x:board", Label: "Board"}) {
		t.Errorf("Categories() = %+v", cats)
	}
}

func TestCategoriesSkipDisabled(t *testing.T) {
	s, err := ParseSources([]byte(`
sources:
  - {label: Old, kind: feed, category: x:old, languages: [en], url: "https://example.test/old", interval: 1h, enabled: false}
  - {label: B, kind: feed, category: x:b, languages: [en], url: "https://example.test/b", interval: 1h}
  - {label: A, kind: feed, category: x:a, languages: [en], url: "https://example.test/a", interval: 1h}
`), testNow)
	if err != nil {
		t.Fatal(err)
	}
	// File order, not alphabetical; disabled categories are not offered.
	want := []Category{{"x:b", "B"}, {"x:a", "A"}}
	if got := s.Categories(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Categories() = %+v, want %+v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	const ok = "    kind: feed\n    category: c\n    languages: [en]\n    url: https://example.test/a\n    interval: 15m\n"
	tests := map[string]string{
		"unknown field":       "sources:\n  - label: A\n" + ok + "    intervall: 1h\n",
		"missing label":       "sources:\n  - kind: feed\n    category: c\n    languages: [en]\n    url: https://example.test/a\n    interval: 15m\n",
		"bad kind":            "sources:\n  - label: A\n    kind: rss\n    category: c\n    languages: [en]\n    url: https://example.test/a\n    interval: 15m\n",
		"bad language":        "sources:\n  - label: A\n    kind: feed\n    category: c\n    languages: [fr]\n    url: https://example.test/a\n    interval: 15m\n",
		"no lang placeholder": "sources:\n  - label: A\n    kind: feed\n    category: c\n    languages: [en, hi]\n    url: https://example.test/a\n    interval: 15m\n",
		"interval too short":  "sources:\n  - label: A\n    kind: feed\n    category: c\n    languages: [en]\n    url: https://example.test/a\n    interval: 10s\n",
		"relative url":        "sources:\n  - label: A\n    kind: feed\n    category: c\n    languages: [en]\n    url: /feed\n    interval: 15m\n",
		"unknown placeholder": "sources:\n  - label: A\n    kind: feed\n    category: c\n    languages: [en]\n    url: https://example.test/{page}\n    interval: 15m\n",
		"bad regex":           "sources:\n  - label: A\n" + ok + "    drop_lines: ['(']\n",
		"windowed pdf":        "sources:\n  - label: A\n    kind: pdf\n    category: c\n    languages: [en]\n    url: https://example.test/a\n    interval: 15m\n    windowed: true\n",
		"duplicate url":       "sources:\n  - label: A\n" + ok + "  - label: B\n" + ok,
		"empty":               "sources: []\n",
	}
	for name, yml := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSources([]byte(yml), testNow); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestSync(t *testing.T) {
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	parse := func(yml string) Sources {
		t.Helper()
		s, err := ParseSources([]byte(yml), testNow)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	two := parse("sources:\n" +
		"  - {label: A, kind: feed, category: c, languages: [en], url: 'https://example.test/a', interval: 15m}\n" +
		"  - {label: B, kind: feed, category: c, languages: [en], url: 'https://example.test/b', interval: 15m}\n")
	if n, err := Sync(t.Context(), st, two); err != nil || n != 0 {
		t.Fatalf("first sync: disabled=%d err=%v", n, err)
	}

	// B is removed from the file and A's interval changes.
	one := parse("sources:\n" +
		"  - {label: A, kind: feed, category: c, languages: [en], url: 'https://example.test/a', interval: 1h}\n")
	if n, err := Sync(t.Context(), st, one); err != nil || n != 1 {
		t.Fatalf("second sync: disabled=%d err=%v, want 1 disabled", n, err)
	}
	all, err := st.Sources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !all[0].Enabled || all[0].CheckInterval != time.Hour || all[1].Enabled {
		t.Errorf("after sync: %+v", all)
	}
}
