package notify

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html"
	"html/template"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"dumpcat/internal/auth"
)

// The logo is sent inside each email (cid:), since mail clients do not load
// SVG or data: images.
//
//go:embed logo.png
var logoPNG []byte

// look is how an event is presented: its labels and colors.
type look struct {
	Kind, Status, Icon string
	Color, Tint, Line  string // text/accent, band background, band border
	Slack              string
	Discord            int
}

func (e *Event) look() look {
	l := look{Kind: map[string]string{
		"backup": "Backup", "restore": "Restore", "verify": "Integrity check", "binlog": "Log archiving", "test": "Test notification", "drill": "Restore drill",
		"stale": "Backup freshness", "binlog_lag": "Log archiving lag", "digest": "Weekly digest", "update": "DumpCat update",
	}[e.Kind]}
	if l.Kind == "" {
		l.Kind = strings.ToUpper(e.Kind[:min(1, len(e.Kind))]) + e.Kind[min(1, len(e.Kind)):]
	}
	switch e.Status {
	case "success":
		l.Status, l.Icon, l.Color, l.Tint, l.Line, l.Slack, l.Discord = "Succeeded", "✅", "#1a7f37", "#eefaf1", "#c4e8cf", "#1a7f37", 0x1a7f37
	case StatusRecovered:
		l.Status, l.Icon, l.Color, l.Tint, l.Line, l.Slack, l.Discord = "Recovered", "✅", "#1a7f37", "#eefaf1", "#c4e8cf", "#1a7f37", 0x1a7f37
	case "partial":
		l.Status, l.Icon, l.Color, l.Tint, l.Line, l.Slack, l.Discord = "Partially completed", "⚠️", "#9a6700", "#fff8e5", "#f1dda2", "#d4a72c", 0xd4a72c
	default:
		l.Status, l.Icon, l.Color, l.Tint, l.Line, l.Slack, l.Discord = "Failed", "❌", "#cf222e", "#fff1f0", "#f4c7c3", "#cf222e", 0xcf222e
		switch e.Kind {
		case "stale":
			l.Status, l.Icon = "Overdue", "⏰"
		case "binlog_lag":
			l.Status, l.Icon = "Behind", "⏳"
		}
	}
	return l
}

