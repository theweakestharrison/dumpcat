package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/notify"
	"dumpcat/internal/runner"
	"dumpcat/internal/store"
)

const (
	KindStale     = "stale"
	KindBinlogLag = "binlog_lag"

	incidentsKey  = "monitor_incidents"
	digestSentKey = "digest_last_sent"
)

// Source is what the checker reads from the runner.
type Source interface {
	ArchiverStatuses() []runner.ArchiverStatus
	IsJobRunning(jobID int64) bool
	Location() *time.Location
	Tasks() []runner.TaskInfo
}

// Incident is an alert that was raised and has not cleared yet.
type Incident struct {
	Kind     string    `json:"kind"`
	JobID    int64     `json:"job_id"`
	Job      string    `json:"job"`
	Since    time.Time `json:"since"`    // when the job became overdue / the archive fell behind
	Notified bool      `json:"notified"` // an alert was sent (so the recovery is sent too)
}

func (i *Incident) key() string { return i.Kind + ":" + strconv.FormatInt(i.JobID, 10) }

// Checker raises freshness and binlog lag alerts once per incident, sends a
// recovery message when they clear and sends the weekly digest.
type Checker struct {
	Store   *store.Store
	Runner  Source
	Send    func(notify.Event)                               // alerts and recoveries
	Digest  func(ctx context.Context, d *notify.Digest)      // the weekly digest to every channel that wants it (nil = none)
	BaseURL func() string                                    // for links ("" = none)
	Disk    func(path string) (total, free int64, err error) // free space of local storages (nil = unknown)
	Now     func() time.Time                                 // nil = time.Now (tests use a fake clock)
	Every   time.Duration                                    // between two checks (0 = a minute)
	Logf    func(format string, args ...any)                 // nil = log.Printf

	mu        sync.Mutex
	incidents map[string]*Incident
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (c *Checker) link(path string) string {
	if c.BaseURL == nil {
		return ""
	}
	if b := strings.TrimRight(c.BaseURL(), "/"); b != "" {
		return b + path
	}
	return ""
}

// Run checks every minute until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	every := c.Every
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Check(ctx)
		}
	}
}

// Incidents returns the open incidents.
func (c *Checker) Incidents() []Incident {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	out := make([]Incident, 0, len(c.incidents))
	for _, i := range c.incidents {
		out = append(out, *i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].key() < out[b].key() })
	return out
}

func (c *Checker) load() {
	if c.incidents != nil {
		return
	}
	c.incidents = map[string]*Incident{}
	v, ok := c.Store.GetKV(incidentsKey)
	if !ok {
		return
	}
	var list []*Incident
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		return
	}
	for _, i := range list {
		c.incidents[i.key()] = i
	}
}

func (c *Checker) save() {
	list := make([]*Incident, 0, len(c.incidents))
	for _, i := range c.incidents {
		list = append(list, i)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].key() < list[b].key() })
	b, _ := json.Marshal(list)
	if err := c.Store.SetKV(incidentsKey, string(b)); err != nil {
		c.logf("monitor: %v", err)
	}
}

// Check evaluates every job and archiver once.
func (c *Checker) Check(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	now := c.now()
	set := c.Store.GetSettings()
	loc := c.Runner.Location()
	jobs, err := c.Store.ListJobs()
	if err != nil {
		c.logf("monitor: %v", err)
		return
	}
	byID := map[int64]*store.Job{}
	for i := range jobs {
		byID[jobs[i].ID] = &jobs[i]
	}
	seen := map[string]bool{}
	changed := false
	if set.FreshnessAlerts {
		grace := time.Duration(set.StaleGraceMinutes) * time.Minute
		for i := range jobs {
			j := &jobs[i]
			ls := c.Store.LastSuccessfulRun(j.ID, j.Method)
			f := Check(j, startOf(ls), now, loc, grace)
			if !f.Monitored {
				continue
			}
			key := KindStale + ":" + strconv.FormatInt(j.ID, 10)
			seen[key] = true
			inc := c.incidents[key]
			switch {
			case f.Stale && inc == nil:
				inc = &Incident{Kind: KindStale, JobID: j.ID, Job: j.Name, Since: f.Due}
				if j.NotifyFailure && c.Send != nil {
					c.Send(c.staleEvent(j, ls, f, now, loc))
					inc.Notified = true
				}
				c.incidents[key] = inc
				changed = true
				c.logf("monitor: job %s is overdue (newest successful backup: %s, due by %s)", j.Name, fmtWhen(f.LastSuccess, loc), fmtWhen(&f.Due, loc))
			case !f.Stale && inc != nil:
				if inc.Notified && j.NotifyFailure && c.Send != nil {
					c.Send(c.freshEvent(j, ls, inc, now, loc))
				}
				delete(c.incidents, key)
				changed = true
				c.logf("monitor: job %s has a fresh backup again", j.Name)
			}
		}
	}
	if limit := time.Duration(set.BinlogLagMinutes) * time.Minute; limit > 0 {
		for _, a := range c.Runner.ArchiverStatuses() {
			key := KindBinlogLag + ":" + strconv.FormatInt(a.JobID, 10)
			seen[key] = true
			lag := time.Duration(a.ArchiveLag) * time.Second
			inc := c.incidents[key]
			j := byID[a.JobID]
			notifies := j != nil && j.NotifyFailure && c.Send != nil
			switch {
			case lag > limit && inc == nil:
				inc = &Incident{Kind: KindBinlogLag, JobID: a.JobID, Job: a.JobName, Since: now.Add(-lag)}
				if notifies {
					c.Send(c.lagEvent(a, lag, limit, now, loc))
					inc.Notified = true
				}
				c.incidents[key] = inc
				changed = true
				c.logf("monitor: binlog archiving of job %s is %s behind", a.JobName, Duration(lag))
			case lag <= limit && inc != nil:
				if inc.Notified && notifies {
					c.Send(c.caughtUpEvent(a, inc, now))
				}
				delete(c.incidents, key)
				changed = true
				c.logf("monitor: binlog archiving of job %s caught up", a.JobName)
			}
		}
	}
	// a job deleted or disabled, alerts or archiving turned off: the incident ends without a message
	for k := range c.incidents {
		if !seen[k] {
			delete(c.incidents, k)
			changed = true
		}
	}
	if changed {
		c.save()
	}
	c.digestIfDue(ctx, now, set, loc)
}

