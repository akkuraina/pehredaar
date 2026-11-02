package differ

import (
	"strconv"
	"strings"
)

// The stored diff (changes.diff) is the rendered text, not a structure, so
// later stages (alert templates, the LLM prompt, link extraction) read it
// back with these parsers. They live here, next to the renderers, so the
// format has a single owner.

// FeedBlock is one block of a rendered feed diff.
type FeedBlock struct {
	Op       byte // '+' added, '-' removed, '~' edited
	ID       string
	Headline string

	// Fields holds an added or removed item's fields after the headline.
	Fields []string
	// Removed and Added hold an edited item's changed fields.
	Removed, Added []string
}

// ParseFeed parses text rendered by FeedDiff (and capped by Diff). more is
// the number of blocks the cap left out.
func ParseFeed(text string) (blocks []FeedBlock, more int) {
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "... "):
			more += notShown(line)
		case strings.HasPrefix(line, "    - ") || strings.HasPrefix(line, "    + "):
			if len(blocks) == 0 || blocks[len(blocks)-1].Op != '~' {
				continue // field line without its item; ignore
			}
			b := &blocks[len(blocks)-1]
			if line[4] == '-' {
				b.Removed = append(b.Removed, line[6:])
			} else {
				b.Added = append(b.Added, line[6:])
			}
		case strings.HasPrefix(line, "+ ") || strings.HasPrefix(line, "- "):
			// "id | createdAt | headline | field..."
			parts := strings.Split(line[2:], feedSep)
			b := FeedBlock{Op: line[0], ID: parts[0]}
			if len(parts) > 2 {
				b.Headline = parts[2]
				b.Fields = parts[3:]
			}
			blocks = append(blocks, b)
		case strings.HasPrefix(line, "~ "):
			// "id | headline"
			id, headline, _ := strings.Cut(line[2:], feedSep)
			blocks = append(blocks, FeedBlock{Op: '~', ID: id, Headline: headline})
		}
	}
	return blocks, more
}

// ParseText parses text rendered by TextDiff (and capped by Diff) into the
// deleted and inserted lines, in order, without context lines.
func ParseText(text string) (removed, added []string, more int) {
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "... "):
			more += notShown(line)
		case strings.HasPrefix(line, "- "):
			removed = append(removed, line[2:])
		case strings.HasPrefix(line, "+ "):
			added = append(added, line[2:])
		}
	}
	return removed, added, more
}

// notShown reads the count from "... N more not shown".
func notShown(line string) int {
	f := strings.Fields(line)
	if len(f) >= 3 && f[2] == "more" {
		if n, err := strconv.Atoi(f[1]); err == nil {
			return n
		}
	}
	return 0
}
