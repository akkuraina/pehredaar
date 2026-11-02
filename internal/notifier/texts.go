package notifier

import "github.com/akkuraina/pehredaar/internal/store"

// texts are the bot's fixed messages in one language. Messages are sent as
// HTML, so any literal <, > or & here must be escaped.
type texts struct {
	chooseLanguage string // shown in both languages, before one is chosen
	languageSet    string
	help           string
	watchPrompt    string
	watching       string // toast after subscribing
	alreadyWatched string
	noneWatched    string
	listHeader     string
	stopPrompt     string
	stopped        string // toast after unsubscribing
	stoppedAll     string
	stopButton     string // fmt verb: category label
	stopAllButton  string
	unknownFeed    string
	openPDF        string
	openLink       string
	somethingWrong string
}

const chooseLanguage = "Choose your alert language:\nअलर्ट की भाषा चुनें:"

var textsEN = texts{
	chooseLanguage: chooseLanguage,
	languageSet:    "Alerts will be in English.\n\nNow choose what to watch: /watch",
	help: "<b>Pehredaar</b> watches SSC notices and tells you when something changes: " +
		"new notices, revised dates, corrigenda.\n\n" +
		"/watch: choose feeds to watch\n" +
		"/list: what you're watching\n" +
		"/stop: stop watching a feed (<code>/stop all</code> stops everything)\n" +
		"/language: change alert language",
	watchPrompt:    "Tap a feed to get alerts when it changes:",
	watching:       "Watching %s",
	alreadyWatched: "Already watching %s. Use /stop to remove it.",
	noneWatched:    "You're not watching anything yet. Choose feeds with /watch.",
	listHeader:     "You're watching:",
	stopPrompt:     "Tap a feed to stop watching it:",
	stopped:        "Stopped watching %s",
	stoppedAll:     "Stopped watching everything. Use /watch to start again.",
	stopButton:     "✖ Stop %s",
	stopAllButton:  "✖ Stop all",
	unknownFeed:    "That feed is no longer available.",
	openPDF:        "Open PDF",
	openLink:       "Open link",
	somethingWrong: "Something went wrong. Please try again in a minute.",
}

var textsHI = texts{
	chooseLanguage: chooseLanguage,
	languageSet:    "अलर्ट हिन्दी में आएंगे।\n\nअब चुनें कि किस पर नज़र रखनी है: /watch",
	help: "<b>पहरेदार</b> SSC की सूचनाओं पर नज़र रखता है और कुछ भी बदलने पर आपको बताता है: " +
		"नई सूचनाएं, बदली हुई तारीखें, शुद्धिपत्र।\n\n" +
		"/watch: फ़ीड चुनें\n" +
		"/list: आप किन फ़ीड पर नज़र रख रहे हैं\n" +
		"/stop: किसी फ़ीड को हटाएं (<code>/stop all</code> सब हटाता है)\n" +
		"/language: अलर्ट की भाषा बदलें",
	watchPrompt:    "जिस फ़ीड के अलर्ट चाहिए, उस पर टैप करें:",
	watching:       "%s पर नज़र रखी जा रही है",
	alreadyWatched: "%s पहले से जुड़ा है। हटाने के लिए /stop इस्तेमाल करें।",
	noneWatched:    "आपने अभी कोई फ़ीड नहीं चुनी है। /watch से चुनें।",
	listHeader:     "आप इन पर नज़र रख रहे हैं:",
	stopPrompt:     "जिस फ़ीड को हटाना है, उस पर टैप करें:",
	stopped:        "%s हटा दिया गया",
	stoppedAll:     "सभी फ़ीड हटा दी गईं। फिर से शुरू करने के लिए /watch।",
	stopButton:     "✖ %s हटाएं",
	stopAllButton:  "✖ सभी हटाएं",
	unknownFeed:    "यह फ़ीड अब उपलब्ध नहीं है।",
	openPDF:        "PDF खोलें",
	openLink:       "लिंक खोलें",
	somethingWrong: "कुछ गड़बड़ हो गई। कृपया थोड़ी देर बाद फिर कोशिश करें।",
}

func textsFor(lang store.Language) texts {
	if lang == store.LangHindi {
		return textsHI
	}
	return textsEN
}
