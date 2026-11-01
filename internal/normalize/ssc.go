package normalize

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// sscEnvelope is the shape of an ssc.gov.in portal API response, as
// observed on 2026-10-30. Only the fields we use are declared: encoding/json
// ignores the rest (isCache, paginate, ...), which is the first layer of
// noise filtering for free.
type sscEnvelope struct {
	StatusCode string    `json:"statusCode"`
	Message    string    `json:"statusMessage"`
	Error      string    `json:"error"`
	Data       []sscItem `json:"data"`
}

type sscItem struct {
	ID          string          `json:"id"`
	Headline    string          `json:"headline"`
	Desc        string          `json:"desc"`    // calendar: tier, e.g. "Tier-I (CBE)"
	Content     string          `json:"content"` // calendar: exam schedule, e.g. "May – June 2026"
	CreatedAt   string          `json:"createdAt"`
	StartDate   *string         `json:"startDate"` // null in most items
	EndDate     *string         `json:"endDate"`
	RedirectURL string          `json:"redirectUrl"`
	Attachments []sscAttachment `json:"attachments"`
}

type sscAttachment struct {
	FileName string `json:"fileName"`
	Path     string `json:"path"` // Windows-style: uploads\masterData\...
	Size     int64  `json:"size"`
}

// normalizeSSCFeed parses an SSC portal feed and renders one canonical line
// per item. Fields that are internal bookkeeping (attachment ids, the
// language/contentType echo) are deliberately left out.
func normalizeSSCFeed(body []byte, feedURL string, rules Rules) (Doc, error) {
	var env sscEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// Typically an HTML error page served with status 200.
		return Doc{}, fmt.Errorf("%w: ssc feed is not JSON: %v", ErrNotContent, err)
	}
	if env.StatusCode != "200" {
		// e.g. {"statusCode":"203","error":"Query params are missing"}
		return Doc{}, fmt.Errorf("%w: ssc api status %q: %s%s", ErrNotContent,
			env.StatusCode, env.Message, env.Error)
	}
	if len(env.Data) == 0 {
		// An SSC feed is never legitimately empty; an empty one is far more
		// likely a backend hiccup than every notice being withdrawn.
		return Doc{}, fmt.Errorf("%w: ssc feed has no items", ErrNotContent)
	}

	base, err := url.Parse(feedURL)
	if err != nil {
		return Doc{}, fmt.Errorf("normalize: bad feed URL %q: %w", feedURL, err)
	}

	items := make([]FeedItem, 0, len(env.Data))
	for _, raw := range env.Data {
		if raw.ID == "" {
			continue
		}
		it := FeedItem{
			ID:          raw.ID,
			Headline:    oneLine(raw.Headline),
			Desc:        oneLine(raw.Desc),
			Content:     oneLine(raw.Content),
			CreatedAt:   raw.CreatedAt,
			StartDate:   deref(raw.StartDate),
			EndDate:     deref(raw.EndDate),
			RedirectURL: strings.TrimSpace(raw.RedirectURL),
		}
		for _, a := range raw.Attachments {
			p := strings.ReplaceAll(strings.TrimSpace(a.Path), `\`, "/")
			if p == "" {
				continue
			}
			it.Attachments = append(it.Attachments, Attachment{
				Path:     p,
				URL:      sscAttachmentURL(base, p),
				FileName: oneLine(a.FileName),
				Size:     a.Size,
			})
		}
		slices.SortFunc(it.Attachments, func(a, b Attachment) int { return cmp.Compare(a.Path, b.Path) })
		items = append(items, it)
	}

	// Newest first (as the site shows them), ties broken by id, so the
	// order depends only on content and never on the API's response order.
	slices.SortFunc(items, func(a, b FeedItem) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})

	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.canonicalLine())
	}
	// Feed lines are already clean; CleanText adds NFC and DropLines rules.
	text := CleanText(strings.Join(lines, "\n"), rules.DropLines)
	return Doc{Text: text, Items: items}, nil
}

// canonicalLine renders an item as "id | created | headline | extras...".
// Empty optional fields are omitted so that, e.g., a null startDate and a
// missing one look the same.
func (it FeedItem) canonicalLine() string {
	parts := []string{it.ID, it.CreatedAt, it.Headline}
	// Labels reuse SSC's own field names rather than guessing at meaning.
	if it.Desc != "" {
		parts = append(parts, "desc: "+it.Desc)
	}
	if it.Content != "" {
		parts = append(parts, "content: "+it.Content)
	}
	if it.StartDate != "" {
		parts = append(parts, "start: "+it.StartDate)
	}
	if it.EndDate != "" {
		parts = append(parts, "end: "+it.EndDate)
	}
	if it.RedirectURL != "" {
		parts = append(parts, "link: "+it.RedirectURL)
	}
	for _, a := range it.Attachments {
		parts = append(parts, fmt.Sprintf("att: %s (%d B)", a.Path, a.Size))
	}
	return strings.Join(parts, " | ")
}

// sscAttachmentURL builds the download URL for an attachment path. The
// "/api/attachment/" prefix is what the SSC site itself uses (seen in its
// JS bundle and verified by downloading). Each segment is escaped because
// file names contain spaces.
func sscAttachmentURL(base *url.URL, path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return base.Scheme + "://" + base.Host + "/api/attachment/" + strings.Join(segs, "/")
}

// oneLine collapses all whitespace, including embedded newlines (some SSC
// headlines contain them), into single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
