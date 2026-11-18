package normalize

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// The page reader. Most exam bodies publish notices as an ordinary web
// page: a table or list of headlines, each linking to a PDF. Two shapes are
// supported, both configured per source in sources.yaml:
//
//   - A notice list (kind: feed, format: html). Each element matching
//     HTMLRules.Items becomes one FeedItem, rendered in the same canonical
//     line format as an SSC feed item. Everything downstream (the feed
//     differ, alert summaries, PDF discovery, the website's cards) then
//     works unchanged, which is the point: a notice board is a notice
//     board, whether it arrives as JSON or as HTML.
//   - A plain page (kind: html). The text of HTMLRules.Content, one line
//     per block element, diffed line by line like a PDF.
//
// Noise is handled by selection rather than by cleanup: the rules name the
// part of the page that matters (the notice table), so menus, counters,
// tickers and "last updated" stamps outside it are never read at all.

// HTMLRules says where the content is on a page. Selectors are CSS
// (cascadia: the same syntax as document.querySelectorAll).
type HTMLRules struct {
	// Content limits reading to the matching elements; default "body".
	Content string
	// Drop removes matching elements before anything is read: a
	// "new!" gif, a visitor counter, a ticker inside the content area.
	Drop []string

	// The rest apply only to notice lists (format: html).

	// Items matches one element per notice, e.g. "table.notices tr".
	Items string
	// Title, inside an item, holds its headline. Default: the item's text
	// (minus the date element, if Date is set).
	Title string
	// Date, inside an item, holds its publication date. Without it items
	// keep page order and the feed can't be windowed.
	Date string
	// Key decides what identifies a notice across fetches: "link" (the
	// default) or "title". With "link", a reworded headline is an edit of
	// the same notice; with "title", a replaced PDF is.
	Key string
}

// Item keys.
const (
	KeyLink  = "link"
	KeyTitle = "title"
)

// Validate checks every selector parses, so a typo in sources.yaml fails
// at startup instead of silently matching nothing at every check.
func (r HTMLRules) Validate(list bool) error {
	sels := map[string]string{"content": r.Content, "items": r.Items, "title": r.Title, "date": r.Date}
	for i, d := range r.Drop {
		sels[fmt.Sprintf("drop[%d]", i)] = d
	}
	for name, sel := range sels {
		if sel == "" {
			continue
		}
		if _, err := cascadia.ParseGroup(sel); err != nil {
			return fmt.Errorf("%s selector %q: %w", name, sel, err)
		}
	}
	switch {
	case list && r.Items == "":
		return errors.New("a format: html feed needs an items selector")
	case !list && (r.Items != "" || r.Title != "" || r.Date != "" || r.Key != ""):
		return errors.New("items, title, date and key only apply to format: html feeds")
	case r.Key != "" && r.Key != KeyLink && r.Key != KeyTitle:
		return fmt.Errorf("key %q must be %s or %s", r.Key, KeyLink, KeyTitle)
	}
	return nil
}

// parseHTML reads body (in whatever charset it declares; many government
// sites still serve windows-1252 or declare it in a meta tag) and returns
// the document with the Drop elements removed.
func parseHTML(body []byte, pageURL string, r HTMLRules) (*goquery.Document, *url.URL, error) {
	if !looksLikeHTML(body) {
		return nil, nil, fmt.Errorf("%w: not an HTML page", ErrNotContent)
	}
	rd, err := charset.NewReader(bytes.NewReader(body), "")
	if err != nil {
		return nil, nil, fmt.Errorf("%w: unknown charset: %v", ErrNotContent, err)
	}
	doc, err := goquery.NewDocumentFromReader(rd)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: parsing html: %v", ErrNotContent, err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, nil, fmt.Errorf("normalize: bad page URL %q: %w", pageURL, err)
	}
	// Relative links resolve against <base href> when the page sets one.
	if href, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if b, err := base.Parse(strings.TrimSpace(href)); err == nil {
			base = b
		}
	}
	doc.Find(strings.Join(slices.Concat(alwaysDrop, r.Drop), ", ")).Remove()
	return doc, base, nil
}

// alwaysDrop never carries readable text.
var alwaysDrop = []string{"script", "style", "noscript", "template", "svg", "iframe", "object", "head"}

// looksLikeHTML rejects bodies that are obviously something else (a PDF
// or JSON served at a page URL) before the lenient HTML parser turns them
// into a "page" of garbage.
func looksLikeHTML(body []byte) bool {
	head := bytes.TrimPrefix(body[:min(len(body), 1024)], []byte("\xef\xbb\xbf"))
	head = bytes.ToLower(bytes.TrimSpace(head))
	return bytes.HasPrefix(head, []byte("<")) && !bytes.HasPrefix(head, []byte("<?xml version")) ||
		bytes.Contains(head, []byte("<html"))
}

