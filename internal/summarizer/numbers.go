package summarizer

import (
	"strings"
	"unicode"
)

// checkNumbers returns the first number in summary that does not appear in
// source, or "" if every number does. It guards against the model writing
// a date, age or fee it wasn't given: a wrong number in an exam alert does
// real harm, and this check is plain code rather than a request to the
// model to be careful.
//
// Numbers are compared as runs of digits with leading zeros dropped, so
// "05" matches "5". Devanagari digits (०-९), which a Hindi summary may use
// despite the prompt, count as their ASCII equivalents.
func checkNumbers(summary, source string) string {
	have := make(map[string]bool)
	for _, n := range digitRuns(source) {
		have[n] = true
	}
	for _, n := range digitRuns(summary) {
		if !have[n] {
			return n
		}
	}
	return ""
}

func digitRuns(s string) []string {
	var runs []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			n := strings.TrimLeft(cur.String(), "0")
			if n == "" {
				n = "0"
			}
			runs = append(runs, n)
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			cur.WriteRune(r)
		case r >= '०' && r <= '९':
			cur.WriteRune('0' + (r - '०'))
		case unicode.IsDigit(r):
			// Another script's digit: keep it as is so it can never match
			// silently.
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return runs
}
