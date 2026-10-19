package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dumpcat/internal/notify"
	"dumpcat/internal/runner"
	"dumpcat/internal/secret"
	"dumpcat/internal/store"
)

var utc = time.UTC

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, utc)
	if err != nil {
		panic(err)
	}
	return t
}

func ptr(t time.Time) *time.Time { return &t }

func TestCheckDaily(t *testing.T) {
	j := &store.Job{Enabled: true, Schedule: "0 2 * * *", Method: store.MethodMysqldump}
	last := ptr(at("2026-10-19 02:00")) // a Monday
	f := Check(j, last, at("2026-10-20 14:59"), utc, time.Hour)
	if !f.Monitored || f.Stale || f.Interval != 24*time.Hour || f.Window != 37*time.Hour || !f.Due.Equal(at("2026-10-20 15:00")) {
		t.Fatalf("daily: %+v", f)
	}
	if f := Check(j, last, at("2026-10-20 15:01"), utc, time.Hour); !f.Stale {
		t.Fatalf("daily, after the window: %+v", f)
	}
	// a backup a moment before its slot is that slot's backup
	if f := Check(j, ptr(at("2026-10-19 01:59")), at("2026-10-19 16:00"), utc, time.Hour); f.Stale {
		t.Fatalf("early start: %+v", f)
	}
	// a manual backup in the afternoon: tomorrow's 02:00 run is due by 14:00 + grace
	if f := Check(j, ptr(at("2026-10-19 16:30")), at("2026-10-20 12:00"), utc, time.Hour); !f.Due.Equal(at("2026-10-20 15:00")) {
		t.Fatalf("manual backup: %+v", f)
	}
}

func TestCheckWeekdays(t *testing.T) {
	j := &store.Job{Enabled: true, Schedule: "0 2 * * 1-5"}
	last := ptr(at("2026-10-23 02:00")) // Friday
	// the weekend is no gap: Monday's run is due by Monday 14:00 + grace
	f := Check(j, last, at("2026-10-26 14:30"), utc, time.Hour)
	if f.Stale || !f.Due.Equal(at("2026-10-26 15:00")) {
		t.Fatalf("weekdays: %+v", f)
	}
	if f := Check(j, last, at("2026-10-26 15:30"), utc, time.Hour); !f.Stale {
		t.Fatalf("weekdays, Monday afternoon: %+v", f)
	}
}

func TestCheckOtherCases(t *testing.T) {
	now := at("2026-10-19 12:00")
	// max age set on the job
	j := &store.Job{Enabled: true, Schedule: "0 2 * * *", Options: store.JobOptions{MaxAgeHours: 6}}
	if f := Check(j, ptr(at("2026-10-19 06:30")), now, utc, time.Hour); !f.MaxAge || f.Stale || f.Window != 6*time.Hour {
		t.Fatalf("max age: %+v", f)
	}
	if f := Check(j, ptr(at("2026-10-19 06:00")), at("2026-10-19 12:00"), utc, time.Hour); f.Stale {
		t.Fatal("max age, inside")
	}
	if f := Check(j, ptr(at("2026-10-19 06:00")), at("2026-10-19 12:01"), utc, time.Hour); !f.Stale {
		t.Fatal("max age, outside")
	}
	// manual jobs are monitored only with a max age (backups started through the API)
	manual := &store.Job{Enabled: true}
	if f := Check(manual, nil, now, utc, time.Hour); f.Monitored {
		t.Fatal("manual job monitored")
	}
	manual.Options.MaxAgeHours = 24
	manual.CreatedAt = at("2026-10-17 12:00")
	if f := Check(manual, nil, now, utc, time.Hour); !f.Monitored || !f.Stale {
		t.Fatalf("manual job with a max age, never run: %+v", f)
	}
	// disabled jobs are not monitored
	if f := Check(&store.Job{Schedule: "0 2 * * *"}, nil, now, utc, time.Hour); f.Monitored {
		t.Fatal("disabled job monitored")
	}
	// physical: the hourly incremental schedule sets the window
	phys := &store.Job{Enabled: true, Method: store.MethodPhysical, Schedule: "0 3 * * 0", IncrSchedule: "0 * * * *"}
	if f := Check(phys, ptr(at("2026-10-19 09:00")), at("2026-10-19 10:31"), utc, 0); !f.Stale || f.Interval != time.Hour {
		t.Fatalf("physical with incrementals: %+v", f)
	}
	// time zone of the schedule
	hcm, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	if hcm != nil {
		// 02:00 in Ho Chi Minh City is 19:00 UTC
		f := Check(&store.Job{Enabled: true, Schedule: "0 2 * * *"}, ptr(at("2026-10-18 19:00")), now, hcm, time.Hour)
		if !f.Due.Equal(at("2026-10-20 08:00")) {
			t.Fatalf("time zone: %v", f.Due.UTC())
		}
	}
	if w := AutoWindow("0 */6 * * *", now, utc, 30*time.Minute); w != 9*time.Hour+30*time.Minute {
		t.Fatalf("auto window: %v", w)
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 45 * time.Second: "45s", 12 * time.Minute: "12m", 3*time.Hour + 20*time.Minute: "3h20m",
		37 * time.Hour: "37h", 52 * time.Hour: "2d4h", 72 * time.Hour: "3d"} {
		if got := Duration(d); got != want {
			t.Errorf("%v: %s, want %s", d, got, want)
		}
	}
}