// content returns the elements to read, or ErrNotContent when the content
// selector matches nothing: the page was redesigned, or this is an error
// page. Either way it must not be stored as "the notices are all gone".
func content(doc *goquery.Document, r HTMLRules) (*goquery.Selection, error) {
	sel := cmp.Or(r.Content, "body")
	s := doc.Find(sel)
	if s.Length() == 0 {
		return nil, fmt.Errorf("%w: content selector %q matches nothing", ErrNotContent, sel)
	}
	return s, nil
}

// normalizeHTMLPage renders a page's content as text, one line per block.
func normalizeHTMLPage(body []byte, pageURL string, rules Rules) (Doc, error) {
	doc, _, err := parseHTML(body, pageURL, rules.HTML)
	if err != nil {
		return Doc{}, err
	}
	sel, err := content(doc, rules.HTML)
	if err != nil {
		return Doc{}, err
	}
	var b strings.Builder
	for _, n := range sel.Nodes {
		writeText(&b, n)
		b.WriteByte('\n')
	}
	text := CleanText(b.String(), rules.DropLines)
	if text == "" {
		return Doc{}, fmt.Errorf("%w: page has no text", ErrNotContent)
	}
	return Doc{Text: text}, nil
}

// normalizeHTMLList reads a notice list: one FeedItem per Items element.
func normalizeHTMLList(body []byte, pageURL string, rules Rules) (Doc, error) {
	r := rules.HTML
	doc, base, err := parseHTML(body, pageURL, r)
	if err != nil {
		return Doc{}, err
	}
	area, err := content(doc, r)
	if err != nil {
		return Doc{}, err
	}

	type raw struct {
		it  FeedItem
		key string
	}
	var raws []raw
	area.Find(r.Items).Each(func(_ int, s *goquery.Selection) {
		it := FeedItem{}
		if r.Date != "" {
			d := s.Find(r.Date).First()
			it.CreatedAt = ParseDate(inlineText(d))
			if r.Title == "" {
				// The headline is the rest of the item; work on a copy so
				// the date's own text doesn't end up in it.
				s = s.Clone()
				s.Find(r.Date).First().Remove()
			}
		}
		title := s
		if r.Title != "" {
			title = s.Find(r.Title).First()
		}
		it.Headline = inlineText(title)
		addLinks(&it, s, base)
		if it.Headline == "" && len(it.Attachments)+len(it.Links) == 0 {
			return // spacer rows, empty <li>s, header rows of just <th>s
		}
		if it.Headline == "" {
			// A bare link (an icon, "click here" in its own cell): name the
			// item after the file so the alert still says something.
			it.Headline = firstLinkName(it)
		}
		raws = append(raws, raw{it: it, key: itemKey(it, r.Key)})
	})
	if len(raws) == 0 {
		return Doc{}, fmt.Errorf("%w: items selector %q matches no notices", ErrNotContent, r.Items)
	}

	// Two notices can share a key (both link to the same "apply online"
	// page). Telling them apart by headline as well keeps their ids
	// distinct; it only applies to the colliding ones, so every other id
	// stays as stable as its key.
	count := map[string]int{}
	for _, x := range raws {
		count[x.key]++
	}
	seen := map[string]bool{}
	items := make([]FeedItem, 0, len(raws))
	for _, x := range raws {
		key := x.key
		if count[key] > 1 {
			key += "\x00" + x.it.Headline
		}
		x.it.ID = shortHash(key)
		if seen[x.it.ID] {
			continue // the same notice listed twice (e.g. in "latest" and in a table)
		}
		seen[x.it.ID] = true
		items = append(items, x.it)
	}

	// Newest first when dates are known; a stable sort keeps the page's own
	// order among equal (or missing) dates, so the result depends only on
	// the page, never on map iteration or parser internals.
	slices.SortStableFunc(items, func(a, b FeedItem) int { return cmp.Compare(b.CreatedAt, a.CreatedAt) })

	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.canonicalLine())
	}
	text := CleanText(strings.Join(lines, "\n"), rules.DropLines)
	return Doc{Text: text, Items: items}, nil
}

// addLinks records an item's links: PDFs as attachments (so discovery
// watches them and alerts link them as documents), anything else as links.
func addLinks(it *FeedItem, s *goquery.Selection, base *url.URL) {
	seen := map[string]bool{}
	s.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		u, ok := resolve(base, href)
		if !ok || seen[u] {
			return
		}
		seen[u] = true
		if isPDF(u) {
			it.Attachments = append(it.Attachments, Attachment{Path: u, URL: u, FileName: inlineText(a)})
		} else {
			it.Links = append(it.Links, u)
		}
	})
}

