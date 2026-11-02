package summarizer

import "github.com/akkuraina/pehredaar/internal/store"

// phrases are the fixed strings of a template summary in one language.
type phrases struct {
	newNotice, updatedNotice, removedNotice, updatedDoc string
	was, now, openPDF                                   string
	attReplaced, attAdded, attRemoved                   string
	andMore                                             string // fmt verb: count
	fieldNames                                          map[string]string
}

var english = phrases{
	newNotice:     "New notice",
	updatedNotice: "Notice updated",
	removedNotice: "Notice removed",
	updatedDoc:    "Document updated",
	was:           "Was",
	now:           "Now",
	openPDF:       "Open the PDF to see everything that changed.",
	attReplaced:   "Attached file replaced",
	attAdded:      "File attached",
	attRemoved:    "Attached file removed",
	andMore:       "…and %d more changes.",
	fieldNames: map[string]string{
		"title":   "Title",
		"posted":  "Posted",
		"desc":    "Details",
		"content": "Schedule",
		"start":   "Start date",
		"end":     "End date",
		"link":    "Link",
	},
}

var hindi = phrases{
	newNotice:     "नई सूचना",
	updatedNotice: "सूचना में बदलाव",
	removedNotice: "सूचना हटाई गई",
	updatedDoc:    "दस्तावेज़ में बदलाव",
	was:           "पहले",
	now:           "अब",
	openPDF:       "क्या बदला, यह देखने के लिए PDF खोलें।",
	attReplaced:   "संलग्न फ़ाइल बदली गई",
	attAdded:      "फ़ाइल संलग्न की गई",
	attRemoved:    "संलग्न फ़ाइल हटाई गई",
	andMore:       "…और %d अन्य बदलाव।",
	fieldNames: map[string]string{
		"title":   "शीर्षक",
		"posted":  "प्रकाशित",
		"desc":    "विवरण",
		"content": "कार्यक्रम",
		"start":   "आरंभ तिथि",
		"end":     "अंतिम तिथि",
		"link":    "लिंक",
	},
}

func phrasesFor(lang store.Language) phrases {
	if lang == store.LangHindi {
		return hindi
	}
	return english
}
