package monitor

import (
	"sort"
	"strings"
	"time"

	"dumpcat/internal/notify"
	"dumpcat/internal/store"
)

// BuildDigest summarizes the backups started in [from, to).
func (c *Checker) BuildDigest(from, to time.Time) *notify.Digest {
	set := c.Store.GetSettings()
	loc := c.Runner.Location()
	d := &notify.Digest{From: from.In(loc), To: to.In(loc), Time: to.In(loc), URL: c.link("/#/dashboard")}
	runs, _ := c.Store.RunsBetween(from, to)
	prev, _ := c.Store.RunsBetween(from.Add(-to.Sub(from)), from)
	for _, r := range prev {
		if counted(r.Status) {
			d.PrevWritten += r.SizeBytes
		}
	}
	type week struct {
		ok, failed, vok, vfail int
		written                int64
	}
	byJob := map[int64]*week{}
	for _, r := range runs {
		if r.JobID == nil {
			continue // the job was deleted since
		}
		w := byJob[*r.JobID]
		if w == nil {
			w = &week{}
			byJob[*r.JobID] = w
		}
		switch {
		case counted(r.Status) && r.Status != store.StatusPartial:
			w.ok++
		case r.Status == store.StatusFailed || r.Status == store.StatusPartial:
			w.failed++
		}
		if counted(r.Status) {
			d.Written += r.SizeBytes
		}
		switch r.VerifyStatus {
		case "ok":
			w.vok++
		case "failed":
			w.vfail++
		}
	}
	jobs, _ := c.Store.ListJobs()
	grace := time.Duration(set.StaleGraceMinutes) * time.Minute
	for i := range jobs {
		j := &jobs[i]
		w := byJob[j.ID]
		if w == nil {
			if !j.Enabled {
				continue
			}
			w = &week{}
		}
		dj := notify.DigestJob{Name: j.Name, Server: j.ServerName, OK: w.ok, Failed: w.failed, VerifyOK: w.vok, VerifyFailed: w.vfail}
		ls := c.Store.LastSuccessfulRun(j.ID, j.Method)
		dj.LastSuccess = startOf(ls)
		if f := c.Store.LastFullBefore(j.ID, to); f != nil {
			dj.Size = f.SizeBytes
		}
		if f := c.Store.LastFullBefore(j.ID, from); f != nil {
			dj.PrevSize = f.SizeBytes
		}
		if f := Check(j, dj.LastSuccess, to, loc, grace); f.Stale {
			dj.Overdue = true
			d.Overdue = append(d.Overdue, notify.DigestOverdue{Job: j.Name, LastSuccess: dj.LastSuccess, Due: f.Due.In(loc)})
		}
		d.Jobs = append(d.Jobs, dj)
	}
	sort.SliceStable(d.Jobs, func(a, b int) bool { return strings.ToLower(d.Jobs[a].Name) < strings.ToLower(d.Jobs[b].Name) })
	use, _ := c.Store.StorageUsage()
	sts, _ := c.Store.ListStorages()
	paths := map[int64]string{}
	for _, st := range sts {
		if st.Type == "local" {
			paths[st.ID] = strings.TrimSpace(st.Config["path"])
		}
	}
	for _, u := range use {
		ds := notify.DigestStorage{Name: u.Name, Type: u.Type, Backups: u.Backups, Bytes: u.Bytes()}
		if p := paths[u.StorageID]; p != "" && c.Disk != nil {
			if total, free, err := c.Disk(p); err == nil {
				ds.Total, ds.Free = total, free
			}
		}
		d.Storages = append(d.Storages, ds)
	}
	return d
}

// counted: the backup was written (retention may have deleted it since).
func counted(status string) bool {
	return status == store.StatusSuccess || status == store.StatusPartial || status == store.StatusDeleted
}