// ---- checker with a fake clock ----

type fakeRunner struct {
	mu   sync.Mutex
	arch []runner.ArchiverStatus
}

func (f *fakeRunner) ArchiverStatuses() []runner.ArchiverStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runner.ArchiverStatus(nil), f.arch...)
}
func (f *fakeRunner) IsJobRunning(int64) bool        { return false }
func (f *fakeRunner) Location() *time.Location       { return utc }
func (f *fakeRunner) Tasks() []runner.TaskInfo       { return nil }
func (f *fakeRunner) set(a ...runner.ArchiverStatus) { f.mu.Lock(); f.arch = a; f.mu.Unlock() }

type env struct {
	st     *store.Store
	run    *fakeRunner
	clock  time.Time
	events []notify.Event
	sent   []*notify.Digest
	c      *Checker
	server *store.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "dumpcat.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set := st.GetSettings()
	set.Timezone = "UTC"
	if err := st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, run: &fakeRunner{}}
	e.server = &store.Server{Name: "db-prod-01", Host: "10.0.0.5", Port: 3306}
	if err := st.CreateServer(e.server); err != nil {
		t.Fatal(err)
	}
	e.c = e.checker()
	return e
}

func (e *env) checker() *Checker {
	return &Checker{Store: e.st, Runner: e.run, Now: func() time.Time { return e.clock },
		Send:    func(ev notify.Event) { e.events = append(e.events, ev) },
		Digest:  func(_ context.Context, d *notify.Digest) { e.sent = append(e.sent, d) },
		BaseURL: func() string { return "https://backup.example.com" },
		Logf:    func(string, ...any) {}}
}

