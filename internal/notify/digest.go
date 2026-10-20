package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// Digest is the weekly summary of every job, emailed to the channels that
// opt in.
type Digest struct {
	From, To    time.Time // the period covered
	Jobs        []DigestJob
	Overdue     []DigestOverdue
	Storages    []DigestStorage
	Drills      []DigestDrill // restore drills that ran in the period
	Written     int64         // bytes written by the backups of the period
	PrevWritten int64         // and of the period before
	URL         string        // the dashboard ("" without a base URL)
	Time        time.Time
	Brand       Brand // set by the Notifier from the settings
}

// DigestJob is one job's week.
type DigestJob struct {
	Name, Server string
	OK, Failed   int // finished backups (partial ones count as failed)
	VerifyOK     int
	VerifyFailed int
	Size         int64 // newest full backup
	PrevSize     int64 // newest full backup before the period (0 = none)
	LastSuccess  *time.Time
	Overdue      bool
}

// DigestOverdue is a job without a recent successful backup.
type DigestOverdue struct {
	Job         string
	LastSuccess *time.Time
	Due         time.Time // when a new one was expected at the latest
}

// DigestDrill is one restore drill's week.
type DigestDrill struct {
	Name, Job      string
	Passed, Failed int
	LastStatus     string        // of the newest run: passed, failed
	RTO            time.Duration // restore time of the newest run
	LastError      string        // of the newest run, when it failed
}

func (r DigestDrill) results() string {
	s := fmt.Sprintf("%d passed", r.Passed)
	if r.Failed > 0 {
		s += fmt.Sprintf(", %d failed", r.Failed)
	}
	return s
}

// DigestStorage is what one storage holds.
type DigestStorage struct {
	Name, Type  string
	Backups     int
	Bytes       int64 // backups and binlogs
	Free, Total int64 // free space of a local storage (Total 0 = unknown)
}

func (d *Digest) totals() (ok, failed, vok, vfail int) {
	for _, j := range d.Jobs {
		ok += j.OK
		failed += j.Failed
		vok += j.VerifyOK
		vfail += j.VerifyFailed
	}
	return
}

func (d *Digest) stored() (n int64) {
	for _, s := range d.Storages {
		n += s.Bytes
	}
	return n
}

// event gives the digest the look of an event (status band, link, footer).
func (d *Digest) event() *Event {
	ok, failed, _, vfail := d.totals()
	ev := &Event{Kind: "digest", Status: "success", URL: d.URL, Time: d.Time}
	if failed > 0 || vfail > 0 || len(d.Overdue) > 0 || d.drillsFailed() {
		ev.Status = "partial"
	}
	switch {
	case ok+failed == 0:
		ev.Title = "No backups ran this week"
	case failed == 0:
		ev.Title = fmt.Sprintf("All %d backups succeeded", ok)
	default:
		ev.Title = fmt.Sprintf("%d of %d backups succeeded", ok, ok+failed)
	}
	return ev
}

