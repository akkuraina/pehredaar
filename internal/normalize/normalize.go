// Package normalize turns a fetched body into canonical text: the exact
// string Pehredaar hashes and diffs. Its job is to make "nothing meaningful
// changed" produce byte-identical output, so that noise (field order,
// whitespace, pagination metadata, page numbers) never looks like a change.
package normalize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/akkuraina/pehredaar/internal/store"
)

var (
	// ErrNotContent means the body is not the content we expected: an error
	// page, an API error envelope, an empty feed. It must be treated as a
	// failed check, never stored as a snapshot; otherwise an outage followed
	// by recovery would look like two changes.
	ErrNotContent = errors.New("normalize: response is not usable content")

	// ErrNoText means a document had no extractable text, typically a
	// scanned PDF that would need OCR.
	ErrNoText = errors.New("normalize: document has no extractable text")
)

// Doc is the normalized form of one fetch.
type Doc struct {
	Text  string     // canonical text; what we hash and diff
	Hash  string     // hex SHA-256 of Text
	Items []FeedItem // feeds only, in the same order as Text's lines
}

// FeedItem is one entry of a notice feed.
type FeedItem struct {
	ID          string
	Headline    string
	Desc        string // SSC calendar: exam tier
	Content     string // SSC calendar: exam schedule
	CreatedAt   string // as the API reports it (RFC 3339)
	StartDate   string
	EndDate     string
	RedirectURL string
	Attachments []Attachment
}

// Attachment is a document linked from a feed item.
type Attachment struct {
	Path     string // as published, with forward slashes
	URL      string // absolute download URL
	FileName string
	Size     int64
}

// Rules are per-source noise filters, configured in sources.yaml.
type Rules struct {
	// DropLines removes any canonical line matching one of these patterns,
	// e.g. a "Visitors: 12345" counter or a print timestamp.
	DropLines []*regexp.Regexp
}

// defaultPDFDrops are noise lines common to PDFs regardless of source.
var defaultPDFDrops = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^page \d+ of \d+$`),
}

// Normalizer holds the configuration for external tools.
type Normalizer struct {
	PDFToText  string        // path to pdftotext; "pdftotext" uses PATH
	PDFTimeout time.Duration // per document
}

// New returns a Normalizer that uses pdftotext from PATH.
func New() *Normalizer {
	return &Normalizer{PDFToText: "pdftotext", PDFTimeout: 30 * time.Second}
}

// Normalize converts body, fetched from src, into canonical form.
func (n *Normalizer) Normalize(ctx context.Context, src store.Source, body []byte, rules Rules) (Doc, error) {
	var (
		doc Doc
		err error
	)
	switch src.Kind {
	case store.KindFeed:
		// SSC is the only feed format so far. When a second exam family
		// arrives, sources.yaml grows a `format:` field and we switch on it.
		doc, err = normalizeSSCFeed(body, src.URL, rules)
	case store.KindPDF:
		doc, err = n.normalizePDF(ctx, body, rules)
	case store.KindHTML:
		err = errors.New("normalize: html sources are not implemented yet")
	default:
		err = fmt.Errorf("normalize: unknown source kind %q", src.Kind)
	}
	if err != nil {
		return Doc{}, err
	}
	doc.Hash = hashText(doc.Text)
	return doc, nil
}

// CleanText applies the whitespace and Unicode canonicalization shared by
// every source kind, then drops lines matching any of drop:
//
//   - Unicode NFC, so the same Devanagari or accented text encoded two ways
//     compares equal;
//   - form feeds (PDF page breaks) and CR/CRLF become plain newlines;
//   - byte-order marks and soft hyphens are removed;
//   - within a line, every run of whitespace (including non-breaking space)
//     becomes one space, and lines are trimmed;
//   - empty lines are removed.
//
// Collapsing internal whitespace means a column shifting by a few spaces in
// `pdftotext -layout` output is not a change, while each table row still
// stays on its own line.
func CleanText(s string, drop []*regexp.Regexp) string {
	s = norm.NFC.String(s)
	s = textReplacer.Replace(s)

	lines := strings.Split(s, "\n")
	out := lines[:0] // reuse the backing array; we only ever shrink
	for _, line := range lines {
		// strings.Fields splits on unicode.IsSpace, which includes U+00A0.
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || matchesAny(drop, line) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

var textReplacer = strings.NewReplacer(
	"\r\n", "\n",
	"\r", "\n",
	"\f", "\n",
	"\ufeff", "", // byte-order mark
	"\u00ad", "", // soft hyphen
)

func matchesAny(res []*regexp.Regexp, s string) bool {
	return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(s) })
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
