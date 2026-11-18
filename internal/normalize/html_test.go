package normalize

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/akkuraina/pehredaar/internal/differ"
	"github.com/akkuraina/pehredaar/internal/store"
)

const tableURL = "https://exams.example.test/portal/notices.html"

var tableRules = Rules{Format: FormatHTML, HTML: HTMLRules{
	Content: "main",
	Items:   "#notices tbody tr",
	Title:   "td.t",
	Date:    "td.d",
}}

func normalizeList(t *testing.T, body []byte, rules Rules) Doc {
	t.Helper()
	doc, err := New().Normalize(context.Background(),
		store.Source{URL: tableURL, Kind: store.KindFeed}, body, rules)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestHTMLListItems(t *testing.T) {
	doc := normalizeList(t, readFixture(t, "html_notice_table.html"), tableRules)

	type want struct {
		date, headline string
		atts, links    []string
	}
	wants := []want{
		{"2026-11-12T00:00:00+05:30", "Revised schedule for Session 2 (supersedes notice of 12.10.2026)",
			[]string{"https://exams.example.test/portal/docs/Revised%20Schedule.PDF"}, nil},
		{"2026-11-05T00:00:00+05:30", "Advisory regarding exam city intimation slip",
			[]string{"https://exams.example.test/portal/docs/Exam-City-Advisory.pdf"}, nil},
		{"2026-11-03T00:00:00+05:30", "Correction window for application forms. Apply online",
			nil, []string{"https://apply.example.test/register"}},
		{"2026-11-01T00:00:00+05:30", "Registration for the December exam is open. Apply online",
			nil, []string{"https://apply.example.test/register"}},
		{"2026-10-31T00:00:00+05:30", "fee-refund.pdf",
			[]string{"https://exams.example.test/portal/docs/fee-refund.pdf"}, nil},
	}
	if len(doc.Items) != len(wants) {
		t.Fatalf("got %d items, want %d:\n%s", len(doc.Items), len(wants), doc.Text)
	}
	ids := map[string]bool{}
	for i, w := range wants {
		it := doc.Items[i]
		if it.CreatedAt != w.date || it.Headline != w.headline {
			t.Errorf("item %d = %q %q, want %q %q", i, it.CreatedAt, it.Headline, w.date, w.headline)
		}
		var atts []string
		for _, a := range it.Attachments {
			atts = append(atts, a.URL)
		}
		if strings.Join(atts, " ") != strings.Join(w.atts, " ") || strings.Join(it.Links, " ") != strings.Join(w.links, " ") {
			t.Errorf("item %d links = %v %v, want %v %v", i, atts, it.Links, w.atts, w.links)
		}
		if len(it.ID) != 12 || ids[it.ID] {
			t.Errorf("item %d id %q is malformed or repeated", i, it.ID)
		}
		ids[it.ID] = true
	}

	// The text is the same line format as an SSC feed, so the differ and
	// the website read it without knowing where it came from.
	first := strings.Split(doc.Text, "\n")[0]
	if want := doc.Items[0].ID + " | 2026-11-12T00:00:00+05:30 | Revised schedule"; !strings.HasPrefix(first, want) {
		t.Errorf("first line = %q, want prefix %q", first, want)
	}
	for _, noise := range []string{"Visitors", "Last updated", "48213", "About us", "New", "mailto", "javascript"} {
		if strings.Contains(doc.Text, noise) {
			t.Errorf("canonical text contains noise %q:\n%s", noise, doc.Text)
		}
	}
}

// Anything outside the content area can change freely.
func TestHTMLListIgnoresChrome(t *testing.T) {
	body := string(readFixture(t, "html_notice_table.html"))
	a := normalizeList(t, []byte(body), tableRules)
	noisy := strings.NewReplacer(
		"48213", "48990",
		"17/11/2026 10:42:07", "18/11/2026 09:01:55",
		`<a href="/about">About us</a>`, `<a href="/about">About</a> <a href="/rti">RTI</a>`,
		`<td class="d">05/11/2026</td>`, "<td class=\"d\">\n\t05/11/2026  </td>",
	).Replace(body)
	b := normalizeList(t, []byte(noisy), tableRules)
	if a.Hash != b.Hash {
		t.Errorf("chrome-only changes altered the hash:\n%s\n---\n%s", a.Text, b.Text)
	}
}

// A reworded headline on the same PDF is an edit of that notice, not a
// new notice plus a removal; the alert then shows old → new.
func TestHTMLListEditKeepsID(t *testing.T) {
	body := string(readFixture(t, "html_notice_table.html"))
	a := normalizeList(t, []byte(body), tableRules)
	edited := strings.Replace(body, "Revised schedule for Session 2", "Revised schedule for Session 2 (exam postponed)", 1)
	b := normalizeList(t, []byte(edited), tableRules)

	d := differ.Feed(a.Text, b.Text, true)
	if len(d.Added) != 0 || len(d.Removed) != 0 || len(d.Edited) != 1 {
		t.Fatalf("diff = %+v, want exactly one edit", d)
	}

	// With key: title, the same headline over a new PDF is the edit.
	byTitle := tableRules
	byTitle.HTML.Key = KeyTitle
	a = normalizeList(t, []byte(body), byTitle)
	replaced := strings.Replace(body, "docs/Exam-City-Advisory.pdf", "docs/Exam-City-Advisory-v2.pdf", 1)
	b = normalizeList(t, []byte(replaced), byTitle)
	d = differ.Feed(a.Text, b.Text, true)
	if len(d.Edited) != 1 || len(d.Added)+len(d.Removed) != 0 {
		t.Fatalf("key title: diff = %+v, want exactly one edit", d)
	}
}

func TestHTMLListNewNoticeIsAdded(t *testing.T) {
	body := string(readFixture(t, "html_notice_table.html"))
	a := normalizeList(t, []byte(body), tableRules)
	added := strings.Replace(body, "<tbody>", `<tbody><tr><td class="t"><a href="docs/answer-key.pdf">Provisional answer key</a></td><td class="d">17/11/2026</td></tr>`, 1)
	b := normalizeList(t, []byte(added), tableRules)
	d := differ.Feed(a.Text, b.Text, true)
	if len(d.Added) != 1 || !strings.Contains(d.Added[0], "Provisional answer key") || len(d.Edited)+len(d.Removed) != 0 {
		t.Fatalf("diff = %+v, want the answer key added", d)
	}
	r, err := differ.Diff(store.KindFeed, a.Text, b.Text, differ.Options{Windowed: true})
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := differ.ParseFeed(r.Text)
	link, ok := AttachmentLink(tableURL, blocks[0].Fields[0])
	if !ok || link != "https://exams.example.test/portal/docs/answer-key.pdf" {
		t.Errorf("AttachmentLink(%q) = %q %v", blocks[0].Fields[0], link, ok)
	}
}

// Items without a date selector keep the page's order.
func TestHTMLListUndatedKeepsPageOrder(t *testing.T) {
	body := []byte(`<html><body><ul class="news">
		<li><a href="/b.pdf">Second</a></li>
		<li><a href="/a.pdf">First</a></li>
		<li><a href="/b.pdf">Second</a></li>
	</ul></body></html>`)
	doc := normalizeList(t, body, Rules{Format: FormatHTML, HTML: HTMLRules{Items: "ul.news li"}})
	if len(doc.Items) != 2 || doc.Items[0].Headline != "Second" || doc.Items[1].Headline != "First" {
		t.Fatalf("items = %+v, want Second, First (duplicate dropped)", doc.Items)
	}
}

func TestHTMLLegacyCharset(t *testing.T) {
	// windows-1252: 0x96 is an en dash, 0xE9 is é.
	body := []byte("<html><head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=windows-1252\"></head>" +
		"<body><ul><li><a href=\"/n.pdf\">Admit card \x96 Caf\xe9 exam</a></li></ul></body></html>")
	doc := normalizeList(t, body, Rules{Format: FormatHTML, HTML: HTMLRules{Items: "li"}})
	if got := doc.Items[0].Headline; got != "Admit card – Café exam" {
		t.Errorf("headline = %q", got)
	}
}

func TestHTMLNotContent(t *testing.T) {
	cases := map[string]struct {
		body  string
		rules Rules
	}{
		"json":            {`{"statusCode":"500"}`, tableRules},
		"empty body":      {``, tableRules},
		"content missing": {`<html><body><p>Under maintenance</p></body></html>`, tableRules},
		"no items":        {`<html><body><main><table id="notices"><tbody></tbody></table></main></body></html>`, tableRules},
		"page no text":    {`<html><body><div id="c"><img src="x.png"></div></body></html>`, Rules{HTML: HTMLRules{Content: "#c"}}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			kind := store.KindFeed
			if c.rules.Format == "" {
				kind = store.KindHTML
			}
			_, err := New().Normalize(context.Background(), store.Source{URL: tableURL, Kind: kind}, []byte(c.body), c.rules)
			if !errors.Is(err, ErrNotContent) {
				t.Errorf("err = %v, want ErrNotContent", err)
			}
		})
	}
}