// mark is the symbol in the status circle of an email.
func (e *Event) mark() string {
	switch e.Status {
	case "success", StatusRecovered:
		return "✓"
	case "partial":
		return "!"
	}
	return "✕"
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// chip is one storage copy in a "Storage" field ("name=ok, other=failed").
type chip struct{ Name, Status, Color string }

var reCopies = regexp.MustCompile(`^[^=,]+=[a-z_]+(, [^=,]+=[a-z_]+)*$`)

func chips(v string) []chip {
	if !reCopies.MatchString(v) {
		return nil
	}
	var out []chip
	for _, part := range strings.Split(v, ", ") {
		name, st, _ := strings.Cut(part, "=")
		c := chip{Name: name, Status: st, Color: "#6b6963"}
		switch st {
		case "ok":
			c.Color = "#1a7f37"
		case "failed", "missing":
			c.Color = "#cf222e"
		}
		out = append(out, c)
	}
	return out
}

func (e *Event) when() string { return e.Time.Format("2 Jan 2006, 15:04 MST") }

// manageURL is the Notifications page of the instance the event links to.
func (e *Event) manageURL() string {
	if base, _, ok := strings.Cut(e.URL, "/#/"); ok && strings.HasPrefix(base, "http") {
		return base + "/#/notifications"
	}
	return ""
}

// ---------- plain text (email alternative, logs) ----------

func (e *Event) text(host string) string {
	l := e.look()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n%s · %s\n\n", l.Icon, e.Title, l.Kind, l.Status)
	w := 0
	for _, k := range e.Order {
		w = max(w, len([]rune(k)))
	}
	for _, k := range e.Order {
		fmt.Fprintf(&b, "  %-*s  %s\n", w, k, e.Fields[k])
	}
	if e.Error != "" {
		b.WriteString("\nError\n")
		for _, ln := range strings.Split(strings.TrimSpace(e.Error), "\n") {
			b.WriteString("  " + ln + "\n")
		}
	}
	if e.URL != "" {
		b.WriteString("\nOpen in " + e.Brand.name() + ": " + e.URL + "\n")
	}
	fmt.Fprintf(&b, "\n-- \n%s on %s · %s\n", e.Brand.name(), host, e.when())
	return b.String()
}

// ---------- email ----------

// emailShell is the frame of every email: logo, card with a status band, an
// optional button and the footer. A "content" template fills the card.
var emailShell = template.Must(template.New("email").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark"><title>{{.Title}}</title>
<style>
@media (prefers-color-scheme: dark) {
  .bg { background: #0d0e10 !important; } .card { background: #16181c !important; border-color: #2b2e34 !important; }
  .ink { color: #eef0f3 !important; } .muted { color: #9aa0a8 !important; } .cell { border-color: #2b2e34 !important; }
  .band { background: #1d2026 !important; border-color: #2b2e34 !important; } .chip { background: #23262c !important; border-color: #33373e !important; }
  .err { background: #2a1717 !important; border-color: #5c2a2a !important; } .errt { color: #f5b4b4 !important; } .errh { color: #ff8f8f !important; }
  .warn { background: #2a2312 !important; border-color: #5c4a1c !important; } .warnt { color: #f2d38a !important; }
}
@media (max-width: 620px) { .px { padding-left: 20px !important; padding-right: 20px !important; } .k { width: 38% !important; } .hs { display: none !important; } }
</style></head>
<body class="bg" style="margin:0;padding:0;background:#f6f6f3;-webkit-text-size-adjust:100%">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent">{{.Preheader}}</div>
<table role="presentation" class="bg" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f6f6f3">
<tr><td align="center" style="padding:28px 12px 36px">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:600px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif">
  <tr><td style="padding:0 4px 14px">
    <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
      <td style="width:36px;vertical-align:middle"><img src="cid:logo@dumpcat" width="36" height="36" alt="{{.Brand}}" style="display:block;border:0;border-radius:9px"></td>
      <td class="ink" style="padding-left:10px;vertical-align:middle;font-size:16px;font-weight:700;color:#0e1116;line-height:1.2">{{.Brand}}<br><span class="muted" style="font-size:12px;font-weight:400;color:#6b6963">MySQL · MariaDB · PostgreSQL</span></td>
    </tr></table>
  </td></tr>
  <tr><td class="card" style="background:#ffffff;border:1px solid #e3e2dd;border-radius:14px;overflow:hidden">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
      <tr><td class="band px" style="background:{{.L.Tint}};border-bottom:1px solid {{.L.Line}};padding:22px 28px 20px">
        <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
          <td style="vertical-align:top;padding-right:12px"><div style="width:30px;height:30px;line-height:30px;border-radius:15px;background:{{.L.Color}};color:#ffffff;text-align:center;font-size:16px;font-weight:700">{{.Mark}}</div></td>
          <td style="vertical-align:top">
            <div style="font-size:11px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:{{.L.Color}}">{{.Label}}</div>
            <div class="ink" style="margin-top:6px;font-size:20px;font-weight:700;line-height:1.3;color:#0e1116">{{.Title}}</div>
          </td>
        </tr></table>
      </td></tr>
      {{template "content" .}}
      {{if .URL}}<tr><td class="px" style="padding:18px 28px 26px">
        <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr><td style="border-radius:8px;background:#2368c4">
          <a href="{{.URL}}" style="display:inline-block;padding:11px 20px;border-radius:8px;font-size:14px;font-weight:600;color:#ffffff;text-decoration:none">Open in {{.Brand}} →</a>
        </td></tr></table>
      </td></tr>{{else}}<tr><td style="height:18px;line-height:18px;font-size:0">&nbsp;</td></tr>{{end}}
    </table>
  </td></tr>
  <tr><td class="muted" style="padding:16px 8px 0;text-align:center;font-size:12px;line-height:1.6;color:#8a8882">
    Sent by {{.Brand}} on {{.Host}} · {{.When}}{{if .Manage}}<br><a href="{{.Manage}}" style="color:#8a8882;text-decoration:underline">Notification settings</a>{{end}}
  </td></tr>
</table>
</td></tr></table>
</body></html>
`))

// emailContent is the shell with src as its content.
func emailContent(src string) *template.Template {
	return template.Must(template.Must(emailShell.Clone()).Parse(`{{define "content"}}` + src + `{{end}}`))
}

// emailTpl is the email of one event: its fields and error.
var emailTpl = emailContent(`{{if .Rows}}<tr><td class="px" style="padding:8px 28px 4px">
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
        {{range $i, $r := .Rows}}<tr>
          <td class="k muted cell" style="width:32%;padding:11px 12px 11px 0;{{if $i}}border-top:1px solid #eeede8;{{end}}font-size:13px;color:#6b6963;vertical-align:top">{{$r.K}}</td>
          <td class="ink cell" style="padding:11px 0;{{if $i}}border-top:1px solid #eeede8;{{end}}font-size:14px;font-weight:600;color:#0e1116;vertical-align:top;word-break:break-word">{{if $r.Chips}}{{range $r.Chips}}<span class="chip" style="display:inline-block;margin:0 6px 4px 0;padding:3px 9px;border:1px solid #e3e2dd;border-radius:999px;background:#f6f6f3;font-size:12.5px;font-weight:600;white-space:nowrap">{{.Name}} <span style="color:{{.Color}}">{{.Status}}</span></span>{{end}}{{else}}{{$r.V}}{{end}}</td>
        </tr>{{end}}
        </table>
      </td></tr>{{end}}
      {{if .Error}}<tr><td class="px" style="padding:10px 28px 6px">
        <div class="err" style="border:1px solid #f4c7c3;background:#fff6f5;border-radius:10px;padding:14px 16px">
          <div class="errh" style="font-size:12px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:#a32424">Error</div>
          <pre class="errt" style="margin:8px 0 0;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;line-height:1.55;color:#5c1a1a">{{.Error}}</pre>
        </div>
      </td></tr>{{end}}`)

type emailRow struct {
	K, V  string
	Chips []chip
}

func (e *Event) emailHTML(host string) (string, error) {
	l := e.look()
	var rows []emailRow
	var pre []string
	for _, k := range e.Order {
		v := e.Fields[k]
		rows = append(rows, emailRow{K: k, V: v, Chips: chips(v)})
		if len(pre) < 3 && len(v) < 60 {
			pre = append(pre, v)
		}
	}
	data := map[string]any{
		"Title": e.Title, "L": l, "Label": l.Kind + " · " + l.Status, "Mark": e.mark(), "Rows": rows, "Error": trunc(strings.TrimSpace(e.Error), 4000),
		"Host": host, "When": e.when(), "Preheader": strings.Join(pre, " · "), "Manage": template.URL(e.manageURL()), "Brand": e.Brand.name(),
	}
	if strings.HasPrefix(e.URL, "http") {
		data["URL"] = template.URL(e.URL)
	}
	var b bytes.Buffer
	if err := emailTpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// emailMessage is the whole message of an event.
func (e *Event) emailMessage(from string, to []string, host string) ([]byte, error) {
	htmlBody, err := e.emailHTML(host)
	if err != nil {
		return nil, err
	}
	l := e.look()
	return mimeMessage(from, to, e.Brand.subject(strings.TrimSpace(l.Icon+" "+e.Title)), e.Time, e.text(host), htmlBody, e.Brand.EmailLogo())
}

// mimeMessage builds an email with a text and an HTML version, the HTML with
// the logo (a PNG) attached inline.
func mimeMessage(from string, to []string, subject string, date time.Time, textBody, htmlBody string, logo []byte) ([]byte, error) {
	var body bytes.Buffer
	alt := multipart.NewWriter(&body)
	part := func(w *multipart.Writer, h textproto.MIMEHeader, content []byte, b64 bool) error {
		pw, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		if b64 {
			enc := base64.StdEncoding.EncodeToString(content)
			for len(enc) > 76 {
				fmt.Fprintf(pw, "%s\r\n", enc[:76])
				enc = enc[76:]
			}
			_, err = fmt.Fprintf(pw, "%s\r\n", enc)
			return err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write(content); err != nil {
			return err
		}
		return qp.Close()
	}
	if err := part(alt, textproto.MIMEHeader{"Content-Type": {"text/plain; charset=UTF-8"}, "Content-Transfer-Encoding": {"quoted-printable"}},
		[]byte(textBody), false); err != nil {
		return nil, err
	}
	var related bytes.Buffer
	rel := multipart.NewWriter(&related)
	if err := part(rel, textproto.MIMEHeader{"Content-Type": {"text/html; charset=UTF-8"}, "Content-Transfer-Encoding": {"quoted-printable"}},
		[]byte(htmlBody), false); err != nil {
		return nil, err
	}
	if err := part(rel, textproto.MIMEHeader{"Content-Type": {"image/png"}, "Content-Transfer-Encoding": {"base64"},
		"Content-Id": {"<logo@dumpcat>"}, "Content-Disposition": {`inline; filename="dumpcat.png"`}}, logo, true); err != nil {
		return nil, err
	}
	if err := rel.Close(); err != nil {
		return nil, err
	}
	rw, err := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {`multipart/related; type="text/html"; boundary=` + rel.Boundary()}})
	if err != nil {
		return nil, err
	}
	if _, err := rw.Write(related.Bytes()); err != nil {
		return nil, err
	}
	if err := alt.Close(); err != nil {
		return nil, err
	}

	var msg bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&msg, "%s: %s\r\n", k, v) }
	hdr("From", from)
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", subject))
	hdr("Date", date.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+auth.RandomToken(12)+"@dumpcat>")
	hdr("Auto-Submitted", "auto-generated")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "multipart/alternative; boundary="+alt.Boundary())
	msg.WriteString("\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes(), nil
}

// ---------- Telegram (HTML parse mode) ----------

func (e *Event) telegram() string {
	l := e.look()
	esc := html.EscapeString
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>\n<i>%s · %s</i>\n", l.Icon, esc(e.Title), esc(l.Kind), esc(l.Status))
	if len(e.Order) > 0 {
		b.WriteString("\n")
	}
	for _, k := range e.Order {
		fmt.Fprintf(&b, "<b>%s</b>  %s\n", esc(k), esc(e.Fields[k]))
	}
	if e.Error != "" {
		fmt.Fprintf(&b, "\n<b>Error</b>\n<pre>%s</pre>\n", esc(trunc(strings.TrimSpace(e.Error), 1500)))
	}
	if strings.HasPrefix(e.URL, "http") {
		fmt.Fprintf(&b, "\n<a href=\"%s\">Open in %s →</a>", esc(e.URL), esc(e.Brand.name()))
	}
	return b.String()
}

// ---------- Slack (attachment with a color bar, Block Kit) ----------

// discordEsc keeps names and messages (job names, errors from a database server or an imported
// catalog) from turning into Discord markdown: masked links, mentions, formatting.
func discordEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\*_~`|[]()<>#@-", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func slackEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (e *Event) slack(host string) map[string]any {
	l := e.look()
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
		"text": fmt.Sprintf("%s *%s*\n%s · %s", l.Icon, slackEsc(trunc(e.Title, 200)), l.Kind, l.Status)}}}
	var fields []any
	for _, k := range e.Order {
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": "*" + slackEsc(k) + "*\n" + slackEsc(trunc(e.Fields[k], 1900))})
	}
	for len(fields) > 0 { // at most 10 fields per section
		n := min(10, len(fields))
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields[:n]})
		fields = fields[n:]
	}
	if e.Error != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
			"text": "*Error*\n```" + slackEsc(trunc(strings.TrimSpace(e.Error), 2800)) + "```"}})
	}
	if strings.HasPrefix(e.URL, "http") {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button",
			"text": map[string]any{"type": "plain_text", "text": trunc("Open in "+e.Brand.name(), 75)}, "url": e.URL}}})
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn",
		"text": fmt.Sprintf("%s on %s · <!date^%d^{date_short_pretty} {time}|%s>", slackEsc(e.Brand.name()), slackEsc(host), e.Time.Unix(), e.when())}}})
	return map[string]any{"text": l.Icon + " " + slackEsc(e.Title), "attachments": []any{map[string]any{"color": l.Slack, "blocks": blocks}}}
}

// ---------- Discord (embed) ----------

func (e *Event) discord(host string) map[string]any {
	l := e.look()
	desc := l.Kind + " · " + l.Status
	if e.Error != "" {
		desc += "\n**Error**\n```\n" + trunc(strings.ReplaceAll(strings.TrimSpace(e.Error), "```", "'''"), 3500) + "\n```"
	}
	embed := map[string]any{
		"title": trunc(l.Icon+" "+e.Title, 256), "description": desc, "color": l.Discord,
		"footer": map[string]any{"text": trunc(e.Brand.name()+" on "+host, 2000)}, "timestamp": e.Time.UTC().Format(time.RFC3339),
	}
	if strings.HasPrefix(e.URL, "http") {
		embed["url"] = e.URL
	}
	var fields []any
	for _, k := range e.Order {
		if len(fields) == 25 {
			break
		}
		v := e.Fields[k]
		if v == "" {
			v = "—"
		}
		fields = append(fields, map[string]any{"name": trunc(k, 256), "value": trunc(discordEsc(v), 1024), "inline": len(v) <= 40})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	return map[string]any{"username": e.Brand.discordName(), "embeds": []any{embed}}
}
