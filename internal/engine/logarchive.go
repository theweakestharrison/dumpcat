package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Continuous log archiving and point-in-time recovery. The runner owns what
// every engine shares: the supervised archiver of each job, staging, the
// upload of closed files to the job's storages through the compression /
// encryption pipeline, the catalog, retention, lag monitoring, the PITR plan
// and the download and checksum checks of a restore. A driver implementing
// LogArchiver receives, names, scans and replays its own log: MySQL binary
// logs, PostgreSQL WAL. See docs/design-multidb.md.

// LogInfo names a driver's log for people and the runner.
type LogInfo struct {
	Name    string   `json:"name"`    // "binlog", "WAL"
	File    string   `json:"file"`    // one file: "binlog", "WAL segment"
	Files   string   `json:"files"`   // "binlogs", "WAL segments"
	Prefix  string   `json:"prefix"`  // staging folder, archive log and storage folder: "binlog", "wal"
	Targets []string `json:"targets"` // PITR target types in the order the UI offers them
	Close   string   `json:"close"`   // what closing the current file runs: "FLUSH BINARY LOGS", "pg_switch_wal()"
	// Recovery: the restore replays the log itself (PostgreSQL recovery after
	// a physical restore, SQL Server's RESTORE LOG in the driver's Restore),
	// so Replay is not called after it.
	Recovery bool `json:"recovery"`
}

// LogArchiver is the optional part of a Driver that archives the server's
// transaction log continuously and replays it for point-in-time recovery.
type LogArchiver interface {
	LogInfo() LogInfo
	// NewStream returns the receiving side of one job; it lives as long as
	// the job's archiver and keeps its state between sessions.
	NewStream(h *LogHost) LogStream
	// CheckTarget validates the engine's own target types (time and latest
	// are checked by NormalizeTarget).
	CheckTarget(t *PITRTarget) error
	// Base tells where replay starts from run, or why run cannot be the
	// base backup of a recovery to t.
	Base(run *store.Run, t *PITRTarget) (*LogBase, error)
	// Plan picks the files to fetch after the base.
	Plan(in *PlanInput) (*LogPlan, error)
	// NewCheck checks the fetched files in order, before anything is changed
	// on the target, and resolves where the replay stops.
	NewCheck(env *Env, plan *LogPlan) LogCheck
	// Replay applies the checked files after the base restore (not called
	// when LogInfo().Recovery is set).
	Replay(env *Env, rp *Replay, spec *ReplaySpec) error
}

// LogHost is what the runner gives a driver's stream.
type LogHost struct {
	JobID    int64
	JobUUID  string
	Dir      string // private staging folder of the job (0700)
	Log      *tasklog.Logger
	Tools    *Tools
	Store    *store.Store
	Location *time.Location
	Update   func(func(st *LogStatus)) // changes the live status
	Status   func() LogStatus          // the live status
	Kick     func()                    // a file was closed: upload now
}

// LogStream receives one job's log into its staging folder.
type LogStream interface {
	// Session connects and receives until the receiving process stops or
	// ctx ends; the runner starts it again with a back-off. rotate asks to
	// close the current file now.
	Session(ctx context.Context, job *store.Job, server *store.Server, rotate <-chan struct{}) error
	// Closed lists the staged files ready for upload, oldest first.
	Closed() []string
	// Seal reads a closed file before its upload (and repairs what can be:
	// a truncated tail, a copy that differs from the server).
	Seal(ctx context.Context, job *store.Job, name string) (*LogFileInfo, error)
	// Archived is called once every storage of the job holds name.
	Archived(name string)
	// Current describes the file being received.
	Current() LogCurrent
	// Snapshot copies the complete part of the file being received to dst,
	// for a restore that needs it.
	Snapshot(dst string) (*LogSnapshot, error)
	// Release undoes what archiving keeps on the server once it is switched
	// off for good (PostgreSQL: the replication slot), logging to lg. Not
	// called when DumpCat stops.
	Release(ctx context.Context, server *store.Server, lg *tasklog.Logger)
}

// LogFileInfo describes a closed file about to be archived.
type LogFileInfo struct {
	Name        string // its name in the archive ("": the staged name)
	Seq         int64
	First, Last time.Time // first and last timed event (zero: none)
	Size        int64     // bytes archived
	Skip        bool      // nothing worth archiving: the staged file is dropped
	Meta        map[string]any
}

// LogCurrent is the file being received.
type LogCurrent struct {
	Name        string    // "" when nothing is received yet
	Size        int64     // bytes received in it
	Events      bool      // it holds at least one complete event
	First, Last time.Time // its first and last timed event
	Meta        map[string]any
	Status      LogStatus // the archiver at that moment
}

