// Package exams is the catalog of exams people can ask Pehredaar to watch.
// It's what the sign-up survey offers. Only exams with curated sources in
// sources.yaml are live; the rest are collected as interests so we know
// whom to tell when they go live (docs/ARCHITECTURE.md §19).
package exams

import "strings"

// Exam is one entry in the catalog.
type Exam struct {
	ID   string // stored in interests; never change it once used
	Name string // short name shown on chips
	Body string // who conducts it
	Hint string // one line of what it covers
	// CategoryPrefix matches the categories in sources.yaml that belong to
	// this exam (e.g. "ssc:"). Empty: nothing watched yet.
	CategoryPrefix string
}

// Catalog lists every exam, in the order the survey shows them.
var Catalog = []Exam{
	{ID: "ssc", Name: "SSC", Body: "Staff Selection Commission", Hint: "CGL, CHSL, MTS, GD, JE, Stenographer", CategoryPrefix: "ssc:"},
	{ID: "upsc", Name: "UPSC", Body: "Union Public Service Commission", Hint: "Civil Services, NDA, CDS, ESE"},
	{ID: "ibps", Name: "IBPS", Body: "Institute of Banking Personnel Selection", Hint: "PO, Clerk, SO, RRB", CategoryPrefix: "ibps:"},
	{ID: "ssb", Name: "SSB", Body: "Services Selection Board", Hint: "Officer selection: Army, Navy, Air Force"},
	{ID: "jee", Name: "JEE", Body: "National Testing Agency", Hint: "JEE Main", CategoryPrefix: "jee:"},
	{ID: "neet", Name: "NEET", Body: "National Testing Agency", Hint: "NEET UG", CategoryPrefix: "neet:"},
	{ID: "cbse", Name: "CBSE", Body: "Central Board of Secondary Education", Hint: "Class 10 and 12 boards"},
	{ID: "cisce", Name: "ICSE / ISC", Body: "CISCE", Hint: "Class 10 and 12 boards", CategoryPrefix: "cisce:"},
	{ID: "clat", Name: "CLAT", Body: "Consortium of NLUs", Hint: "Law entrance"},
	{ID: "cat", Name: "CAT", Body: "IIMs", Hint: "MBA entrance"},
	{ID: "gre", Name: "GRE", Body: "ETS", Hint: "Graduate school abroad"},
	{ID: "gmat", Name: "GMAT", Body: "GMAC", Hint: "Business school abroad"},
}

// Live reports whether Pehredaar watches this exam today, given the
// categories that have enabled sources.
func (e Exam) Live(categories []string) bool {
	return len(e.Categories(categories)) > 0
}

// Categories returns which of the given categories belong to e.
func (e Exam) Categories(categories []string) []string {
	if e.CategoryPrefix == "" {
		return nil
	}
	var out []string
	for _, c := range categories {
		if strings.HasPrefix(c, e.CategoryPrefix) {
			out = append(out, c)
		}
	}
	return out
}

// ByID finds an exam.
func ByID(id string) (Exam, bool) {
	for _, e := range Catalog {
		if e.ID == id {
			return e, true
		}
	}
	return Exam{}, false
}

// ForCategory returns the exam a category belongs to, e.g. "ssc:results"
// belongs to SSC.
func ForCategory(category string) (Exam, bool) {
	for _, e := range Catalog {
		if e.CategoryPrefix != "" && strings.HasPrefix(category, e.CategoryPrefix) {
			return e, true
		}
	}
	return Exam{}, false
}