// resolve makes href absolute. Fragments are dropped (the same page
// linked with two anchors is one link), and only http(s) links are kept.
func resolve(base *url.URL, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return "", false
	}
	u, err := base.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false // javascript:, mailto:, tel:, garbage
	}
	u.Fragment = ""
	return u.String(), true
}

func isPDF(u string) bool {
	p, err := url.Parse(u)
	return err == nil && strings.EqualFold(path.Ext(p.Path), ".pdf")
}

func itemKey(it FeedItem, key string) string {
	if key != KeyTitle {
		if len(it.Attachments) > 0 {
			return it.Attachments[0].URL
		}
		if len(it.Links) > 0 {
			return it.Links[0]
		}
	}
	return it.Headline
}

func firstLinkName(it FeedItem) string {
	u := ""
	if len(it.Attachments) > 0 {
		u = it.Attachments[0].URL
	} else {
		u = it.Links[0]
	}
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	name, _ := url.PathUnescape(path.Base(p.Path))
	return cmp.Or(name, u)
}

// shortHash is a stable item id: 12 hex digits (48 bits) is plenty for
// the few hundred items a page lists, and keeps canonical lines readable.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// inlineText is a selection's text on one line.
func inlineText(s *goquery.Selection) string {
	var b strings.Builder
	for _, n := range s.Nodes {
		writeText(&b, n)
	}
	return oneLine(b.String())
}

// writeText writes n's text, with a newline around every block-level
// element and <br>, so "<td>Notice</td><td>12/11/2026</td>" reads as two
// words rather than "Notice12/11/2026". goquery's Text() just concatenates
// text nodes, which glues cells and list items together.
func writeText(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.ElementNode:
		if n.Data == "br" {
			b.WriteByte('\n')
			return
		}
		if n.Data == "img" {
			// Some sites mark new notices with an image whose alt is the
			// only text ("New"). It is decoration, not content.
			return
		}
	case html.DocumentNode:
	default:
		return // comments, doctypes
	}
	block := n.Type == html.ElementNode && blockElements[n.Data]
	if block {
		b.WriteByte('\n')
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		writeText(b, c)
	}
	if block {
		b.WriteByte('\n')
	}
}

var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "dd": true,
	"details": true, "dialog": true, "div": true, "dl": true, "dt": true,
	"fieldset": true, "figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hr": true, "li": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "summary": true, "table": true,
	"tbody": true, "thead": true, "tfoot": true, "tr": true, "td": true, "th": true,
	"caption": true, "ul": true, "option": true, "body": true, "html": true,
}

// --- dates -----------------------------------------------------------------------

// Indian sites write dates day first: 05/11/2026, 05-11-2026, 05.11.2026,
// 5 Nov 2026, 05-Nov-2026, 5th November, 2026. ISO 2026-11-05 also
// appears. Month-first (US) order is never assumed: 05/11 is 5 November.
var (
	dmyNumRE  = regexp.MustCompile(`\b(\d{1,2})[./-](\d{1,2})[./-](\d{4})\b`)
	isoRE     = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	dmyNameRE = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?[\s./-]*(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?[\s,./-]*(\d{4})\b`)
	mdyNameRE = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
)

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
	"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

var istZone = time.FixedZone("IST", 5*3600+1800)

// ParseDate finds the first date in s and returns it as RFC 3339 midnight
// IST (the shape SSC's createdAt has, so feeds of both kinds sort and
// display alike), or "" if s holds no valid date.
func ParseDate(s string) string {
	var y, m, d int
	switch {
	case isoRE.MatchString(s):
		g := isoRE.FindStringSubmatch(s)
		y, m, d = atoi(g[1]), atoi(g[2]), atoi(g[3])
	case dmyNumRE.MatchString(s):
		g := dmyNumRE.FindStringSubmatch(s)
		d, m, y = atoi(g[1]), atoi(g[2]), atoi(g[3])
	case dmyNameRE.MatchString(s):
		g := dmyNameRE.FindStringSubmatch(s)
		d, m, y = atoi(g[1]), int(months[strings.ToLower(g[2])]), atoi(g[3])
	case mdyNameRE.MatchString(s):
		g := mdyNameRE.FindStringSubmatch(s)
		m, d, y = int(months[strings.ToLower(g[1])]), atoi(g[2]), atoi(g[3])
	default:
		return ""
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, istZone)
	// time.Date normalizes 31/02 into March; a date that doesn't survive
	// the round trip wasn't a date.
	if t.Day() != d || int(t.Month()) != m || y < 1990 || y > 2100 {
		return ""
	}
	return t.Format(time.RFC3339)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