func (e *env) job(t *testing.T, name, sched string, notify bool) *store.Job {
	t.Helper()
	j := &store.Job{UUID: name + "-uuid", Name: name, ServerID: e.server.ID, ServerName: e.server.Name, Method: store.MethodMysqldump, DBMode: "all",
		Schedule: sched, Compression: "zstd", Enabled: true, NotifyFailure: notify}
	if err := e.st.CreateJob(j); err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *env) backup(t *testing.T, j *store.Job, start time.Time, status string, size int64) *store.Run {
	t.Helper()
	fin := start.Add(3 * time.Minute)
	r := &store.Run{UUID: fmt.Sprintf("%s-%d", j.Name, start.Unix()), JobID: &j.ID, JobName: j.Name, JobUUID: j.UUID, JobSlug: j.Name, ServerID: &j.ServerID,
		ServerName: j.ServerName, Method: j.Method, Type: store.TypeFull, Status: status, StartedAt: start, FinishedAt: &fin, SizeBytes: size, RawBytes: size * 4}
	if status == store.StatusFailed {
		r.Error = "mysqldump: Got error: 2013: Lost connection"
	}
	if err := e.st.CreateRun(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) check(when string) []notify.Event {
	e.clock = at(when)
	n := len(e.events)
	e.c.Check(context.Background())
	return e.events[n:]
}

func TestCheckerStaleAlerts(t *testing.T) {
	e := newEnv(t)
	shop := e.job(t, "shop-nightly", "0 2 * * *", true)
	quiet := e.job(t, "quiet", "0 2 * * *", false) // no failure notifications
	e.backup(t, shop, at("2026-10-19 02:00"), store.StatusSuccess, 1<<30)
	e.backup(t, quiet, at("2026-10-19 02:00"), store.StatusSuccess, 1<<20)
	e.backup(t, shop, at("2026-10-20 02:00"), store.StatusFailed, 0)

	if ev := e.check("2026-10-20 14:59"); len(ev) != 0 {
		t.Fatalf("inside the window: %+v", ev)
	}
	ev := e.check("2026-10-20 15:01")
	if len(ev) != 1 {
		t.Fatalf("alert: %+v", ev)
	}
	a := ev[0]
	if a.Kind != KindStale || a.Status != store.StatusFailed || a.Title != "No recent backup: shop-nightly" || a.URL != fmt.Sprintf("https://backup.example.com/#/runs?job_id=%d", shop.ID) ||
		a.Fields["Due by"] != "20 Oct 2026, 15:00" || !strings.Contains(a.Fields["Last successful backup"], "19 Oct 2026, 02:00 (37h01m ago)") ||
		!strings.HasPrefix(a.Fields["Last run"], "failed, 13h") || !strings.Contains(a.Error, "Lost connection") ||
		a.Fields["Expected"] != "every 24h by the schedule, so within 37h (1.5 × the interval + grace)" {
		t.Fatalf("alert: %+v", a)
	}
	if len(e.c.Incidents()) != 2 {
		t.Fatalf("incidents: %+v", e.c.Incidents())
	}
	// once per incident
	if ev := e.check("2026-10-20 15:02"); len(ev) != 0 {
		t.Fatalf("repeated: %+v", ev)
	}
	// a restart keeps the incident: no second alert
	e.c = e.checker()
	if ev := e.check("2026-10-20 15:30"); len(ev) != 0 {
		t.Fatalf("after a restart: %+v", ev)
	}
	// a successful backup ends it
	ok := e.backup(t, shop, at("2026-10-20 16:00"), store.StatusSuccess, 1<<30)
	ev = e.check("2026-10-20 16:05")
	if len(ev) != 1 || ev[0].Status != notify.StatusRecovered || ev[0].Title != "Backups are fresh again: shop-nightly" ||
		ev[0].Fields["Overdue for"] != "1h05m" || ev[0].URL != fmt.Sprintf("https://backup.example.com/#/runs/%d", ok.ID) {
		t.Fatalf("recovery: %+v", ev)
	}
	// the job without failure notifications recovers silently too
	e.backup(t, quiet, at("2026-10-20 17:00"), store.StatusSuccess, 1<<20)
	if ev := e.check("2026-10-20 17:05"); len(ev) != 0 || len(e.c.Incidents()) != 0 {
		t.Fatalf("silent recovery: %+v %+v", ev, e.c.Incidents())
	}
	// a disabled job's incident ends without a message
	if ev := e.check("2026-10-22 03:00"); len(ev) != 1 {
		t.Fatalf("second incident: %+v", ev)
	}
	if err := e.st.SetJobEnabled(shop.ID, false); err != nil {
		t.Fatal(err)
	}
	if ev := e.check("2026-10-22 03:01"); len(ev) != 0 || len(e.c.Incidents()) != 1 || e.c.Incidents()[0].JobID != quiet.ID {
		t.Fatalf("disabled job: %+v %+v", ev, e.c.Incidents())
	}
	// alerts turned off
	if err := e.st.SetJobEnabled(shop.ID, true); err != nil {
		t.Fatal(err)
	}
	set := e.st.GetSettings()
	set.FreshnessAlerts = false
	e.st.SaveSettings(set)
	if ev := e.check("2026-10-22 04:00"); len(ev) != 0 {
		t.Fatalf("alerts off: %+v", ev)
	}
}

func TestCheckerBinlogLag(t *testing.T) {
	e := newEnv(t)
	j := e.job(t, "shop-nightly", "", true)
	until := at("2026-10-19 09:58")
	st := runner.ArchiverStatus{JobID: j.ID, JobName: j.Name, ServerName: j.ServerName, State: "retrying", Error: "connection refused", ArchivedUntil: &until}
	st.ArchiveLag = 29 * 60
	e.run.set(st)
	if ev := e.check("2026-10-19 10:00"); len(ev) != 0 {
		t.Fatalf("below the threshold: %+v", ev)
	}
	st.ArchiveLag = 31 * 60
	e.run.set(st)
	ev := e.check("2026-10-19 10:02")
	if len(ev) != 1 || ev[0].Kind != KindBinlogLag || ev[0].Title != "Binlog archiving is behind: shop-nightly" || ev[0].Fields["Behind by"] != "31m" ||
		ev[0].Fields["Alert after"] != "30m" || ev[0].Error != "connection refused" || ev[0].Fields["Recoverable up to"] != "19 Oct 2026, 09:58" {
		t.Fatalf("lag alert: %+v", ev)
	}
	st.ArchiveLag = 45 * 60
	e.run.set(st)
	if ev := e.check("2026-10-19 10:16"); len(ev) != 0 {
		t.Fatalf("repeated: %+v", ev)
	}
	st.ArchiveLag, st.State, st.Error = 0, "streaming", ""
	e.run.set(st)
	ev = e.check("2026-10-19 10:20")
	if len(ev) != 1 || ev[0].Status != notify.StatusRecovered || ev[0].Fields["Was behind for"] != "49m" {
		t.Fatalf("caught up: %+v", ev)
	}
	// threshold 0 = no lag alerts
	set := e.st.GetSettings()
	set.BinlogLagMinutes = 0
	e.st.SaveSettings(set)
	st.ArchiveLag = 5 * 3600
	e.run.set(st)
	if ev := e.check("2026-10-19 15:00"); len(ev) != 0 {
		t.Fatalf("lag alerts off: %+v", ev)
	}
}

func TestArchiveLagStaysWithoutArchiver(t *testing.T) {
	e := newEnv(t)
	j := e.job(t, "shop", "", true)
	e.run.set(runner.ArchiverStatus{JobID: j.ID, JobName: j.Name, ArchiveLag: 3600})
	if ev := e.check("2026-10-19 10:00"); len(ev) != 1 {
		t.Fatalf("alert: %+v", ev)
	}
	e.run.set() // archiving turned off: the incident ends without a message
	if ev := e.check("2026-10-19 10:01"); len(ev) != 0 || len(e.c.Incidents()) != 0 {
		t.Fatalf("archiver gone: %+v", ev)
	}
}

func TestDigestSchedule(t *testing.T) {
	if s := digestSlot(at("2026-10-19 07:59"), 1, 8, utc); !s.Equal(at("2026-10-12 08:00")) {
		t.Fatalf("slot before: %v", s)
	}
	if s := digestSlot(at("2026-10-19 08:00"), 1, 8, utc); !s.Equal(at("2026-10-19 08:00")) {
		t.Fatalf("slot at: %v", s)
	}
	if s := digestSlot(at("2026-10-22 08:00"), 0, 18, utc); !s.Equal(at("2026-10-18 18:00")) {
		t.Fatalf("sunday slot: %v", s)
	}
	e := newEnv(t)
	shop := e.job(t, "shop-nightly", "0 2 * * *", true)
	for d := 12; d <= 18; d++ {
		e.backup(t, shop, at(fmt.Sprintf("2026-10-%02d 02:00", d)), store.StatusSuccess, int64(1000+d)<<20)
	}
	e.backup(t, shop, at("2026-10-05 02:00"), store.StatusSuccess, 900<<20)
	e.backup(t, shop, at("2026-10-18 14:00"), store.StatusFailed, 0)
	drill := func(start time.Time, status, msg string, ms int64) {
		x := &store.DrillRun{DrillName: "shop drill", JobID: &shop.ID, JobName: shop.Name, Status: store.StatusRunning, StartedAt: start}
		if err := e.st.CreateDrillRun(x); err != nil {
			t.Fatal(err)
		}
		fin := start.Add(5 * time.Minute)
		x.Status, x.Error, x.RestoreMs, x.FinishedAt = status, msg, ms, &fin
		if err := e.st.UpdateDrillRun(x); err != nil {
			t.Fatal(err)
		}
	}
	drill(at("2026-10-11 05:00"), store.DrillFailed, "the week before", 1000)
	drill(at("2026-10-14 05:00"), store.DrillPassed, "", 200000)
	drill(at("2026-10-18 05:00"), store.DrillFailed, "shop.orders: rows differ", 250000)
	drill(at("2026-10-19 07:00"), store.StatusRunning, "", 0)
	// the first check only remembers the current slot
	e.check("2026-10-17 09:00")
	if len(e.sent) != 0 {
		t.Fatal("digest of a past week sent at the first start")
	}
	e.check("2026-10-19 07:59")
	if len(e.sent) != 0 {
		t.Fatal("digest before its time")
	}
	e.check("2026-10-19 08:00")
	e.check("2026-10-19 08:01")
	time.Sleep(50 * time.Millisecond) // sent in the background
	if len(e.sent) != 1 {
		t.Fatalf("digests sent: %d", len(e.sent))
	}
	d := e.sent[0]
	// 13-18 Oct at 02:00 and the failure; 12 Oct 02:00 belongs to the week before
	if len(d.Jobs) != 1 || d.Jobs[0].OK != 6 || d.Jobs[0].Failed != 1 || d.Jobs[0].Size != 1018<<20 || d.Jobs[0].PrevSize != 1012<<20 ||
		d.URL != "https://backup.example.com/#/dashboard" || !d.From.Equal(at("2026-10-12 08:00")) || len(d.Overdue) != 0 {
		t.Fatalf("digest: %+v", d)
	}
	if d.Written != (1013+1014+1015+1016+1017+1018)<<20 || d.PrevWritten != 1012<<20 {
		t.Fatalf("written: %d, the week before: %d", d.Written, d.PrevWritten)
	}
	// restore drills of the week: the newest finished run sets the status, RTO and error
	want := notify.DigestDrill{Name: "shop drill", Job: "shop-nightly", Passed: 1, Failed: 1, LastStatus: store.DrillFailed, RTO: 250 * time.Second, LastError: "shop.orders: rows differ"}
	if len(d.Drills) != 1 || d.Drills[0] != want {
		t.Fatalf("drills: %+v", d.Drills)
	}
}

// ---- metrics ----

func TestMetrics(t *testing.T) {
	e := newEnv(t)
	shop := e.job(t, `shop "eu"\nightly`, "0 2 * * *", true)
	e.backup(t, shop, at("2026-10-18 02:00"), store.StatusSuccess, 1<<30)
	r := e.backup(t, shop, at("2026-10-19 02:00"), store.StatusFailed, 0)
	e.st.SetRunVerify(r.ID, "failed", "sha256 mismatch")
	local := &store.Storage{Name: "local-nas", Type: "local", Config: map[string]string{"path": t.TempDir()}}
	if err := e.st.CreateStorage(local); err != nil {
		t.Fatal(err)
	}
	until := at("2026-10-19 11:50")
	e.run.set(runner.ArchiverStatus{JobID: shop.ID, JobName: shop.Name, ServerName: shop.ServerName, State: "streaming", ArchiveLag: 42, Pending: 1, ArchivedUntil: &until})
	e.clock = at("2026-10-19 12:00")
	e.c.Disk = func(string) (int64, int64, error) { return 4 << 40, 1 << 40, nil }
	m := e.c.Metrics(MetricSources{Version: "1.4.0", Started: at("2026-10-19 08:00"), DataDir: t.TempDir(),
		AgentOnline: func(int64) bool { return false },
		WireGuard:   func() (bool, map[int64]PeerState) { return true, map[int64]PeerState{} }})
	var b strings.Builder
	if err := m.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	n, err := CheckExposition(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n < 20 {
		t.Fatalf("only %d samples:\n%s", n, out)
	}
	lbl := `job="shop \"eu\"\\nightly",server="db-prod-01",method="mysqldump"`
	for _, want := range []string{
		`dumpcat_build_info{version="1.4.0",goversion="go`,
		"dumpcat_start_time_seconds " + formatValue(unix(at("2026-10-19 08:00"))) + "\n",
		"dumpcat_job_last_success_timestamp_seconds{" + lbl + "} " + formatValue(unix(at("2026-10-18 02:00"))) + "\n",
		"dumpcat_job_last_run_status{" + lbl + `,status="failed"} 1` + "\n",
		"dumpcat_job_last_run_success{" + lbl + "} 0\n",
		"dumpcat_job_last_run_duration_seconds{" + lbl + "} 180\n",
		"dumpcat_job_last_backup_size_bytes{" + lbl + "} 1.073741824e+09\n",
		"dumpcat_job_last_verify_success{" + lbl + "} 0\n",
		"dumpcat_job_stale{" + lbl + "} 0\n",
		"dumpcat_job_max_age_seconds{" + lbl + "} 133200\n",
		`dumpcat_catalog_backups{status="failed"} 1`,
		`dumpcat_tasks_running{kind="backup"} 0`,
		`dumpcat_alerts_open{kind="stale"} 0`,
		`dumpcat_binlog_archive_lag_seconds{job="shop \"eu\"\\nightly",server="db-prod-01"} 42`,
		`dumpcat_binlog_pending_files{job="shop \"eu\"\\nightly",server="db-prod-01"} 1`,
		`dumpcat_storage_used_bytes{storage="local-nas",type="local"} 0`,
		`dumpcat_storage_free_bytes{storage="local-nas",type="local"} 1.099511627776e+12`,
		"dumpcat_data_dir_free_bytes 1.099511627776e+12\n",
		"dumpcat_wireguard_up 1\n",
		"# TYPE dumpcat_job_stale gauge\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(out, "dumpcat_agent_online{") {
		t.Error("an agent series without agents")
	}
}

func TestCheckExposition(t *testing.T) {
	good := "# HELP a_b A help \\\\ text\n# TYPE a_b gauge\na_b{x=\"1\",y=\"a\\\"b\\\\c\\n\"} 1.5\na_b{x=\"2\"} -Inf\n# a comment\nc_total 3 1700000000000\n"
	if n, err := CheckExposition(good); err != nil || n != 3 {
		t.Fatalf("good: %d %v", n, err)
	}
	for name, bad := range map[string]string{
		"no newline":      "a 1",
		"bad name":        "1a 1\n",
		"no value":        "a\n",
		"bad value":       "a one\n",
		"unquoted":        "a{x=1} 1\n",
		"bad escape":      "a{x=\"\\t\"} 1\n",
		"repeated label":  "a{x=\"1\",x=\"2\"} 1\n",
		"repeated series": "a{x=\"1\"} 1\na{x=\"1\"} 2\n",
		"split family":    "a 1\nb 1\na{x=\"1\"} 1\n",
		"type after":      "a 1\n# TYPE a gauge\n",
		"unknown type":    "# TYPE a meter\na 1\n",
		"counter name":    "# TYPE a counter\na 1\n",
		"reserved label":  "a{__name__=\"x\"} 1\n",
	} {
		if _, err := CheckExposition(bad); err == nil {
			t.Errorf("%s: accepted %q", name, bad)
		}
	}
}