// LogSnapshot is a copy of the file being received.
type LogSnapshot struct {
	Name      string // the file's name in the archive
	Size      int64
	Last      time.Time // last timed event
	CaughtUp  bool      // it held everything the server had written at CheckedAt
	CheckedAt time.Time
	Meta      map[string]any
}

// LogStatus is the live state of a job's archiver (UI, dashboard, monitor).
type LogStatus struct {
	JobID          int64      `json:"job_id"`
	JobName        string     `json:"job_name"`
	ServerName     string     `json:"server_name"`
	Log            string     `json:"log"`   // "binlog", "WAL"
	State          string     `json:"state"` // starting, streaming, retrying, stopped
	Error          string     `json:"error"`
	ErrorAt        *time.Time `json:"error_at"`
	NextTry        *time.Time `json:"next_try"`
	Since          *time.Time `json:"since"`
	Restarts       int        `json:"restarts"`
	Current        string     `json:"current_file"`
	Received       int64      `json:"received_bytes"`
	ReceivedAt     *time.Time `json:"received_at"`     // time of the last event received
	ServerPosition string     `json:"server_position"` // where the server writes: mysql-bin.000012:4711, 0/3000148
	CheckedAt      *time.Time `json:"checked_at"`
	LagBytes       int64      `json:"lag_bytes"`
	LagSeconds     int64      `json:"lag_seconds"`
	Pending        int        `json:"pending_files"`
	PendingBytes   int64      `json:"pending_bytes"`
	UploadError    string     `json:"upload_error"`
	LastArchived   string     `json:"last_archived"`
	ArchivedAt     *time.Time `json:"archived_at"`
	ArchivedUntil  *time.Time `json:"archived_until"` // last event of the newest archived file
	RotatedAt      *time.Time `json:"rotated_at"`
	StartedAt      time.Time  `json:"started_at"`
	CaughtUpAt     *time.Time `json:"caught_up_at"`        // the stream was last level with the server
	PendingSince   *time.Time `json:"pending_since"`       // the oldest closed file waiting for upload
	ArchiveLag     int64      `json:"archive_lag_seconds"` // how long the archive has been behind the server
	// PostgreSQL: the replication slot and the WAL it keeps on the server
	Slot         string `json:"slot,omitempty"`
	SlotRetained int64  `json:"slot_retained_bytes,omitempty"`
}

// ArchiveLag is how long the archive has been behind the server: since the
// stream was last level with it (or started), or since the oldest file
// waiting for upload was closed, whichever is longer.
func (st *LogStatus) ArchiveLagAt(now time.Time) time.Duration {
	var lag time.Duration
	level := st.State == "streaming" && st.LagBytes == 0 && st.CheckedAt != nil && now.Sub(*st.CheckedAt) < 2*time.Minute
	if !level {
		ref := st.StartedAt
		if st.CaughtUpAt != nil && st.CaughtUpAt.After(ref) {
			ref = *st.CaughtUpAt
		}
		lag = now.Sub(ref)
	}
	if st.Pending > 0 && st.PendingSince != nil {
		lag = max(lag, now.Sub(*st.PendingSince))
	}
	return max(lag, 0)
}

// Level tells whether the stream held everything the server had written
// at the last check (made within the given time; 0: any time).
func (st *LogStatus) Level(current string, within time.Duration) bool {
	return st.State == "streaming" && st.LagBytes == 0 && st.Current == current && st.CheckedAt != nil &&
		(within <= 0 || time.Since(*st.CheckedAt) < within)
}

// ---- point-in-time recovery ----

// PITRTarget is where a point-in-time recovery stops.
type PITRTarget struct {
	Type string     `json:"type"` // time, latest; MySQL: position, gtid; PostgreSQL: lsn, name; SQL Server: lsn, mark
	Time *time.Time `json:"time,omitempty"`
	File string     `json:"file,omitempty"` // MySQL position
	Pos  int64      `json:"pos,omitempty"`
	GTID string     `json:"gtid,omitempty"`
	LSN  string     `json:"lsn,omitempty"`  // PostgreSQL, SQL Server
	Name string     `json:"name,omitempty"` // PostgreSQL restore point, SQL Server marked transaction
}

// NormalizeTarget checks t against the types la offers and clears the
// fields the type does not use.
func NormalizeTarget(la LogArchiver, t *PITRTarget) error {
	ok := false
	for _, ty := range la.LogInfo().Targets {
		ok = ok || ty == t.Type
	}
	if !ok {
		return fmt.Errorf("invalid recovery target type: %q", t.Type)
	}
	switch t.Type {
	case "time":
		if t.Time == nil || t.Time.IsZero() {
			return errors.New("no recovery time selected")
		}
		if t.Time.After(time.Now().Add(time.Minute)) {
			return errors.New("the recovery time is in the future")
		}
		*t = PITRTarget{Type: t.Type, Time: t.Time}
		return nil
	case "latest":
		*t = PITRTarget{Type: t.Type}
		return nil
	}
	return la.CheckTarget(t)
}