func (d *Digest) period() string {
	f, t := d.From, d.To
	switch {
	case f.Year() != t.Year():
		return f.Format("2 Jan 2006") + " – " + t.Format("2 Jan 2006")
	case f.Month() != t.Month():
		return f.Format("2 Jan") + " – " + t.Format("2 Jan 2006")
	}
	return f.Format("2") + " – " + t.Format("2 Jan 2006")
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// growth compares a size with an earlier one ("+3.1%", "new").
func growth(now, before int64) string {
	switch {
	case now == 0 && before == 0:
		return ""
	case before == 0:
		return "new"
	}
	p := float64(now-before) / float64(before) * 100
	if p > -0.05 && p < 0.05 {
		return "±0%"
	}
	if p > 0 {
		return fmt.Sprintf("+%.1f%%", p)
	}
	return fmt.Sprintf("−%.1f%%", -p)
}

func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	d := now.Sub(*t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", max(int(d.Minutes()), 0))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// roundDur: 45s, 3m12s, 1h05m.
func roundDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (o DigestOverdue) text(now time.Time) string {
	last := "no successful backup yet"
	if o.LastSuccess != nil {
		last = "last successful backup " + o.LastSuccess.Format("2 Jan, 15:04") + " (" + ago(o.LastSuccess, now) + ")"
	}
	return last + ", a new one was due by " + o.Due.Format("2 Jan, 15:04")
}

func (j DigestJob) backups() string {
	if j.OK+j.Failed == 0 {
		return "none"
	}
	s := fmt.Sprintf("%d ok", j.OK)
	if j.Failed > 0 {
		s += fmt.Sprintf(", %d failed", j.Failed)
	}
	return s
}

func (j DigestJob) checks() string {
	switch {
	case j.VerifyOK+j.VerifyFailed == 0:
		return "—"
	case j.VerifyFailed == 0:
		return fmt.Sprintf("%d ok", j.VerifyOK)
	}
	return fmt.Sprintf("%d ok, %d failed", j.VerifyOK, j.VerifyFailed)
}

func (d *Digest) drillsFailed() bool {
	for _, r := range d.Drills {
		if r.LastStatus == "failed" {
			return true
		}
	}
	return false
}

func (s DigestStorage) free() string {
	if s.Total <= 0 {
		return "—"
	}
	return fmt.Sprintf("%s (%d%%)", fmtBytes(s.Free), s.Free*100/s.Total)
}

// ---------- plain text ----------

func (d *Digest) text(host string) string {
	ev := d.event()
	ok, failed, vok, vfail := d.totals()
	var b strings.Builder
	fmt.Fprintf(&b, "📊 Weekly digest: %s\n%s\n\n", ev.Title, d.period())
	fmt.Fprintf(&b, "  Succeeded  %d of %s\n", ok, plural(ok+failed, "backup", "backups"))
	fmt.Fprintf(&b, "  Failed     %d\n", failed)
	fmt.Fprintf(&b, "  Written    %s", fmtBytes(d.Written))
	if g := growth(d.Written, d.PrevWritten); g != "" && g != "new" {
		fmt.Fprintf(&b, " (%s vs last week)", g)
	}
	fmt.Fprintf(&b, "\n  Integrity  %d ok, %d failed\n", vok, vfail)
	if len(d.Overdue) > 0 {
		b.WriteString("\nOverdue\n")
		for _, o := range d.Overdue {
			fmt.Fprintf(&b, "  %s — %s\n", o.Job, o.text(d.Time))
		}
	}
	if len(d.Jobs) > 0 {
		b.WriteString("\nJobs\n")
		w := 0
		for _, j := range d.Jobs {
			w = max(w, len([]rune(j.Name)))
		}
		for _, j := range d.Jobs {
			size := "—"
			if j.Size > 0 {
				size = fmtBytes(j.Size)
				if g := growth(j.Size, j.PrevSize); g != "" {
					size += " (" + g + ")"
				}
			}
			fmt.Fprintf(&b, "  %-*s  backups %s · size %s · checks %s · last success %s\n", w, j.Name, j.backups(), size, j.checks(), ago(j.LastSuccess, d.Time))
		}
	}
	if len(d.Drills) > 0 {
		b.WriteString("\nRestore drills\n")
		w := 0
		for _, r := range d.Drills {
			w = max(w, len([]rune(r.Name)))
		}
		for _, r := range d.Drills {
			fmt.Fprintf(&b, "  %-*s  %s · %s · restore %s", w, r.Name, r.Job, r.results(), roundDur(r.RTO))
			if r.LastStatus == "failed" {
				fmt.Fprintf(&b, " · last run FAILED: %s", trunc(firstLine(r.LastError), 200))
			}
			b.WriteString("\n")
		}
	}
	if len(d.Storages) > 0 {
		fmt.Fprintf(&b, "\nStorage (%s in total)\n", fmtBytes(d.stored()))
		w := 0
		for _, s := range d.Storages {
			w = max(w, len([]rune(s.Name)))
		}
		for _, s := range d.Storages {
			fmt.Fprintf(&b, "  %-*s  %s · %s · free %s\n", w, s.Name, plural(s.Backups, "backup", "backups"), fmtBytes(s.Bytes), s.free())
		}
	}
	if d.URL != "" {
		b.WriteString("\nOpen in " + d.Brand.name() + ": " + d.URL + "\n")
	}
	fmt.Fprintf(&b, "\n-- \n%s on %s · %s\n", d.Brand.name(), host, ev.when())
	return b.String()
}

// ---------- email ----------

var digestTpl = emailContent(`<tr><td class="px" style="padding:20px 28px 2px">
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
        {{range $i, $t := .Tiles}}<td width="25%" style="vertical-align:top;{{if $i}}padding-left:8px{{end}}">
          <div class="band" style="border:1px solid #e3e2dd;border-radius:10px;background:#f6f6f3;padding:11px 12px 10px">
            <div class="muted" style="font-size:10.5px;font-weight:700;letter-spacing:.6px;text-transform:uppercase;color:#6b6963">{{$t.K}}</div>
            <div{{if not $t.Color}} class="ink"{{end}} style="margin-top:4px;font-size:19px;font-weight:700;line-height:1.25;color:{{if $t.Color}}{{$t.Color}}{{else}}#0e1116{{end}}">{{$t.V}}</div>
            <div class="muted" style="margin-top:2px;font-size:11.5px;line-height:1.35;color:#6b6963">{{$t.Sub}}</div>
          </div></td>{{end}}
        </tr></table>
      </td></tr>
      {{if .Overdue}}<tr><td class="px" style="padding:14px 28px 0">
        <div class="warn" style="border:1px solid #f1dda2;background:#fff8e5;border-radius:10px;padding:12px 16px">
          <div class="warnt" style="font-size:12px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:#9a6700">Overdue</div>
          {{range .Overdue}}<div class="ink" style="margin-top:6px;font-size:13.5px;line-height:1.45;color:#0e1116"><b>{{.Job}}</b> — {{.Text}}</div>{{end}}
        </div>
      </td></tr>{{end}}
      {{if .Jobs}}<tr><td class="px" style="padding:20px 28px 0">
        <div class="muted" style="font-size:11px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:#6b6963">Jobs</div>
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:6px">
          <tr><td class="muted cell" style="padding:6px 8px 6px 0;border-bottom:1px solid #eeede8;font-size:12px;color:#6b6963">Job</td>
            <td class="muted cell" style="padding:6px 8px;border-bottom:1px solid #eeede8;font-size:12px;color:#6b6963">Backups</td>
            <td class="muted cell" style="padding:6px 8px;border-bottom:1px solid #eeede8;font-size:12px;color:#6b6963;text-align:right">Size</td>
            <td class="muted cell hs" style="padding:6px 0 6px 8px;border-bottom:1px solid #eeede8;font-size:12px;color:#6b6963">Checks</td></tr>
          {{range .Jobs}}<tr>
            <td class="cell" style="padding:9px 8px 9px 0;border-bottom:1px solid #eeede8;vertical-align:top"><div class="ink" style="font-size:14px;font-weight:600;color:#0e1116;word-break:break-word">{{.Name}}{{if .Overdue}} <span style="color:#9a6700;font-size:12px">overdue</span>{{end}}</div>
              <div class="muted" style="font-size:12px;color:#6b6963">{{.Server}} · last success {{.Last}}</div></td>
            <td class="cell" style="padding:9px 8px;border-bottom:1px solid #eeede8;vertical-align:top;font-size:13.5px;white-space:nowrap"><span class="ink" style="color:#0e1116">{{.OK}} ok</span>{{if .Failed}}<br><span style="color:#cf222e;font-weight:600">{{.Failed}} failed</span>{{end}}</td>
            <td class="cell" style="padding:9px 8px;border-bottom:1px solid #eeede8;vertical-align:top;text-align:right;white-space:nowrap"><div class="ink" style="font-size:13.5px;color:#0e1116">{{.Size}}</div>{{if .Growth}}<div class="muted" style="font-size:12px;color:#6b6963">{{.Growth}}</div>{{end}}</td>
            <td class="cell hs" style="padding:9px 0 9px 8px;border-bottom:1px solid #eeede8;vertical-align:top;font-size:13.5px;white-space:nowrap">{{if .ChecksFailed}}<span style="color:#cf222e;font-weight:600">{{.Checks}}</span>{{else}}<span class="ink" style="color:#0e1116">{{.Checks}}</span>{{end}}</td>
          </tr>{{end}}
        </table>
      </td></tr>{{end}}
      {{if .Drills}}<tr><td class="px" style="padding:20px 28px 0">
        <div class="muted" style="font-size:11px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:#6b6963">Restore drills</div>
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:6px">
          {{range $i, $r := .Drills}}<tr>
            <td class="cell" style="padding:9px 8px 9px 0;{{if $i}}border-top:1px solid #eeede8;{{end}}vertical-align:top"><div class="ink" style="font-size:14px;font-weight:600;color:#0e1116;word-break:break-word">{{$r.Name}}</div>
              <div class="muted" style="font-size:12px;color:#6b6963">{{$r.Job}} · restore {{$r.RTO}}</div>{{if $r.Error}}<div style="margin-top:3px;font-size:12px;color:#cf222e;word-break:break-word">{{$r.Error}}</div>{{end}}</td>
            <td class="cell" style="padding:9px 0 9px 8px;{{if $i}}border-top:1px solid #eeede8;{{end}}vertical-align:top;text-align:right;white-space:nowrap;font-size:13.5px"><span class="ink" style="color:#0e1116">{{$r.Passed}} passed</span>{{if $r.Failed}}<br><span style="color:#cf222e;font-weight:600">{{$r.Failed}} failed</span>{{end}}</td>
          </tr>{{end}}
        </table>
      </td></tr>{{end}}
      {{if .Storages}}<tr><td class="px" style="padding:20px 28px 0">
        <div class="muted" style="font-size:11px;font-weight:700;letter-spacing:1px;text-transform:uppercase;color:#6b6963">Storage · {{.Stored}} in total</div>
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin-top:6px">
          {{range $i, $s := .Storages}}<tr>
            <td class="cell" style="padding:9px 8px 9px 0;{{if $i}}border-top:1px solid #eeede8;{{end}}vertical-align:top"><div class="ink" style="font-size:14px;font-weight:600;color:#0e1116;word-break:break-word">{{$s.Name}}</div><div class="muted" style="font-size:12px;color:#6b6963">{{$s.Type}} · {{$s.Count}}</div></td>
            <td class="cell" style="padding:9px 0 9px 8px;{{if $i}}border-top:1px solid #eeede8;{{end}}vertical-align:top;text-align:right;white-space:nowrap"><div class="ink" style="font-size:13.5px;color:#0e1116">{{$s.Used}}</div>{{if $s.Free}}<div class="muted" style="font-size:12px;color:#6b6963">free {{$s.Free}}</div>{{end}}</td>
          </tr>{{end}}
        </table>
      </td></tr>{{end}}`)

type digestTile struct{ K, V, Sub, Color string }

func (d *Digest) emailHTML(host string) (string, error) {
	ev := d.event()
	l := ev.look()
	ok, failed, vok, vfail := d.totals()
	tiles := []digestTile{
		{K: "Succeeded", V: fmt.Sprint(ok), Sub: "of " + plural(ok+failed, "backup", "backups")},
		{K: "Failed", V: fmt.Sprint(failed), Sub: "failed or partial"},
		{K: "Written", V: fmtBytes(d.Written), Sub: "this week"},
		{K: "Integrity", V: fmt.Sprintf("%d ok", vok), Sub: "checks passed"},
	}
	if failed > 0 {
		tiles[1].Color = "#cf222e"
	}
	if g := growth(d.Written, d.PrevWritten); g != "" && g != "new" {
		tiles[2].Sub = g + " vs last week"
	}
	if vfail > 0 {
		tiles[3].V, tiles[3].Sub, tiles[3].Color = fmt.Sprintf("%d failed", vfail), fmt.Sprintf("%d of %d checks", vfail, vok+vfail), "#cf222e"
	}
	type overdue struct{ Job, Text string }
	var ods []overdue
	for _, o := range d.Overdue {
		ods = append(ods, overdue{o.Job, o.text(d.Time)})
	}
	type jobRow struct {
		Name, Server, Last, Size, Growth, Checks string
		OK, Failed                               int
		Overdue, ChecksFailed                    bool
	}
	var jobs []jobRow
	for _, j := range d.Jobs {
		r := jobRow{Name: j.Name, Server: j.Server, Last: ago(j.LastSuccess, d.Time), Size: "—", Checks: j.checks(), OK: j.OK, Failed: j.Failed,
			Overdue: j.Overdue, ChecksFailed: j.VerifyFailed > 0}
		if j.Size > 0 {
			r.Size, r.Growth = fmtBytes(j.Size), growth(j.Size, j.PrevSize)
		}
		jobs = append(jobs, r)
	}
	type drillRow struct {
		Name, Job, RTO, Error string
		Passed, Failed        int
	}
	var drills []drillRow
	for _, r := range d.Drills {
		row := drillRow{Name: r.Name, Job: r.Job, RTO: roundDur(r.RTO), Passed: r.Passed, Failed: r.Failed}
		if r.LastStatus == "failed" {
			row.Error = "Last run failed: " + trunc(firstLine(r.LastError), 200)
		}
		drills = append(drills, row)
	}
	type storageRow struct{ Name, Type, Count, Used, Free string }
	var sts []storageRow
	for _, s := range d.Storages {
		r := storageRow{Name: s.Name, Type: s.Type, Count: plural(s.Backups, "backup", "backups"), Used: fmtBytes(s.Bytes)}
		if s.Total > 0 {
			r.Free = s.free()
		}
		sts = append(sts, r)
	}
	data := map[string]any{
		"Title": ev.Title, "L": l, "Label": "Weekly digest · " + d.period(), "Mark": ev.mark(), "Tiles": tiles, "Overdue": ods,
		"Jobs": jobs, "Drills": drills, "Storages": sts, "Stored": fmtBytes(d.stored()), "Host": host, "When": ev.when(), "Brand": d.Brand.name(),
		"Preheader": fmt.Sprintf("%s · %s written · %d overdue", ev.Title, fmtBytes(d.Written), len(d.Overdue)), "Manage": template.URL(ev.manageURL()),
	}
	if strings.HasPrefix(d.URL, "http") {
		data["URL"] = template.URL(d.URL)
	}
	var b bytes.Buffer
	if err := digestTpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// HTML is the page of the digest email (its logo is cid:logo@dumpcat).
func (d *Digest) HTML(host string) (string, error) { return d.emailHTML(host) }

// Logo is the PNG logo that emails show.
func Logo() []byte { return logoPNG }

func (d *Digest) emailMessage(from string, to []string, host string) ([]byte, error) {
	page, err := d.emailHTML(host)
	if err != nil {
		return nil, err
	}
	return mimeMessage(from, to, d.Brand.subject("📊 Weekly digest: "+d.event().Title+" ("+d.period()+")"), d.Time, d.text(host), page, d.Brand.EmailLogo())
}
