// Package differ compares two canonical texts produced by package normalize
// and describes what changed, in a form that is both stored with the change
// and handed to the LLM.
//
// The normalizer already makes "nothing changed" byte-identical, so the
// differ only runs when hashes differ. Its noise filtering is the second
// line of defence: changes that are real at the byte level but meaningless
// to a reader (a feed page scrolling, a PDF paragraph re-wrapping).
package differ

import (
	"cmp"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/akkuraina/pehredaar/internal/store"
)

// DefaultMaxBytes caps Result.Text. A diff bigger than this is a rewrite,
// and the LLM summarizes it from the first few blocks just as well.
const DefaultMaxBytes = 4 << 10

// Options tune a diff for one source.
type Options struct {
	// Windowed means the feed is one page of a longer list (SSC feeds are
	// fetched with limit=10), so items scroll in and out of it without
	// being added or deleted. False for complete lists like the calendar.
	Windowed bool

	// MaxBytes caps Result.Text; 0 means DefaultMaxBytes.
	MaxBytes int
}

// Result is the outcome of one diff.
type Result struct {
	Kind      store.ChangeKind
	Text      string // readable diff, at most MaxBytes plus a short note
	Truncated bool   // Text omits some blocks
	Ignored   int    // blocks dropped as noise (scrolling, re-wrapping); for logs
}

// Empty reports whether nothing meaningful changed. The scheduler still
// saves the new snapshot in that case (so the next diff starts from it) but
// records no change.
func (r Result) Empty() bool { return r.Text == "" }

// Diff compares old and new canonical text of a source of the given kind.
func Diff(kind store.Kind, old, new string, opts Options) (Result, error) {
	var (
		r      Result
		blocks []string
	)
	switch kind {
	case store.KindFeed:
		d := Feed(old, new, opts.Windowed)
		r.Kind, r.Ignored, blocks = d.Kind(), d.Scrolled, d.blocks()
	case store.KindPDF, store.KindHTML:
		d := Text(old, new)
		r.Kind, r.Ignored, blocks = store.ChangeEdited, d.Ignored, d.blocks()
	default:
		return Result{}, fmt.Errorf("differ: unknown source kind %q", kind)
	}
	if len(blocks) == 0 {
		return Result{Ignored: r.Ignored}, nil
	}
	r.Text, r.Truncated = join(blocks, cmp.Or(opts.MaxBytes, DefaultMaxBytes))
	return r, nil
}

// join concatenates whole blocks while they fit in max bytes, so the reader
// never sees half an item or half a hunk, except when even the first block
// is too big, which is cut at a character boundary.
func join(blocks []string, max int) (string, bool) {
	var b strings.Builder
	for i, blk := range blocks {
		if b.Len() > 0 && b.Len()+1+len(blk) > max {
			fmt.Fprintf(&b, "\n... %d more not shown", len(blocks)-i)
			return b.String(), true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if len(blk) > max {
			b.WriteString(cut(blk, max))
			b.WriteString("\n... (cut)")
			if rest := len(blocks) - i - 1; rest > 0 {
				fmt.Fprintf(&b, "\n... %d more not shown", rest)
			}
			return b.String(), true
		}
		b.WriteString(blk)
	}
	return b.String(), false
}

// cut shortens s to at most n bytes without splitting a UTF-8 sequence;
// slicing a string mid-character would leave invalid bytes (and Devanagari
// characters are 3 bytes each).
func cut(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
