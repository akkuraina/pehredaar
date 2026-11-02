// Package config loads Pehredaar's settings: the curated source list from
// sources.yaml and runtime settings from environment variables.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Source is one curated source after expansion: the part the store keeps,
// plus per-source settings that live only in sources.yaml.
type Source struct {
	store.Source // URL, Kind, Category, Language, Label, CheckInterval, Enabled

	Windowed            bool // feed is one page of a longer list; see differ.Options
	DiscoverAttachments bool // watch PDFs linked from the feed's items
	Rules               normalize.Rules
}

// Sources is the parsed sources.yaml.
type Sources struct {
	List []Source

	// DiscoveredInterval is how often PDFs found in feeds are re-checked.
	DiscoveredInterval time.Duration
}

// ByURL indexes the curated sources by URL, which is how the scheduler
// finds the yaml-only settings for a store.Source.
func (s Sources) ByURL() map[string]Source {
	m := make(map[string]Source, len(s.List))
	for _, src := range s.List {
		m[src.URL] = src
	}
	return m
}

// The yaml shapes. They are separate from Source so the file format can
// use friendly types (a language list, "15m" durations, regex strings)
// while the rest of the program gets validated, ready-to-use values.
type fileYAML struct {
	DiscoveredInterval string       `yaml:"discovered_interval"`
	Sources            []sourceYAML `yaml:"sources"`
}

type sourceYAML struct {
	Label               string   `yaml:"label"`
	Kind                string   `yaml:"kind"`
	Category            string   `yaml:"category"`
	Languages           []string `yaml:"languages"`
	URL                 string   `yaml:"url"`
	Interval            string   `yaml:"interval"`
	Enabled             *bool    `yaml:"enabled"` // pointer: absent means true
	Windowed            bool     `yaml:"windowed"`
	DiscoverAttachments bool     `yaml:"discover_attachments"`
	DropLines           []string `yaml:"drop_lines"`
}

// Placeholders in source URLs.
var langParam = map[store.Language]string{
	store.LangEnglish: "english",
	store.LangHindi:   "hindi",
}

const minInterval = time.Minute

// LoadSources reads and validates a sources file. now fills the {year}
// placeholder.
func LoadSources(path string, now time.Time) (Sources, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sources{}, fmt.Errorf("config: %w", err)
	}
	s, err := ParseSources(data, now)
	if err != nil {
		return Sources{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return s, nil
}

// ParseSources parses sources.yaml content. Each entry expands into one
// source per language, replacing {lang} with the site's language name and
// {year} with now's year.
func ParseSources(data []byte, now time.Time) (Sources, error) {
	var f fileYAML
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a typo like "intervall:" is an error, not silently ignored
	if err := dec.Decode(&f); err != nil {
		return Sources{}, err
	}

	out := Sources{DiscoveredInterval: 6 * time.Hour}
	if f.DiscoveredInterval != "" {
		d, err := parseInterval(f.DiscoveredInterval)
		if err != nil {
			return Sources{}, fmt.Errorf("discovered_interval: %w", err)
		}
		out.DiscoveredInterval = d
	}

	seen := make(map[string]string) // URL -> label, to catch duplicates
	for i, y := range f.Sources {
		srcs, err := expand(y, now)
		if err != nil {
			name := y.Label
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			return Sources{}, fmt.Errorf("source %s: %w", name, err)
		}
		for _, s := range srcs {
			if other, dup := seen[s.URL]; dup {
				return Sources{}, fmt.Errorf("sources %q and %q have the same URL %s", other, s.Label, s.URL)
			}
			seen[s.URL] = s.Label
			out.List = append(out.List, s)
		}
	}
	if len(out.List) == 0 {
		return Sources{}, errors.New("no sources defined")
	}
	return out, nil
}

func expand(y sourceYAML, now time.Time) ([]Source, error) {
	kind := store.Kind(y.Kind)
	switch {
	case y.Label == "":
		return nil, errors.New("label is required")
	case kind != store.KindFeed && kind != store.KindPDF && kind != store.KindHTML:
		return nil, fmt.Errorf("kind %q must be feed, pdf or html", y.Kind)
	case y.Category == "":
		return nil, errors.New("category is required")
	case len(y.Languages) == 0:
		return nil, errors.New("languages is required, e.g. [en, hi]")
	case len(y.Languages) > 1 && !strings.Contains(y.URL, "{lang}"):
		return nil, errors.New("url needs a {lang} placeholder when several languages are listed")
	case kind != store.KindFeed && (y.Windowed || y.DiscoverAttachments):
		return nil, errors.New("windowed and discover_attachments only apply to feeds")
	}

	interval, err := parseInterval(y.Interval)
	if err != nil {
		return nil, fmt.Errorf("interval: %w", err)
	}
	var rules normalize.Rules
	for _, pat := range y.DropLines {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("drop_lines: %w", err)
		}
		rules.DropLines = append(rules.DropLines, re)
	}
	enabled := y.Enabled == nil || *y.Enabled

	var out []Source
	for _, l := range y.Languages {
		lang := store.Language(l)
		param, ok := langParam[lang]
		if !ok {
			return nil, fmt.Errorf("language %q must be en or hi", l)
		}
		u := strings.NewReplacer("{lang}", param, "{year}", strconv.Itoa(now.Year())).Replace(y.URL)
		if err := checkURL(u); err != nil {
			return nil, err
		}
		label := y.Label
		if len(y.Languages) > 1 && lang != store.LangEnglish {
			label += " (" + param + ")"
		}
		out = append(out, Source{
			Source: store.Source{
				URL:           u,
				Kind:          kind,
				Category:      y.Category,
				Language:      lang,
				Label:         label,
				CheckInterval: interval,
				Enabled:       enabled,
			},
			Windowed:            y.Windowed,
			DiscoverAttachments: y.DiscoverAttachments,
			Rules:               rules,
		})
	}
	return out, nil
}

func parseInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("is required, e.g. 15m")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	// Politeness floor: nothing on a government site changes minute by minute.
	if d < minInterval {
		return 0, fmt.Errorf("%v is shorter than the %v minimum", d, minInterval)
	}
	return d, nil
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http(s) URL", s)
	}
	if strings.ContainsAny(s, "{}") {
		return fmt.Errorf("url %q has an unknown placeholder", s)
	}
	return nil
}

// Sync makes the store's curated sources match the file: each source is
// upserted (keeping its polling state), and curated sources no longer in
// the file are disabled rather than deleted, so their history survives.
// It returns the number of sources disabled.
func Sync(ctx context.Context, st store.Store, s Sources) (int64, error) {
	urls := make([]string, 0, len(s.List))
	for _, src := range s.List {
		if _, err := st.UpsertSource(ctx, src.Source); err != nil {
			return 0, err
		}
		urls = append(urls, src.URL)
	}
	return st.DisableCuratedExcept(ctx, urls)
}