func startOf(r *store.Run) *time.Time {
	if r == nil {
		return nil
	}
	t := r.StartedAt
	return &t
}

func fmtWhen(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "never"
	}
	return t.In(loc).Format("2 Jan 2006, 15:04")
}

func (c *Checker) staleEvent(j *store.Job, ls *store.Run, f Freshness, now time.Time, loc *time.Location) notify.Event {
	ev := notify.Event{Kind: KindStale, Status: store.StatusFailed, Title: "No recent backup: " + j.Name, Time: now,
		URL: c.link(fmt.Sprintf("/#/runs?job_id=%d", j.ID))}
	ev.Add("Server", j.ServerName)
	if f.LastSuccess != nil {
		ev.Add("Last successful backup", fmtWhen(f.LastSuccess, loc)+" ("+Duration(now.Sub(*f.LastSuccess))+" ago)")
	} else {
		ev.Add("Last successful backup", "none yet (job created "+j.CreatedAt.In(loc).Format("2 Jan 2006, 15:04")+")")
	}
	ev.Add("Expected", f.Expected())
	ev.Add("Due by", f.Due.In(loc).Format("2 Jan 2006, 15:04"))
	if lr := c.Store.LastRun(j.ID); lr != nil && (ls == nil || lr.ID != ls.ID) {
		ev.Add("Last run", lr.Status+", "+Duration(now.Sub(lr.StartedAt))+" ago")
		if lr.Status == store.StatusFailed || lr.Status == store.StatusPartial {
			ev.Error = lr.Error
		}
	}
	if c.Runner.IsJobRunning(j.ID) {
		ev.Add("Now", "a backup is running")
	}
	return ev
}

func (c *Checker) freshEvent(j *store.Job, ls *store.Run, inc *Incident, now time.Time, loc *time.Location) notify.Event {
	ev := notify.Event{Kind: KindStale, Status: notify.StatusRecovered, Title: "Backups are fresh again: " + j.Name, Time: now,
		URL: c.link(fmt.Sprintf("/#/runs?job_id=%d", j.ID))}
	ev.Add("Server", j.ServerName)
	if ls != nil {
		ev.Add("Newest backup", fmtWhen(&ls.StartedAt, loc))
		ev.URL = c.link(fmt.Sprintf("/#/runs/%d", ls.ID))
	}
	ev.Add("Overdue for", Duration(now.Sub(inc.Since)))
	return ev
}

func (c *Checker) lagEvent(a runner.ArchiverStatus, lag, limit time.Duration, now time.Time, loc *time.Location) notify.Event {
	ev := notify.Event{Kind: KindBinlogLag, Status: store.StatusFailed, Title: "Binlog archiving is behind: " + a.JobName, Time: now,
		URL: c.link("/#/binlogs"), Error: a.Error}
	if ev.Error == "" {
		ev.Error = a.UploadError
	}
	ev.Add("Server", a.ServerName)
	ev.Add("Behind by", Duration(lag))
	ev.Add("Alert after", Duration(limit))
	ev.Add("State", a.State)
	if a.Pending > 0 {
		ev.Add("Waiting for upload", fmt.Sprintf("%d files", a.Pending))
	}
	if a.ArchivedUntil != nil {
		ev.Add("Recoverable up to", fmtWhen(a.ArchivedUntil, loc))
	}
	return ev
}

func (c *Checker) caughtUpEvent(a runner.ArchiverStatus, inc *Incident, now time.Time) notify.Event {
	ev := notify.Event{Kind: KindBinlogLag, Status: notify.StatusRecovered, Title: "Binlog archiving caught up: " + a.JobName, Time: now,
		URL: c.link("/#/binlogs")}
	ev.Add("Server", a.ServerName)
	ev.Add("Was behind for", Duration(now.Sub(inc.Since)))
	return ev
}

// ---- weekly digest ----

// digestSlot is the latest digest time at or before now.
func digestSlot(now time.Time, weekday, hour int, loc *time.Location) time.Time {
	t := now.In(loc)
	d := time.Date(t.Year(), t.Month(), t.Day(), hour, 0, 0, 0, loc)
	d = d.AddDate(0, 0, -((int(t.Weekday()) - weekday + 7) % 7))
	if d.After(t) {
		d = d.AddDate(0, 0, -7)
	}
	return d
}

func (c *Checker) digestIfDue(ctx context.Context, now time.Time, set store.Settings, loc *time.Location) {
	slot := digestSlot(now, set.DigestWeekday, set.DigestHour, loc)
	v, ok := c.Store.GetKV(digestSentKey)
	last, _ := strconv.ParseInt(v, 10, 64)
	if ok && last >= slot.Unix() {
		return
	}
	// a first start does not send the digest of a past week right away
	if err := c.Store.SetKV(digestSentKey, strconv.FormatInt(slot.Unix(), 10)); err != nil || !ok || c.Digest == nil {
		return
	}
	d := c.BuildDigest(now.AddDate(0, 0, -7), now)
	go c.Digest(ctx, d)
}
