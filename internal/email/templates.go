package email

import (
	"bytes"
	htmltpl "html/template"
	"strings"
	texttpl "text/template"

	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/store"
)

// Emails are built from two template sets: HTML (html/template escapes
// everything, so notice text can't inject markup) and plain text (for
// clients that prefer it, and for spam filters that like seeing both).
//
// The HTML uses inline styles and tables only: email clients ignore
// <style> blocks unevenly and most strip CSS layout. Colours match the
// website's owl palette: night ink, amber eyes, paper.

type page struct {
	Lang        store.Language
	BaseURL     string
	Unsubscribe string // alerts only
}

func (p page) Hindi() bool { return p.Lang == store.LangHindi }

type alertData struct {
	page
	Title       string
	Lines       []string // summary, then the old → new lines
	Links       []notifier.Link
	ChangeURL   string
	HasChangeID bool
}

type linkData struct {
	page
	Link string
}

const htmlBase = `{{define "top"}}<!doctype html>
<html lang="{{if .Hindi}}hi{{else}}en{{end}}"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;background:#f6f1e7;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f6f1e7;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border-radius:16px;overflow:hidden;font-family:-apple-system,'Segoe UI',Roboto,'Noto Sans','Noto Sans Devanagari',Arial,sans-serif;color:#141a2e;">
<tr><td style="background:#141a2e;padding:18px 24px;">
  <span style="font-size:22px;">🦉</span>
  <span style="color:#ffb547;font-weight:700;font-size:18px;letter-spacing:.5px;vertical-align:3px;">&nbsp;pehredaar</span>
</td></tr>
<tr><td style="padding:24px;">{{end}}

{{define "bottom"}}</td></tr>
<tr><td style="padding:16px 24px 22px;border-top:1px solid #eee6d6;font-size:12px;line-height:1.6;color:#6b6f80;">
  {{if .Hindi}}मैं पलक नहीं झपकाता। आपको भी ज़रूरत नहीं।{{else}}I don't blink. You don't have to.{{end}} 🦉<br>
  {{if .Unsubscribe}}<a href="{{.BaseURL}}/feed" style="color:#6b6f80;">{{if .Hindi}}क्या देखना है, बदलें{{else}}Change what I watch{{end}}</a>
  &nbsp;·&nbsp; <a href="{{.Unsubscribe}}" style="color:#6b6f80;">{{if .Hindi}}ईमेल बंद करें{{else}}Unsubscribe{{end}}</a>{{end}}
</td></tr>
</table></td></tr></table></body></html>{{end}}

{{define "alert"}}{{template "top" .}}
<div style="font-size:12px;font-weight:700;letter-spacing:1.2px;text-transform:uppercase;color:#b7791f;">{{if .Hindi}}पकड़ लिया{{else}}Caught one{{end}} · {{.Title}}</div>
{{range $i, $l := .Lines}}{{if eq $i 0}}<p style="font-size:18px;line-height:1.45;font-weight:600;margin:10px 0 14px;">{{$l}}</p>
{{else if $l}}<div style="font-family:ui-monospace,Consolas,monospace;font-size:13px;line-height:1.5;background:#fbf6ec;border-left:3px solid #ffb547;padding:6px 10px;margin:4px 0;white-space:pre-wrap;">{{$l}}</div>
{{end}}{{end}}
<p style="margin:18px 0 0;">
{{range .Links}}<a href="{{.URL}}" style="display:inline-block;margin:0 8px 8px 0;padding:9px 14px;border-radius:999px;background:#141a2e;color:#ffffff;text-decoration:none;font-size:14px;">{{if .PDF}}📄 {{if $.Hindi}}PDF खोलें{{else}}Open the PDF{{end}}{{else}}🔗 {{if $.Hindi}}लिंक खोलें{{else}}Open link{{end}}{{end}}</a>{{end}}
{{if .HasChangeID}}<a href="{{.ChangeURL}}" style="display:inline-block;margin:0 8px 8px 0;padding:9px 14px;border-radius:999px;border:1px solid #141a2e;color:#141a2e;text-decoration:none;font-size:14px;">{{if .Hindi}}पूरा बदलाव देखें{{else}}See the full change{{end}}</a>{{end}}
</p>
<p style="font-size:12px;color:#6b6f80;margin:14px 0 0;">{{if .Hindi}}हमेशा आधिकारिक सूचना से मिलान करें।{{else}}Always double-check against the official notice.{{end}}</p>
{{template "bottom" .}}{{end}}

{{define "confirm"}}{{template "top" .}}
<p style="font-size:20px;font-weight:700;margin:0 0 10px;">{{if .Hindi}}बस एक क्लिक बाकी है 👀{{else}}One tap and I'm on watch 👀{{end}}</p>
<p style="font-size:15px;line-height:1.6;margin:0 0 20px;">{{if .Hindi}}पुष्टि करें कि यह आपका ईमेल है, फिर मैं आपकी परीक्षाओं की सूचनाओं पर नज़र रखूँगा और कुछ भी बदलते ही बताऊँगा।{{else}}Confirm this is your email and I'll start watching your exams' notices, and tell you the moment something changes.{{end}}</p>
<a href="{{.Link}}" style="display:inline-block;padding:12px 22px;border-radius:999px;background:#ffb547;color:#141a2e;font-weight:700;text-decoration:none;font-size:15px;">{{if .Hindi}}ईमेल कन्फ़र्म करें{{else}}Confirm my email{{end}}</a>
<p style="font-size:12px;color:#6b6f80;margin:20px 0 0;">{{if .Hindi}}यह लिंक 48 घंटे चलेगा। अगर आपने साइन-अप नहीं किया, तो इस ईमेल को अनदेखा करें।{{else}}This link works for 48 hours. Didn't sign up? Ignore this email and nothing happens.{{end}}</p>
{{template "bottom" .}}{{end}}

{{define "login"}}{{template "top" .}}
<p style="font-size:20px;font-weight:700;margin:0 0 10px;">{{if .Hindi}}वापसी पर स्वागत है 🦉{{else}}Welcome back 🦉{{end}}</p>
<p style="font-size:15px;line-height:1.6;margin:0 0 20px;">{{if .Hindi}}अपनी फ़ीड खोलने के लिए नीचे टैप करें।{{else}}Tap below to open your feed.{{end}}</p>
<a href="{{.Link}}" style="display:inline-block;padding:12px 22px;border-radius:999px;background:#ffb547;color:#141a2e;font-weight:700;text-decoration:none;font-size:15px;">{{if .Hindi}}साइन इन करें{{else}}Sign me in{{end}}</a>
<p style="font-size:12px;color:#6b6f80;margin:20px 0 0;">{{if .Hindi}}यह लिंक 30 मिनट तक और एक ही बार चलेगा।{{else}}This link works once, for 30 minutes. Didn't ask for it? You can ignore this email.{{end}}</p>
{{template "bottom" .}}{{end}}`