// Describe says where the recovery stops, for people.
func (t PITRTarget) Describe(loc *time.Location) string {
	switch t.Type {
	case "time":
		return "time " + t.Time.In(loc).Format("2006-01-02 15:04:05")
	case "position":
		return fmt.Sprintf("position %s:%d", t.File, t.Pos)
	case "gtid":
		return "just before GTID " + t.GTID
	case "lsn":
		return "LSN " + t.LSN
	case "name":
		return fmt.Sprintf("restore point %q", t.Name)
	case "mark":
		return fmt.Sprintf("marked transaction %q", t.Name)
	}
	return "the latest archived log"
}

// LogBase is a backup a recovery can start from.
type LogBase struct {
	Run          *store.Run
	From         string    // where replay starts, for people
	ConsistentAt time.Time // the backup's data is not newer than this
	State        any       // the driver's own (positions of the backup)
}

// PlanInput is what a plan is made from.
type PlanInput struct {
	Base    *LogBase
	Files   []store.LogFile // the job's archived files held by a storage, in archive order
	Current LogCurrent      // the file being received (Name "" when the archiver is off)
	Target  PITRTarget
	Loc     *time.Location // times in messages
}

// LogPlan is what a recovery fetches and replays after the base backup.
type LogPlan struct {
	Target   PITRTarget
	Base     *LogBase
	Files    []store.LogFile // archived files to fetch, in replay order
	Partial  string          // the file being received, replayed after Files
	Coverage time.Time       // events are complete up to here
	Bytes    int64
	Warnings []string
	State    any // the driver's own
}

// FileIDs lists the catalog ids of the planned files.
func (p *LogPlan) FileIDs() []int64 {
	ids := make([]int64, len(p.Files))
	for i, f := range p.Files {
		ids[i] = f.ID
	}
	return ids
}

// LocalLog is a fetched file, decoded and checksum-checked.
type LocalLog struct {
	Name string         // its name in the archive
	Path string         // the local copy
	File *store.LogFile // nil for the file being received
}

// LogCheck checks fetched files in replay order.
type LogCheck interface {
	// Add checks the next file; done tells that the stop point is known and
	// later files are not needed.
	Add(f LocalLog) (done bool, err error)
	// Finish resolves the replay once every needed file was added.
	Finish() (*Replay, error)
}

// Replay is a checked recovery: the files to apply and where to stop.
type Replay struct {
	Plan  *LogPlan
	Files []LocalLog
	Stop  string // where the replay stops, for the log
	State any    // the driver's own
}

// ReplaySpec is the restore a replay follows.
type ReplaySpec struct {
	Run       *store.Run
	Target    *store.Server
	Databases []store.DBEntry   // the restored databases (logical restores)
	Rename    map[string]string // source name → restored name
	NoBinlog  bool
	FastMode  func() (release func()) // MySQL: relax durability while replaying (nil: off)
	Progress  func(rawBytes int64)    // a file (of that size) was applied
	Estimate  func(rawBytes int64)    // bytes the replay will apply
}

// ConsistentAt is an upper bound of the moment a backup's data reflects:
// recorded precisely by newer backups, otherwise the end of the run.
func ConsistentAt(run *store.Run) time.Time {
	if v, ok := MetaInt(run.Meta, "consistent_at"); ok && v > 0 {
		return time.Unix(v, 0)
	}
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	return run.StartedAt
}

// MetaInt reads an integer of a run's meta (JSON numbers, strings).
func MetaInt(m map[string]any, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case uint64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// LogStored tells whether a storage holds f.
func LogStored(f *store.LogFile) bool {
	for _, l := range f.Locations {
		if l.Status == store.LocOK && l.StorageID != nil {
			return true
		}
	}
	return false
}

// TimeLimit is how far after a target time files are still fetched: commit
// times only lag file creation by a moment, so a file whose first event is
// well after the target cannot hold anything older than it.
const TimeLimit = 5 * time.Minute

// CoverageError explains that the archive does not reach a target time yet.
func CoverageError(info LogInfo, cov time.Time, loc *time.Location) error {
	return fmt.Errorf("%s are only archived up to %s — pick an earlier time, close the current %s now and wait for the upload, or choose 'Latest'",
		info.Files, cov.In(loc).Format("2006-01-02 15:04:05"), info.File)
}
