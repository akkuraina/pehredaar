package differ

import (
	"cmp"
	"strings"

	"github.com/akkuraina/pehredaar/internal/store"
)

// FeedDiff is an item-level comparison of two canonical feed texts.
type FeedDiff struct {
	Added   []string // canonical lines of new items
	Removed []string // canonical lines of withdrawn items
	Edited  []ItemEdit

	// Scrolled counts items that only entered or left the page because the
	// list moved: noise for a windowed feed.
	Scrolled int
}

// ItemEdit is one item whose line changed.
type ItemEdit struct {
	ID       string
	Old, New string   // full canonical lines
	Removed  []string // fields only in Old
	Added    []string // fields only in New
}

// Kind picks the single ChangeKind stored with the change. "added" wins
// because a new notice is what subscribers most want to hear about.
func (d FeedDiff) Kind() store.ChangeKind {
	switch {
	case len(d.Added) > 0:
		return store.ChangeAdded
	case len(d.Edited) > 0:
		return store.ChangeEdited
	case len(d.Removed) > 0:
		return store.ChangeRemoved
	}
	return ""
}

// Empty reports whether no item was added, removed or edited.
func (d FeedDiff) Empty() bool {
	return len(d.Added)+len(d.Removed)+len(d.Edited) == 0
}

// feedSep separates fields in a canonical feed line. The differ relies only
// on the line shape "id | createdAt | field | field ...", which package
// normalize guarantees for every feed format; it never interprets fields.
const feedSep = " | "

type feedLine struct {
	id     string
	fields []string // createdAt first, then the rest in canonical order
	raw    string
}

func (l feedLine) createdAt() string {
	if len(l.fields) == 0 {
		return ""
	}
	return l.fields[0]
}

// compareFeed orders lines like the normalizer does: newest first, then id.
func compareFeed(a, b feedLine) int {
	return cmp.Or(cmp.Compare(b.createdAt(), a.createdAt()), cmp.Compare(a.id, b.id))
}

func parseFeed(text string) []feedLine {
	var out []feedLine
	for raw := range strings.SplitSeq(text, "\n") {
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, feedSep)
		out = append(out, feedLine{id: parts[0], fields: parts[1:], raw: raw})
	}
	return out
}

// oldest returns the line that sorts last, i.e. the bottom of the page.
func oldest(lines []feedLine) (feedLine, bool) {
	if len(lines) == 0 {
		return feedLine{}, false
	}
	last := lines[0]
	for _, l := range lines[1:] {
		if compareFeed(l, last) > 0 {
			last = l
		}
	}
	return last, true
}

// Feed compares two canonical feed texts item by item, matching items by id.
//
// For a windowed feed (one page of a longer list), an item can appear or
// disappear without being added or deleted: when SSC posts a new notice,
// the oldest one on page 1 moves to page 2, and when a notice is deleted,
// one moves up from page 2. Both happen at the bottom of the page, so:
//
//   - a missing item that is older than everything now on the page fell
//     off the bottom, and is not "removed";
//   - a new item that is older than everything previously on the page came
//     up from page 2, and is not "added". A genuinely new notice is the
//     newest item, never the oldest.
//
// This replaces a count-based rule ("removed only if the count didn't
// grow") that misreads a deletion and an addition in the same interval.
func Feed(old, new string, windowed bool) FeedDiff {
	o, n := parseFeed(old), parseFeed(new)
	oldByID := make(map[string]feedLine, len(o))
	for _, l := range o {
		oldByID[l.id] = l
	}
	newByID := make(map[string]feedLine, len(n))
	for _, l := range n {
		newByID[l.id] = l
	}
	oldBottom, haveOld := oldest(o)
	newBottom, haveNew := oldest(n)

	var d FeedDiff
	for _, nl := range n {
		ol, seen := oldByID[nl.id]
		switch {
		case !seen && windowed && haveOld && compareFeed(nl, oldBottom) > 0:
			d.Scrolled++
		case !seen:
			d.Added = append(d.Added, nl.raw)
		case ol.raw != nl.raw:
			removed, added := fieldDiff(ol.fields, nl.fields)
			d.Edited = append(d.Edited, ItemEdit{
				ID: nl.id, Old: ol.raw, New: nl.raw, Removed: removed, Added: added,
			})
		}
	}
	for _, ol := range o {
		if _, kept := newByID[ol.id]; kept {
			continue
		}
		if windowed && haveNew && compareFeed(ol, newBottom) > 0 {
			d.Scrolled++
			continue
		}
		d.Removed = append(d.Removed, ol.raw)
	}
	return d
}

// fieldDiff returns the fields only in a and only in b, as multisets: an
// item may legitimately have two identical-looking attachment fields.
func fieldDiff(a, b []string) (onlyA, onlyB []string) {
	return subtract(a, b), subtract(b, a)
}

func subtract(a, b []string) []string {
	left := make(map[string]int, len(b))
	for _, f := range b {
		left[f]++
	}
	var out []string
	for _, f := range a {
		if left[f] > 0 {
			left[f]--
			continue
		}
		out = append(out, f)
	}
	return out
}

// blocks renders one block per item: additions, then edits, then removals.
// An edit shows the item's headline for context even if only an
// attachment changed, so the reader (or LLM) knows which notice it is.
func (d FeedDiff) blocks() []string {
	var out []string
	for _, l := range d.Added {
		out = append(out, "+ "+l)
	}
	for _, e := range d.Edited {
		var b strings.Builder
		b.WriteString("~ " + e.ID)
		// fields[0] is createdAt, fields[1] the headline.
		if f := strings.Split(e.New, feedSep); len(f) > 2 {
			b.WriteString(feedSep + f[2])
		}
		for _, f := range e.Removed {
			b.WriteString("\n    - " + f)
		}
		for _, f := range e.Added {
			b.WriteString("\n    + " + f)
		}
		out = append(out, b.String())
	}
	for _, l := range d.Removed {
		out = append(out, "- "+l)
	}
	return out
}

// String renders the whole diff, uncapped.
func (d FeedDiff) String() string { return strings.Join(d.blocks(), "\n") }