func TestHTMLPage(t *testing.T) {
	body := []byte(`<html><body>
		<nav>Home | About</nav>
		<div id="content">
		  <h1>Important dates</h1>
		  <p>Registration: <b>1 Nov</b> to <b>20 Nov 2026</b></p>
		  <table><tr><td>Exam</td><td>15 Dec 2026</td></tr></table>
		  <div class="counter">Page views: 9912</div>
		  <p>Printed on 17/11/2026</p>
		</div></body></html>`)
	rules := Rules{
		HTML:      HTMLRules{Content: "#content", Drop: []string{".counter"}},
		DropLines: []*regexp.Regexp{regexp.MustCompile(`^Printed on `)},
	}
	doc, err := New().Normalize(context.Background(), store.Source{URL: tableURL, Kind: store.KindHTML}, body, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := "Important dates\nRegistration: 1 Nov to 20 Nov 2026\nExam\n15 Dec 2026"
	if doc.Text != want {
		t.Errorf("text =\n%s\nwant\n%s", doc.Text, want)
	}
}

func TestParseDate(t *testing.T) {
	cases := map[string]string{
		"05/11/2026":         "2026-11-05",
		"5-11-2026":          "2026-11-05",
		"05.11.2026":         "2026-11-05",
		"Dated: 05/11/2026":  "2026-11-05",
		"5 Nov 2026":         "2026-11-05",
		"05-Nov-2026":        "2026-11-05",
		"5th November, 2026": "2026-11-05",
		"November 5, 2026":   "2026-11-05",
		"Nov. 5th 2026":      "2026-11-05",
		"2026-11-05":         "2026-11-05",
		"31/02/2026":         "", // no such day
		"13/13/2026":         "",
		"no date here":       "",
		"Session 2, 2026":    "",
		"12/11/26":           "",           // two-digit years are ambiguous; not guessed
		"01 Jul 26":          "2026-07-01", // ...unless a month name makes them clear (IBPS)
		"30 Jun 26":          "2026-06-30",
		"published 1/1/1890": "",
	}
	for in, want := range cases {
		got := ParseDate(in)
		if want != "" {
			want += "T00:00:00+05:30"
		}
		if got != want {
			t.Errorf("ParseDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTMLRulesValidate(t *testing.T) {
	bad := map[string]struct {
		r    HTMLRules
		list bool
	}{
		"list without items": {HTMLRules{}, true},
		"items on a page":    {HTMLRules{Items: "li"}, false},
		"bad selector":       {HTMLRules{Items: "li[", Title: "a"}, true},
		"bad drop":           {HTMLRules{Drop: []string{">>"}}, false},
		"unknown key":        {HTMLRules{Items: "li", Key: "id"}, true},
		"link_date no group": {HTMLRules{Items: "li", LinkDate: regexp.MustCompile(`\d{8}`)}, true},
		"max_items undated":  {HTMLRules{Items: "li", MaxItems: 10}, true},
		"filter on a page":   {HTMLRules{Include: regexp.MustCompile(`JEE`)}, false},
	}
	for name, c := range bad {
		if err := c.r.Validate(c.list); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c.r)
		}
	}
	if err := tableRules.HTML.Validate(true); err != nil {
		t.Errorf("valid rules rejected: %v", err)
	}
}

func TestLinkDate(t *testing.T) {
	re := regexp.MustCompile(`(?i)Notice_(\d{8,14})\.pdf`)
	cases := map[string]string{
		"https://x.test/Download/Notice/Notice_20261117205801.pdf": "2026-11-17T20:58:01+05:30",
		"https://x.test/Download/Notice/Notice_20261117.PDF":       "2026-11-17T00:00:00+05:30",
		"https://x.test/Download/Notice/Notice_20261340000000.pdf": "", // month 13
		"https://x.test/Download/Notice/Notice_2026111720.pdf":     "", // 10 digits: no layout
		"https://x.test/other.pdf":                                 "",
	}
	for link, want := range cases {
		it := FeedItem{Attachments: []Attachment{{URL: link}}}
		if got := linkDate(it, re); got != want {
			t.Errorf("linkDate(%s) = %q, want %q", link, got, want)
		}
	}
}

// SSC attachment fields still resolve to SSC's download URL.
func TestAttachmentLinkBothShapes(t *testing.T) {
	ssc := "https://ssc.gov.in/api/general-website/portal/notice-boards?page=1"
	if u, ok := AttachmentLink(ssc, "att: uploads/a b (1).pdf (2048 B)"); !ok || u != "https://ssc.gov.in/api/attachment/uploads/a%20b%20%281%29.pdf" {
		t.Errorf("ssc field: %q %v", u, ok)
	}
	if u, ok := AttachmentLink(ssc, "att: https://nta.example.test/x.pdf"); !ok || u != "https://nta.example.test/x.pdf" {
		t.Errorf("url field: %q %v", u, ok)
	}
	if _, ok := AttachmentLink(ssc, "link: https://x.test/"); ok {
		t.Error("link field taken as attachment")
	}
}