const textBase = `{{define "foot"}}
--
{{if .Hindi}}मैं पलक नहीं झपकाता। आपको भी ज़रूरत नहीं। — पहरेदार 🦉{{else}}I don't blink. You don't have to. — Pehredaar 🦉{{end}}
{{if .Unsubscribe}}{{if .Hindi}}ईमेल बंद करें{{else}}Unsubscribe{{end}}: {{.Unsubscribe}}{{end}}{{end}}

{{define "alert"}}{{if .Hindi}}पकड़ लिया{{else}}Caught one{{end}} · {{.Title}}

{{range .Lines}}{{.}}
{{end}}
{{range .Links}}{{.URL}}
{{end}}{{if .HasChangeID}}{{.ChangeURL}}
{{end}}{{template "foot" .}}{{end}}

{{define "confirm"}}{{if .Hindi}}अपना ईमेल कन्फ़र्म करें (48 घंटे तक मान्य):{{else}}Confirm your email (link works for 48 hours):{{end}}
{{.Link}}
{{template "foot" .}}{{end}}

{{define "login"}}{{if .Hindi}}साइन इन करें (30 मिनट, एक बार):{{else}}Sign in (works once, for 30 minutes):{{end}}
{{.Link}}
{{template "foot" .}}{{end}}`

var (
	htmlT = htmltpl.Must(htmltpl.New("email").Parse(htmlBase))
	textT = texttpl.Must(texttpl.New("email").Parse(textBase))
)

// render executes the named template in both formats.
func render(name string, data any) (htmlOut, textOut string, err error) {
	var h, t bytes.Buffer
	if err := htmlT.ExecuteTemplate(&h, name, data); err != nil {
		return "", "", err
	}
	if err := textT.ExecuteTemplate(&t, name, data); err != nil {
		return "", "", err
	}
	return h.String(), strings.TrimSpace(t.String()) + "\n", nil
}
