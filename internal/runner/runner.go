// Package runner orchestrates backups, uploads, retention, verification and
// restores, and tracks live progress of running tasks.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/agent"
	"dumpcat/internal/engine"
	"dumpcat/internal/notify"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

var ErrJobRunning = errors.New("the job is already running")

type Runner struct {
	Store      *store.Store
	Storage    *storage.Manager
	Tools      *engine.Tools
	Notifier   *notify.Notifier
	DataDir    string
	AppVersion string
	BaseURL    func() string
	Agents     *agent.Manager // physical backups of servers reached through an agent

	mu         sync.Mutex
	tasks      map[string]*Task
	jobRunning map[int64]bool
	active     int
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	inUse      map[int64]int // runs read by restore/verify/copy tasks

	fastMu sync.Mutex
	fast   map[int64]*fastOrig // fast-restore settings per server

	archMu    sync.Mutex
	archivers map[int64]*archiver // binlog archivers per job
	binlogUse map[int64]int       // archived binlogs read by PITR restores

	draining bool       // an update is being installed: no new tasks
	starting int        // tasks between their start checks and addTask
	deferred []Deferred // scheduled backups rejected while draining
}

func New(st *store.Store, sm *storage.Manager, tools *engine.Tools, n *notify.Notifier, dataDir, version string) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{Store: st, Storage: sm, Tools: tools, Notifier: n, DataDir: dataDir, AppVersion: version,
		tasks: map[string]*Task{}, jobRunning: map[int64]bool{}, ctx: ctx, cancel: cancel,
		inUse: map[int64]int{}, fast: map[int64]*fastOrig{}, archivers: map[int64]*archiver{}, binlogUse: map[int64]int{}}
	r.BaseURL = func() string { return "" }
	return r
}

// Shutdown cancels running tasks and waits for them (bounded).
func (r *Runner) Shutdown(timeout time.Duration) {
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// ---- tasks & progress ----

type Task struct {
	Key       string
	Kind      string // backup, restore, verify, upload
	ID        int64
	JobID     int64
	Title     string
	StartedAt time.Time
	Raw       pipeline.Counter
	Out       pipeline.Counter
	cancel    context.CancelFunc

	mu        sync.Mutex
	phase     string
	estimate  int64
	uploadPct int
	lastRaw   int64
	lastAt    time.Time
	rate      float64
}

func (t *Task) SetPhase(p string) {
	t.mu.Lock()
	t.phase = p
	t.uploadPct = 0
	t.mu.Unlock()
}

func (t *Task) SetEstimate(n int64) {
	t.mu.Lock()
	t.estimate = n
	t.mu.Unlock()
}

func (t *Task) SetUploadPct(p int) {
	t.mu.Lock()
	t.uploadPct = p
	t.mu.Unlock()
}

type TaskInfo struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	ID        int64     `json:"id"`
	JobID     int64     `json:"job_id"`
	Title     string    `json:"title"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"started_at"`
	RawBytes  int64     `json:"raw_bytes"`
	OutBytes  int64     `json:"out_bytes"`
	Estimate  int64     `json:"estimate_bytes"`
	Rate      float64   `json:"rate"`
	UploadPct int       `json:"upload_pct"`
	Percent   float64   `json:"percent"`
}

func (t *Task) Info() TaskInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw := t.Raw.N()
	now := time.Now()
	if t.lastAt.IsZero() {
		t.lastAt = t.StartedAt
	}
	if dt := now.Sub(t.lastAt).Seconds(); dt >= 2 {
		inst := float64(raw-t.lastRaw) / dt
		if t.rate == 0 {
			t.rate = inst
		} else {
			t.rate = 0.6*t.rate + 0.4*inst
		}
		t.lastRaw, t.lastAt = raw, now
	}
	ti := TaskInfo{Key: t.Key, Kind: t.Kind, ID: t.ID, JobID: t.JobID, Title: t.Title, Phase: t.phase, StartedAt: t.StartedAt,
		RawBytes: raw, OutBytes: t.Out.N(), Estimate: t.estimate, Rate: t.rate, UploadPct: t.uploadPct}
	if t.phase == "upload" {
		ti.Percent = float64(t.uploadPct)
	} else if t.estimate > 0 {
		ti.Percent = float64(raw) * 100 / float64(t.estimate)
		if ti.Percent > 99 {
			ti.Percent = 99
		}
	}
	return ti
}

