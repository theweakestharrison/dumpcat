package monitor

import (
	"runtime"
	"sort"
	"strings"
	"time"

	"dumpcat/internal/store"
)

// PeerState is the live state of a WireGuard peer.
type PeerState struct {
	Online        bool
	LastHandshake time.Time
}

// MetricSources are the inputs of the metrics that are not in the store.
type MetricSources struct {
	Version     string
	Started     time.Time
	DataDir     string
	AgentOnline func(agentID int64) bool                         // nil: agents are not available
	WireGuard   func() (running bool, peers map[int64]PeerState) // nil: WireGuard is not available
}

func unix(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

// Metrics gathers the Prometheus metrics of DumpCat.
func (c *Checker) Metrics(src MetricSources) *Metrics {
	m := &Metrics{}
	now := c.now()
	set := c.Store.GetSettings()
	loc := c.Runner.Location()
	grace := time.Duration(set.StaleGraceMinutes) * time.Minute

	m.Gauge("dumpcat_build_info", "DumpCat version and build (always 1).").
		Add(1, "version", src.Version, "goversion", runtime.Version(), "os", runtime.GOOS, "arch", runtime.GOARCH)
	if !src.Started.IsZero() {
		m.Gauge("dumpcat_start_time_seconds", "When DumpCat started (Unix time).").Add(unix(src.Started))
	}

	// jobs
	enabled := m.Gauge("dumpcat_job_enabled", "1 when the job runs on its schedule.")
	running := m.Gauge("dumpcat_job_running", "1 while a backup of the job runs.")
	lastOK := m.Gauge("dumpcat_job_last_success_timestamp_seconds", "Start of the newest successful backup (Unix time).")
	lastRun := m.Gauge("dumpcat_job_last_run_timestamp_seconds", "Start of the newest finished backup (Unix time).")
	lastStatus := m.Gauge("dumpcat_job_last_run_status", "Status of the newest finished backup (1 for the status label).")
	lastSuccess := m.Gauge("dumpcat_job_last_run_success", "1 when the newest finished backup succeeded, 0 otherwise.")
	lastDur := m.Gauge("dumpcat_job_last_run_duration_seconds", "Duration of the newest finished backup.")
	size := m.Gauge("dumpcat_job_last_backup_size_bytes", "Size of the newest successful backup as stored (compressed, encrypted).")
	rawSize := m.Gauge("dumpcat_job_last_backup_raw_bytes", "Uncompressed size of the newest successful backup.")
	verified := m.Gauge("dumpcat_job_last_verify_timestamp_seconds", "When a backup of the job was last checked (Unix time).")
	verifyOK := m.Gauge("dumpcat_job_last_verify_success", "1 when the last integrity check passed, 0 when it failed.")
	stale := m.Gauge("dumpcat_job_stale", "1 when the job has no successful backup within its expected window.")
	due := m.Gauge("dumpcat_job_backup_due_timestamp_seconds", "When a newer successful backup is due at the latest (Unix time).")
	maxAge := m.Gauge("dumpcat_job_max_age_seconds", "Age of the newest successful backup that is tolerated.")
	jobs, _ := c.Store.ListJobs()
	for i := range jobs {
		j := &jobs[i]
		l := []string{"job", j.Name, "server", j.ServerName, "method", j.Method}
		enabled.Bool(j.Enabled, l...)
		running.Bool(c.Runner.IsJobRunning(j.ID), l...)
		ls := c.Store.LastSuccessfulRun(j.ID, j.Method)
		if ls != nil {
			lastOK.Add(unix(ls.StartedAt), l...)
			size.Add(float64(ls.SizeBytes), l...)
			rawSize.Add(float64(ls.RawBytes), l...)
		}
		if lr := c.Store.LastFinishedRun(j.ID); lr != nil {
			lastRun.Add(unix(lr.StartedAt), l...)
			lastStatus.Add(1, append(l, "status", lr.Status)...)
			lastSuccess.Bool(lr.Status == store.StatusSuccess, l...)
			if lr.FinishedAt != nil {
				lastDur.Add(lr.FinishedAt.Sub(lr.StartedAt).Seconds(), l...)
			}
		}
		if v := c.Store.LastVerifiedRun(j.ID); v != nil && v.VerifiedAt != nil {
			verified.Add(unix(*v.VerifiedAt), l...)
			verifyOK.Bool(v.VerifyStatus == "ok", l...)
		}
		if f := Check(j, startOf(ls), now, loc, grace); f.Monitored {
			stale.Bool(f.Stale, l...)
			due.Add(unix(f.Due), l...)
			maxAge.Add(f.Window.Seconds(), l...)
		}
	}
	if counts, err := c.Store.RunCounts(); err == nil {
		g := m.Gauge("dumpcat_catalog_backups", "Finished backups in the catalog, by status.")
		for _, st := range []string{store.StatusSuccess, store.StatusPartial, store.StatusFailed, store.StatusCanceled, store.StatusDeleted} {
			g.Add(float64(counts[st]), "status", st)
		}
	}
	kinds := map[string]int{"backup": 0, "restore": 0, "verify": 0}
	for _, t := range c.Runner.Tasks() {
		kinds[t.Kind]++
	}
	tasks := m.Gauge("dumpcat_tasks_running", "Tasks running now, by kind.")
	for _, k := range sortedKeys(kinds) {
		tasks.Add(float64(kinds[k]), "kind", k)
	}
	alerts := map[string]int{KindStale: 0, KindBinlogLag: 0}
	for _, i := range c.Incidents() {
		alerts[i.Kind]++
	}
	open := m.Gauge("dumpcat_alerts_open", "Freshness and log archive lag alerts that have not cleared.")
	for _, k := range sortedKeys(alerts) {
		open.Add(float64(alerts[k]), "kind", k)
	}

	// log archiving (MySQL binlogs, PostgreSQL WAL; the series names predate PostgreSQL)
	lag := m.Gauge("dumpcat_binlog_archive_lag_seconds", "How long the log archive (binlog / WAL) has been behind the server.")
	streaming := m.Gauge("dumpcat_binlog_streaming", "1 while the log stream (binlog / WAL) of the job is connected.")
	pending := m.Gauge("dumpcat_binlog_pending_files", "Closed log files (binlogs / WAL segments) waiting for upload.")
	until := m.Gauge("dumpcat_binlog_archived_until_timestamp_seconds", "Last event of the newest archived log file: the latest point-in-time recovery target (Unix time).")
	for _, a := range c.Runner.ArchiverStatuses() {
		l := []string{"job", a.JobName, "server", a.ServerName}
		lag.Add(float64(a.ArchiveLag), l...)
		streaming.Bool(a.State == "streaming", l...)
		pending.Add(float64(a.Pending), l...)
		if a.ArchivedUntil != nil {
			until.Add(unix(*a.ArchivedUntil), l...)
		}
	}

	// storage
	backups := m.Gauge("dumpcat_storage_backups", "Backups kept on the storage.")
	used := m.Gauge("dumpcat_storage_used_bytes", "Bytes DumpCat keeps on the storage (backups and archived logs).")
	binlogs := m.Gauge("dumpcat_storage_binlog_bytes", "Bytes of archived log files (binlogs / WAL) on the storage.")
	free := m.Gauge("dumpcat_storage_free_bytes", "Free space of a local storage's file system.")
	total := m.Gauge("dumpcat_storage_size_bytes", "Size of a local storage's file system.")
	use, _ := c.Store.StorageUsage()
	sts, _ := c.Store.ListStorages()
	paths := map[int64]string{}
	for _, st := range sts {
		if st.Type == "local" {
			paths[st.ID] = strings.TrimSpace(st.Config["path"])
		}
	}
	for _, u := range use {
		l := []string{"storage", u.Name, "type", u.Type}
		backups.Add(float64(u.Backups), l...)
		used.Add(float64(u.Bytes()), l...)
		binlogs.Add(float64(u.BinlogBytes), l...)
		if p := paths[u.StorageID]; p != "" && c.Disk != nil {
			if t, f, err := c.Disk(p); err == nil {
				free.Add(float64(f), l...)
				total.Add(float64(t), l...)
			}
		}
	}
	if src.DataDir != "" && c.Disk != nil {
		if t, f, err := c.Disk(src.DataDir); err == nil {
			m.Gauge("dumpcat_data_dir_free_bytes", "Free space where DumpCat keeps its data.").Add(float64(f))
			m.Gauge("dumpcat_data_dir_size_bytes", "Size of the file system where DumpCat keeps its data.").Add(float64(t))
		}
	}

	// agents and WireGuard
	if src.AgentOnline != nil {
		online := m.Gauge("dumpcat_agent_online", "1 while the agent is connected.")
		seen := m.Gauge("dumpcat_agent_last_seen_timestamp_seconds", "When the agent last connected (Unix time).")
		agents, _ := c.Store.ListAgents()
		for _, a := range agents {
			online.Bool(src.AgentOnline(a.ID), "agent", a.Name)
			if a.LastSeenAt != nil {
				seen.Add(unix(*a.LastSeenAt), "agent", a.Name)
			}
		}
	}
	if src.WireGuard != nil {
		up, peers := src.WireGuard()
		m.Gauge("dumpcat_wireguard_up", "1 while the built-in WireGuard runs.").Bool(up)
		online := m.Gauge("dumpcat_wireguard_peer_online", "1 when the peer made a handshake in the last 3 minutes.")
		hs := m.Gauge("dumpcat_wireguard_peer_last_handshake_timestamp_seconds", "Last handshake with the peer (Unix time).")
		list, _ := c.Store.ListWGPeers()
		for _, p := range list {
			st := peers[p.ID]
			online.Bool(st.Online, "peer", p.Name)
			if !st.LastHandshake.IsZero() {
				hs.Add(unix(st.LastHandshake), "peer", p.Name)
			}
		}
	}
	return m
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
