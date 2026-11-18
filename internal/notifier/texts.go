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

	linked            string // after linking Telegram from the website
	linkedNothingLive string
	feedLink          string // fmt verb: the sign-in link
	noSite            string

	launch       string // fmt verb: exam names (HTML-escaped)
	launchButton string // fmt verb: exam names
	launchDone   string // fmt verb: exam names (HTML-escaped)
}

// linkExpired is shown before a language is chosen, so it is bilingual.
const linkExpired = "🦉 That sign-up link has expired or was already used. You can set me up right here instead.\n" +
	"यह लिंक पुराना हो गया है। आप यहीं से शुरू कर सकते हैं।"

const chooseLanguage = "Choose your alert language:\nअलर्ट की भाषा चुनें:"

var textsEN = texts{
	chooseLanguage: chooseLanguage,
	languageSet:    "Alerts will be in English.\n\nNow choose what to watch: /watch",
	help: "🦉 <b>Pehredaar</b> here. I watch official exam notices (SSC, JEE, NEET, IBPS, ICSE/ISC) day and night and tell you the moment " +
		"something changes: new notices, revised dates, corrigenda. I don't blink. You don't have to.\n\n" +
		"/watch: choose feeds to watch\n" +
		"/list: what you're watching\n" +
		"/stop: stop watching a feed (<code>/stop all</code> stops everything)\n" +
		"/language: change alert language\n" +
		"/feed: open your live feed on the web",
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

	linked:            "🦉 Linked! I'll message you here the moment any of these change:",
	linkedNothingLive: "(None of your exams are live yet. I'll tell you here when they are.)",
	feedLink:          "👀 Your live feed: %s\n\nThis link signs you in once and works for 30 minutes.",
	launch: "🦉 Good news: I'm now watching <b>%s</b> notices. You asked me to tell you when I started.\n\n" +
		"Want them in your alerts? One tap:",
	launchButton: "👀 Start watching %s",
	launchDone:   "Done. I'm watching %s for you now. 🦉\n\n/list shows everything I watch for you; /stop to change it.",
	noSite:       "The website isn't available right now. Your alerts still come here.",
}

var textsHI = texts{
	chooseLanguage: chooseLanguage,
	languageSet:    "अलर्ट हिन्दी में आएंगे।\n\nअब चुनें कि किस पर नज़र रखनी है: /watch",
	help: "🦉 <b>पहरेदार</b> हाज़िर है। मैं दिन-रात परीक्षाओं (SSC, JEE, NEET, IBPS, ICSE/ISC) की आधिकारिक सूचनाओं पर नज़र रखता हूँ और कुछ भी बदलते ही बताता हूँ: " +
		"नई सूचनाएं, बदली हुई तारीखें, शुद्धिपत्र।\n\n" +
		"/watch: फ़ीड चुनें\n" +
		"/list: आप किन फ़ीड पर नज़र रख रहे हैं\n" +
		"/stop: किसी फ़ीड को हटाएं (<code>/stop all</code> सब हटाता है)\n" +
		"/language: अलर्ट की भाषा बदलें\n" +
		"/feed: वेब पर अपनी लाइव फ़ीड खोलें",
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

	linked:            "🦉 जुड़ गया! इनमें से कुछ भी बदलते ही मैं यहीं बताऊँगा:",
	linkedNothingLive: "(आपकी परीक्षाएं अभी लाइव नहीं हैं। लाइव होते ही यहीं बताऊँगा।)",
	feedLink:          "👀 आपकी लाइव फ़ीड: %s\n\nयह लिंक एक बार, 30 मिनट तक चलेगा।",
	launch: "🦉 खुशखबरी: अब मैं <b>%s</b> की सूचनाओं पर भी नज़र रखता हूँ। आपने कहा था कि शुरू होते ही बताऊँ।\n\n" +
		"क्या इन्हें आपके अलर्ट में जोड़ूँ? बस एक टैप:",
	launchButton: "👀 %s देखना शुरू करें",
	launchDone:   "हो गया। अब मैं आपके लिए %s पर नज़र रख रहा हूँ। 🦉\n\n/list में सब कुछ है; बदलने के लिए /stop।",
	noSite:       "वेबसाइट अभी उपलब्ध नहीं है। आपके अलर्ट यहीं आते रहेंगे।",
}

func textsFor(lang store.Language) texts {
	if lang == store.LangHindi {
		return textsHI
	}
	return textsEN
}