func (r *Runner) addTask(kind string, id, jobID int64, title string) (*Task, context.Context) {
	ctx, cancel := context.WithCancel(r.ctx)
	t := &Task{Key: fmt.Sprintf("%s:%d", kind, id), Kind: kind, ID: id, JobID: jobID, Title: title, StartedAt: time.Now(), cancel: cancel, phase: "queued"}
	r.mu.Lock()
	r.tasks[t.Key] = t
	r.mu.Unlock()
	return t, ctx
}

func (r *Runner) removeTask(t *Task) {
	r.mu.Lock()
	delete(r.tasks, t.Key)
	if t.Kind == "backup" && t.JobID > 0 {
		delete(r.jobRunning, t.JobID)
	}
	r.mu.Unlock()
	t.cancel()
}

func (r *Runner) Tasks() []TaskInfo {
	r.mu.Lock()
	list := make([]*Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		list = append(list, t)
	}
	r.mu.Unlock()
	out := make([]TaskInfo, 0, len(list))
	for _, t := range list {
		out = append(out, t.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func (r *Runner) Task(key string) (TaskInfo, bool) {
	r.mu.Lock()
	t, ok := r.tasks[key]
	r.mu.Unlock()
	if !ok {
		return TaskInfo{}, false
	}
	return t.Info(), true
}

func (r *Runner) Cancel(key string) bool {
	r.mu.Lock()
	t, ok := r.tasks[key]
	r.mu.Unlock()
	if ok {
		t.cancel()
	}
	return ok
}

func (r *Runner) IsJobRunning(jobID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobRunning[jobID]
}

// useRuns marks backups as being read so retention/deletion leaves them alone.
// The returned func releases them.
func (r *Runner) useRuns(ids ...int64) func() {
	r.mu.Lock()
	for _, id := range ids {
		r.inUse[id]++
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		for _, id := range ids {
			if r.inUse[id]--; r.inUse[id] <= 0 {
				delete(r.inUse, id)
			}
		}
		r.mu.Unlock()
	}
}

func (r *Runner) runInUse(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inUse[id] > 0
}

// useBinlogs protects archived binlogs read by a restore from retention.
func (r *Runner) useBinlogs(ids ...int64) func() {
	r.mu.Lock()
	for _, id := range ids {
		r.binlogUse[id]++
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		for _, id := range ids {
			if r.binlogUse[id]--; r.binlogUse[id] <= 0 {
				delete(r.binlogUse, id)
			}
		}
		r.mu.Unlock()
	}
}

func (r *Runner) binlogInUse(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.binlogUse[id] > 0
}

// acquire waits for a free backup slot (max_concurrent setting).
func (r *Runner) acquire(ctx context.Context) error {
	for {
		max := r.Store.GetSettings().MaxConcurrent
		r.mu.Lock()
		if r.active < max {
			r.active++
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *Runner) release() {
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
}

// ---- paths ----

func (r *Runner) RunLogPath(id int64) string {
	return filepath.Join(r.DataDir, "logs", fmt.Sprintf("run-%d.log", id))
}

func (r *Runner) RestoreLogPath(id int64) string {
	return filepath.Join(r.DataDir, "logs", fmt.Sprintf("restore-%d.log", id))
}

func (r *Runner) stagingRoot() string {
	if d := strings.TrimSpace(r.Store.GetSettings().StagingDir); d != "" {
		// own subfolder: stale-work cleanup must never touch foreign files
		return filepath.Join(d, "dumpcat-staging")
	}
	return filepath.Join(r.DataDir, "staging")
}

var slugRe = regexp.MustCompile(`[^a-z0-9_\-]+`)

// JobDir is the per-job folder on storages: the slug plus the job id, so two
// jobs whose names slugify alike ("Đơn hàng" / "don-hang") never collide.
func JobDir(name string, id int64) string {
	return fmt.Sprintf("%s-%d", Slugify(name, id), id)
}

func Slugify(name string, id int64) string {
	s := strings.ToLower(strings.TrimSpace(name))
	repl := strings.NewReplacer("đ", "d", "à", "a", "á", "a", "ả", "a", "ã", "a", "ạ", "a", "ă", "a", "ằ", "a", "ắ", "a", "ẳ", "a", "ẵ", "a", "ặ", "a",
		"â", "a", "ầ", "a", "ấ", "a", "ẩ", "a", "ẫ", "a", "ậ", "a", "è", "e", "é", "e", "ẻ", "e", "ẽ", "e", "ẹ", "e", "ê", "e", "ề", "e", "ế", "e",
		"ể", "e", "ễ", "e", "ệ", "e", "ì", "i", "í", "i", "ỉ", "i", "ĩ", "i", "ị", "i", "ò", "o", "ó", "o", "ỏ", "o", "õ", "o", "ọ", "o", "ô", "o",
		"ồ", "o", "ố", "o", "ổ", "o", "ỗ", "o", "ộ", "o", "ơ", "o", "ờ", "o", "ớ", "o", "ở", "o", "ỡ", "o", "ợ", "o", "ù", "u", "ú", "u", "ủ", "u",
		"ũ", "u", "ụ", "u", "ư", "u", "ừ", "u", "ứ", "u", "ử", "u", "ữ", "u", "ự", "u", "ỳ", "y", "ý", "y", "ỷ", "y", "ỹ", "y", "ỵ", "y")
	s = repl.Replace(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = fmt.Sprintf("job-%d", id)
	}
	return s
}

// Location returns the configured timezone for schedules and display.
func (r *Runner) Location() *time.Location { return r.location() }

func (r *Runner) location() *time.Location {
	if tz := r.Store.GetSettings().Timezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.Local
}

func (r *Runner) link(path string) string {
	if r.BaseURL == nil {
		return ""
	}
	b := r.BaseURL()
	if b == "" {
		return ""
	}
	return strings.TrimRight(b, "/") + path
}

func humanBytes(n int64) string {
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

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// RecoverInterrupted settles tasks interrupted by a restart and removes the
// incomplete data they were writing (in the background).
func (r *Runner) RecoverInterrupted() {
	partial := r.Store.MarkInterrupted()
	if len(partial) == 0 {
		return
	}
	go func() {
		for _, l := range partial {
			if l.StorageID == nil {
				continue
			}
			st, err := r.Store.GetStorage(*l.StorageID)
			if err != nil {
				continue
			}
			b, err := r.Storage.Get(st)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := b.DeleteDir(ctx, l.Path); err == nil {
				log.Printf("recover: removed incomplete data %s on %s", l.Path, st.Name)
			}
			cancel()
		}
	}()
}

// CleanupStaleWork removes leftover staging/work directories (after crash).
func (r *Runner) CleanupStaleWork() {
	root := r.stagingRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	r.mu.Lock()
	active := map[string]bool{}
	for _, t := range r.tasks {
		active[fmt.Sprintf("%s-%d", t.Kind, t.ID)] = true
	}
	r.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		key := strings.TrimPrefix(strings.TrimPrefix(name, "work-"), "stage-")
		if active[key] {
			continue
		}
		if strings.HasPrefix(name, "work-") || strings.HasPrefix(name, "stage-") {
			if err := os.RemoveAll(filepath.Join(root, name)); err == nil {
				log.Printf("cleanup: removed stale %s", name)
			}
		}
	}
}
