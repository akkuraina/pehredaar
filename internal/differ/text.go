package differ

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Op is what happened to one line.
type Op int8

const (
	Equal Op = iota
	Delete
	Insert
)

// Line is one line of a text diff.
type Line struct {
	Op   Op
	Text string
}

// Hunk is a run of changed lines plus surrounding context.
type Hunk struct {
	OldStart, NewStart int // 1-based line numbers of the hunk's first line
	Lines              []Line
}

// TextDiff is a line-level comparison of two canonical document texts.
type TextDiff struct {
	Hunks   []Hunk
	Ignored int // hunks dropped because only whitespace/punctuation moved
}

// Empty reports whether no meaningful hunk remains.
func (d TextDiff) Empty() bool { return len(d.Hunks) == 0 }

// contextLines is how many unchanged lines surround each change. Two is
// enough for the LLM to see e.g. the table row header above a changed date.
const contextLines = 2

// Text diffs two canonical document texts line by line and drops hunks
// whose change is only re-wrapping or punctuation (see essence).
func Text(old, new string) TextDiff {
	var d TextDiff
	for _, h := range group(lineDiff(old, new)) {
		if h.cosmetic() {
			d.Ignored++
			continue
		}
		d.Hunks = append(d.Hunks, h)
	}
	return d
}

// lineDiff runs go-diff in line mode: each distinct line is mapped to one
// rune, the rune strings are diffed (fast, and the result aligns to whole
// lines), then mapped back. This is the documented recipe for line diffs
// with diffmatchpatch.
func lineDiff(old, new string) []Line {
	dmp := diffmatchpatch.New()
	// The default 1s timeout makes the result depend on machine load; a
	// line-mode diff of a notice is tiny, so we want the exact answer.
	dmp.DiffTimeout = 0

	a, b, lines := dmp.DiffLinesToChars(terminate(old), terminate(new))
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)

	var out []Line
	for _, df := range diffs {
		op := Equal
		switch df.Type {
		case diffmatchpatch.DiffDelete:
			op = Delete
		case diffmatchpatch.DiffInsert:
			op = Insert
		}
		for l := range strings.SplitSeq(strings.TrimSuffix(df.Text, "\n"), "\n") {
			out = append(out, Line{Op: op, Text: l})
		}
	}
	return out
}

// terminate ends every line, including the last, with "\n". Canonical text
// has no trailing newline, so without this, appending a line would also
// "change" the previous last line ("x" vs "x\n").
func terminate(s string) string {
	if s == "" {
		return ""
	}
	return s + "\n"
}

// group cuts a line diff into hunks. Changes separated by at most
// 2*contextLines unchanged lines share a hunk, so contexts never overlap.
func group(lines []Line) []Hunk {
	// at[i] is where lines[i] sits in the old and new texts.
	type pos struct{ old, new int }
	at := make([]pos, len(lines))
	o, n := 1, 1
	for i, l := range lines {
		at[i] = pos{o, n}
		if l.Op != Insert {
			o++
		}
		if l.Op != Delete {
			n++
		}
	}

	var hunks []Hunk
	for i := 0; i < len(lines); {
		if lines[i].Op == Equal {
			i++
			continue
		}
		last := i // last changed line in this hunk
		for j := i + 1; j < len(lines) && j-last <= 2*contextLines+1; j++ {
			if lines[j].Op != Equal {
				last = j
			}
		}
		start := max(0, i-contextLines)
		stop := min(len(lines), last+contextLines+1)
		hunks = append(hunks, Hunk{
			OldStart: at[start].old,
			NewStart: at[start].new,
			Lines:    slices.Clone(lines[start:stop]),
		})
		i = stop
	}
	return hunks
}

// cosmetic reports whether the deleted and inserted lines of h say the same
// thing once whitespace, line breaks and punctuation are ignored. That
// catches pdftotext re-wrapping a paragraph, or a curly quote becoming a
// straight one, which would otherwise each cost an LLM call.
func (h Hunk) cosmetic() bool {
	var del, ins []string
	for _, l := range h.Lines {
		switch l.Op {
		case Delete:
			del = append(del, l.Text)
		case Insert:
			ins = append(ins, l.Text)
		}
	}
	return essence(strings.Join(del, " ")) == essence(strings.Join(ins, " "))
}

// essence keeps the characters that carry meaning: letters, digits and
// combining marks (Devanagari vowel signs are marks; dropping them would
// make "कि" equal "की"). Punctuation between two digits is kept too, so
// "1.5" never equals "15" and "12.11.2026" never equals "1211.2026".
func essence(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			// dropped
		case i > 0 && i < len(rs)-1 && unicode.IsDigit(rs[i-1]) && unicode.IsDigit(rs[i+1]):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// blocks renders one block per hunk in a compact unified-diff style.
func (d TextDiff) blocks() []string {
	out := make([]string, 0, len(d.Hunks))
	for _, h := range d.Hunks {
		var b strings.Builder
		fmt.Fprintf(&b, "@@ -%d +%d @@", h.OldStart, h.NewStart)
		for _, l := range h.Lines {
			b.WriteString("\n" + [...]string{"  ", "- ", "+ "}[l.Op] + l.Text)
		}
		out = append(out, b.String())
	}
	return out
}

// String renders the whole diff, uncapped.
func (d TextDiff) String() string { return strings.Join(d.blocks(), "\n") }
