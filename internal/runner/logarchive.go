package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/drivers"
	"dumpcat/internal/engine"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Continuous log archiving: one supervised archiver per job receives the
// server's transaction log (MySQL binlogs, PostgreSQL WAL) through its
// driver's LogStream into a private staging folder. Every file the stream
// has closed is compressed/encrypted like a backup file, uploaded to the
// job's storages and recorded in the catalog; the file still being written
// stays local until it is closed.

var errRestart = errors.New("restart requested")

// ArchiverStatus is the live state of a job's archiver (shown in the UI).
type ArchiverStatus = engine.LogStatus

type archiver struct {
	r        *Runner
	la       engine.LogArchiver
	info     engine.LogInfo
	stream   engine.LogStream
	jobID    int64
	serverID int64
	dir      string
	cancel   context.CancelFunc
	done     chan struct{}
	kick     chan struct{}
	rotate   chan struct{}
	restart  chan struct{}
	lg       *tasklog.Logger

	mu sync.Mutex
	st ArchiverStatus

	nextTry  map[string]time.Time
	attempts map[string]int
	fails    int
	notified bool
}

// logArchiverOf returns the log archiver of a job's method (nil: none).
func logArchiverOf(method string) engine.LogArchiver {
	m, drv, ok := drivers.Method(method)
	if !ok || !m.LogArchive {
		return nil
	}
	la, _ := drv.(engine.LogArchiver)
	return la
}

// ArchiveLogPath is the archiver log of a job (binlog-<id>.log, wal-<id>.log).
func (r *Runner) ArchiveLogPath(jobID int64) string {
	prefix := "binlog"
	if j, err := r.Store.GetJob(jobID); err == nil {
		if la := logArchiverOf(j.Method); la != nil {
			prefix = la.LogInfo().Prefix
		}
	}
	return filepath.Join(r.DataDir, "logs", fmt.Sprintf("%s-%d.log", prefix, jobID))
}

func (r *Runner) logStageDir(prefix string, jobID int64) string {
	return filepath.Join(r.stagingRoot(), fmt.Sprintf("%s-%d", prefix, jobID))
}

// SyncArchivers starts/stops archivers to match the job configuration. An
// archiver that is no longer wanted releases what it keeps on the server.
func (r *Runner) SyncArchivers() {
	jobs, err := r.Store.ListJobs()
	if err != nil {
		return
	}
	exists := map[int64]bool{}
	want := map[int64]*store.Job{}
	for i := range jobs {
		j := &jobs[i]
		exists[j.ID] = true
		if j.Enabled && j.Options.LogArchive && logArchiverOf(j.Method) != nil {
			want[j.ID] = j
		}
	}
	r.archMu.Lock()
	defer r.archMu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	for id, a := range r.archivers {
		j, ok := want[id]
		if ok && j.ServerID == a.serverID && logArchiverOf(j.Method) == a.la {
			continue
		}
		a.stop()
		a.release()
		delete(r.archivers, id)
	}
	// staged files of deleted jobs are never uploaded any more
	prefixes := map[string]bool{}
	for _, d := range drivers.All() {
		if la, ok := d.(engine.LogArchiver); ok {
			prefixes[la.LogInfo().Prefix] = true
		}
	}
	if entries, err := os.ReadDir(r.stagingRoot()); err == nil {
		for _, e := range entries {
			prefix, num, ok := strings.Cut(e.Name(), "-")
			var id int64
			if _, err := fmt.Sscanf(num, "%d", &id); ok && err == nil && prefixes[prefix] && e.IsDir() && !exists[id] {
				os.RemoveAll(filepath.Join(r.stagingRoot(), e.Name()))
			}
		}
	}
	for id, j := range want {
		if _, ok := r.archivers[id]; !ok {
			r.archivers[id] = r.startArchiver(j)
		}
	}
}

// StopArchiver stops a job's archiver for good (the job is being deleted).
func (r *Runner) StopArchiver(jobID int64) {
	r.archMu.Lock()
	defer r.archMu.Unlock()
	if a, ok := r.archivers[jobID]; ok {
		a.stop()
		a.release()
		delete(r.archivers, jobID)
	}
}

func (r *Runner) startArchiver(j *store.Job) *archiver {
	la := logArchiverOf(j.Method)
	info := la.LogInfo()
	ctx, cancel := context.WithCancel(r.ctx)
	a := &archiver{r: r, la: la, info: info, jobID: j.ID, serverID: j.ServerID, dir: r.logStageDir(info.Prefix, j.ID), cancel: cancel, done: make(chan struct{}),
		kick: make(chan struct{}, 1), rotate: make(chan struct{}, 1), restart: make(chan struct{}, 1),
		nextTry: map[string]time.Time{}, attempts: map[string]int{}}
	a.st = ArchiverStatus{JobID: j.ID, JobName: j.Name, ServerName: j.ServerName, Log: info.Name, State: "starting", StartedAt: time.Now()}
	if last := r.Store.LastLogFile(j.UUID); last != nil {
		a.st.LastArchived = last.FileName
		ca := last.CreatedAt
		a.st.ArchivedAt = &ca
		if !last.LastTS.IsZero() {
			lt := last.LastTS
			a.st.ArchivedUntil = &lt
		}
	}
	a.lg = openArchiveLog(filepath.Join(r.DataDir, "logs", fmt.Sprintf("%s-%d.log", info.Prefix, j.ID)))
	a.stream = la.NewStream(&engine.LogHost{JobID: j.ID, JobUUID: j.UUID, Dir: a.dir, Log: a.lg, Tools: r.Tools, Store: r.Store, Location: r.location(),
		Update: a.set, Status: a.status, Kick: func() { poke(a.kick) }})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(a.done)
		defer a.lg.Close()
		a.run(ctx)
	}()
	return a
}

func (a *archiver) stop() {
	a.cancel()
	select {
	case <-a.done:
	case <-time.After(30 * time.Second):
	}
}

// release undoes what archiving keeps on the server (after stop).
func (a *archiver) release() {
	server, err := a.r.Store.GetServer(a.serverID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lg := openArchiveLog(filepath.Join(a.r.DataDir, "logs", fmt.Sprintf("%s-%d.log", a.info.Prefix, a.jobID)))
	defer lg.Close()
	lg.Redact(server.Password)
	a.stream.Release(ctx, server, lg)
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *Runner) archiver(jobID int64) *archiver {
	r.archMu.Lock()
	defer r.archMu.Unlock()
	return r.archivers[jobID]
}

// ArchiverStatuses returns the state of every running archiver.
func (r *Runner) ArchiverStatuses() []ArchiverStatus {
	r.archMu.Lock()
	list := make([]*archiver, 0, len(r.archivers))
	for _, a := range r.archivers {
		list = append(list, a)
	}
	r.archMu.Unlock()
	out := make([]ArchiverStatus, 0, len(list))
	for _, a := range list {
		out = append(out, a.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobName < out[j].JobName })
	return out
}

func (r *Runner) ArchiverStatus(jobID int64) (ArchiverStatus, bool) {
	a := r.archiver(jobID)
	if a == nil {
		return ArchiverStatus{}, false
	}
	return a.status(), true
}

// RotateLog asks the server to close the current file so that it is archived now.
func (r *Runner) RotateLog(jobID int64) error {
	a := r.archiver(jobID)
	if a == nil {
		return errors.New("log archiving is not enabled for this job")
	}
	poke(a.rotate)
	return nil
}

func (r *Runner) RestartArchiver(jobID int64) error {
	a := r.archiver(jobID)
	if a == nil {
		return errors.New("log archiving is not enabled for this job")
	}
	poke(a.restart)
	return nil
}

func (a *archiver) status() ArchiverStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.st
	st.ArchiveLag = int64(st.ArchiveLagAt(time.Now()).Seconds())
	return st
}

// set changes the status; a stream that has been healthy for a minute
// clears the last error and the failure count.
func (a *archiver) set(fn func(st *ArchiverStatus)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.st)
	st := &a.st
	now := time.Now()
	if st.State == "streaming" && st.Error != "" && st.Since != nil && now.Sub(*st.Since) > time.Minute &&
		st.CheckedAt != nil && now.Sub(*st.CheckedAt) < 30*time.Second {
		st.Error, st.ErrorAt = "", nil
		a.fails, a.notified = 0, false
	}
}

func openArchiveLog(p string) *tasklog.Logger {
	// long running: keep one previous generation instead of growing forever
	if st, err := os.Stat(p); err == nil && st.Size() > 8<<20 {
		_ = os.Rename(p, p+".1")
	}
	lg, err := tasklog.Open(p)
	if err != nil {
		return tasklog.Discard()
	}
	return lg
}

func (a *archiver) run(ctx context.Context) {
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		a.lg.Printf("ERROR: cannot create staging folder %s: %v", a.dir, err)
	}
	a.lg.Printf("DumpCat %s — %s archiving started (staging folder %s)", a.r.AppVersion, a.info.Name, a.dir)
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		a.uploader(ctx)
	}()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(began) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		if errors.Is(err, errRestart) {
			a.lg.Printf("Restarting the %s stream", a.info.Name)
			continue
		}
		a.failed(err)
		next := time.Now().Add(backoff)
		a.set(func(st *ArchiverStatus) { st.State, st.NextTry = "retrying", &next; st.Restarts++ })
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		case <-a.restart:
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
	<-upDone
	a.set(func(st *ArchiverStatus) { st.State = "stopped" })
	a.lg.Printf("%s archiving stopped", strings.ToUpper(a.info.Name[:1])+a.info.Name[1:])
}

// session runs one receiving session of the stream; a restart request
// cancels it.
func (a *archiver) session(ctx context.Context) error {
	job, err := a.r.Store.GetJob(a.jobID)
	if err != nil {
		return err
	}
	server, err := a.r.Store.GetServer(job.ServerID)
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}
	a.lg.Redact(server.Password)
	// what the last session saw of the server does not hold for this one
	a.set(func(st *ArchiverStatus) { st.CheckedAt = nil })
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-a.restart:
			cancel(errRestart)
		case <-sctx.Done():
		}
	}()
	err = a.stream.Session(sctx, job, server, a.rotate)
	if cause := context.Cause(sctx); ctx.Err() == nil && errors.Is(cause, errRestart) {
		return errRestart
	}
	return err
}

func (a *archiver) failed(err error) {
	if err == nil {
		err = errors.New("process exited")
	}
	now := time.Now()
	a.set(func(st *ArchiverStatus) { st.Error, st.ErrorAt = err.Error(), &now })
	a.lg.Printf("ERROR: %v", err)
	a.mu.Lock()
	a.fails++
	notify := a.fails >= 3 && !a.notified
	a.notified = a.notified || notify
	a.mu.Unlock()
	if notify {
		job, jerr := a.r.Store.GetJob(a.jobID)
		if jerr == nil && job.NotifyFailure && a.r.Notifier != nil {
			title := fmt.Sprintf("%s archiving FAILED: %s", strings.ToUpper(a.info.Name[:1])+a.info.Name[1:], job.Name)
			ev := notifyEvent("binlog", store.StatusFailed, title, err.Error())
			ev.URL = a.r.link("/#/binlogs")
			ev.Add("Server", job.ServerName)
			a.r.Notifier.Send(ev)
		}
	}
}

// ---- upload ----

func (a *archiver) uploader(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.uploadPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

func (a *archiver) uploadPending(ctx context.Context) {
	closed := a.stream.Closed()
	pendingBytes, since := a.pendingInfo(closed)
	a.set(func(st *ArchiverStatus) {
		st.Pending, st.PendingBytes, st.PendingSince = len(closed), pendingBytes, since
	})
	if len(closed) == 0 {
		return
	}
	job, err := a.r.Store.GetJob(a.jobID)
	if err != nil {
		return
	}
	for _, name := range closed {
		if ctx.Err() != nil {
			return
		}
		if t, ok := a.nextTry[name]; ok && time.Now().Before(t) {
			continue
		}
		if err := a.archiveFile(ctx, job, name); err != nil {
			if ctx.Err() != nil {
				return
			}
			a.attempts[name]++
			wait := min(time.Duration(a.attempts[name])*time.Minute, 30*time.Minute)
			a.nextTry[name] = time.Now().Add(wait)
			msg := fmt.Sprintf("%s: %v", name, err)
			a.set(func(st *ArchiverStatus) { st.UploadError = msg })
			a.lg.Printf("Archiving %s FAILED (retrying in %s): %v", name, humanDuration(wait), err)
			continue
		}
		delete(a.nextTry, name)
		delete(a.attempts, name)
	}
	left := a.stream.Closed()
	leftBytes, since := a.pendingInfo(left)
	a.set(func(st *ArchiverStatus) {
		st.Pending, st.PendingBytes, st.PendingSince = len(left), leftBytes, since
		if len(left) == 0 {
			st.UploadError = ""
		}
	})
}

// pendingInfo returns the size of the staged closed files and when the
// oldest of them was closed (its last write).
func (a *archiver) pendingInfo(files []string) (int64, *time.Time) {
	var n int64
	var oldest *time.Time
	for _, f := range files {
		st, err := os.Stat(filepath.Join(a.dir, f))
		if err != nil {
			continue
		}
		n += st.Size()
		if t := st.ModTime(); oldest == nil || t.Before(*oldest) {
			oldest = &t
		}
	}
	return n, oldest
}

type logTarget struct {
	st      *store.Storage
	backend storage.Backend
}

func (a *archiver) targets(job *store.Job) ([]logTarget, error) {
	var out []logTarget
	for _, t := range job.Targets {
		st, err := a.r.Store.GetStorage(t.StorageID)
		if err != nil {
			continue
		}
		b, err := a.r.Storage.Get(st)
		if err != nil {
			return nil, fmt.Errorf("storage %s: %w", st.Name, err)
		}
		out = append(out, logTarget{st: st, backend: b})
	}
	if len(out) == 0 {
		return nil, errors.New("the job has no storage targets")
	}
	return out, nil
}

// archiveFile uploads one closed file to every storage of the job; the
// stream drops its staged copy once all of them hold it.
func (a *archiver) archiveFile(ctx context.Context, job *store.Job, name string) error {
	inf, err := a.stream.Seal(ctx, job, name)
	if err != nil {
		return err
	}
	stored := filepath.Join(a.dir, ".enc", name+".stored")
	if inf.Skip {
		os.Remove(stored)
		a.stream.Archived(name)
		return nil
	}
	targets, err := a.targets(job)
	if err != nil {
		return err
	}
	raw := filepath.Join(a.dir, name)
	row, err := a.r.Store.FindLogFile(job.UUID, name, inf.First)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	op := &storage.Op{Log: a.lg, BwLimit: job.Options.BwLimit}
	if row == nil {
		comp := pipeline.NormalizeCompression(job.Compression)
		pass := ""
		if job.Encrypt {
			if job.Passphrase == "" {
				return errNoPassphrase // never archive in plaintext what should be encrypted
			}
			pass = job.Passphrase
		}
		size, sum, rawSum, err := encodeLogFile(raw, stored, inf.Size, pipeline.ChainOptions{Compression: comp, Level: job.CompressionLevel, Threads: 2, Passphrase: pass})
		if err != nil {
			os.Remove(stored)
			return err
		}
		rel := a.storagePath(job, name, inf.First, comp, pass != "")
		jid, sid := job.ID, job.ServerID
		row = &store.LogFile{JobID: &jid, JobUUID: job.UUID, JobName: job.Name, ServerID: &sid, ServerName: job.ServerName,
			FileName: name, Seq: inf.Seq, FirstTS: inf.First, LastTS: inf.Last, SizeBytes: inf.Size, StoredBytes: size,
			SHA256: sum, RawSHA256: rawSum, Compression: comp, Encrypted: pass != "", Passphrase: pass, Path: rel, Meta: inf.Meta}
		var errs []string
		for _, t := range targets {
			sid := t.st.ID
			loc := store.LogLocation{StorageID: &sid, StorageName: t.st.Name, StorageType: t.st.Type, Status: store.LocOK}
			if err := putFile(ctx, t.backend, stored, rel, size, op); err != nil {
				loc.Status, loc.Error = store.LocFailed, err.Error()
				errs = append(errs, t.st.Name+": "+err.Error())
			}
			row.Locations = append(row.Locations, loc)
		}
		if len(errs) == len(targets) {
			os.Remove(stored) // nothing refers to these bytes yet: re-encode next time
			return errors.New(strings.Join(errs, "; "))
		}
		if err := a.r.Store.AddLogFile(row); err != nil {
			return err
		}
		if len(errs) > 0 {
			return errors.New(strings.Join(errs, "; ")) // retried later from the stored copy
		}
	}
	// storages still missing the file (earlier failures, targets added later)
	var errs []string
	for _, t := range targets {
		if row.OKOn(t.st.ID) != nil {
			continue
		}
		if err := a.ensureStored(ctx, row, stored, targets); err != nil {
			return err
		}
		var loc *store.LogLocation
		for i := range row.Locations {
			if l := &row.Locations[i]; l.StorageID != nil && *l.StorageID == t.st.ID {
				loc = l
			}
		}
		if loc == nil {
			sid := t.st.ID
			row.Locations = append(row.Locations, store.LogLocation{FileID: row.ID, StorageID: &sid, StorageName: t.st.Name, StorageType: t.st.Type})
			loc = &row.Locations[len(row.Locations)-1]
		}
		loc.Status, loc.Error = store.LocOK, ""
		if err := putFile(ctx, t.backend, stored, row.Path, row.StoredBytes, op); err != nil {
			loc.Status, loc.Error = store.LocFailed, err.Error()
			errs = append(errs, t.st.Name+": "+err.Error())
		}
		_ = a.r.Store.SaveLogLocation(loc)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	os.Remove(stored)
	a.stream.Archived(name)
	now := time.Now()
	lt := inf.Last
	a.set(func(st *ArchiverStatus) {
		st.LastArchived, st.ArchivedAt = name, &now
		if !lt.IsZero() && (st.ArchivedUntil == nil || lt.After(*st.ArchivedUntil)) {
			st.ArchivedUntil = &lt
		}
	})
	loc := a.r.location()
	span := "no timed events"
	if !inf.First.IsZero() {
		span = inf.First.In(loc).Format("15:04:05") + " → " + inf.Last.In(loc).Format("2006-01-02 15:04:05")
	}
	a.lg.Printf("Archived %s (%s, %s → %s) to %d storage(s)", name, span, humanBytes(inf.Size), humanBytes(row.StoredBytes), len(targets))
	return nil
}

// storagePath is <job>/<prefix>/<day>/<file><ext>; a file of an older
// numbering with the same name gets its first event's time appended.
func (a *archiver) storagePath(job *store.Job, name string, first time.Time, comp string, enc bool) string {
	day := first
	if day.IsZero() {
		day = time.Now()
	}
	dir := JobDir(job.Name, job.ID) + "/" + a.info.Prefix + "/" + day.In(a.r.location()).Format("20060102")
	ext := pipeline.Ext(comp, enc)
	rel := dir + "/" + name + ext
	if a.r.Store.LogPathUsed(rel) {
		rel = fmt.Sprintf("%s/%s-%d%s", dir, name, day.Unix(), ext)
	}
	return rel
}

// ensureStored makes sure the encoded copy recorded in row exists locally,
// fetching it back from a storage that has it when needed.
func (a *archiver) ensureStored(ctx context.Context, row *store.LogFile, stored string, targets []logTarget) error {
	if sum, err := fileSHA256(stored); err == nil && sum == row.SHA256 {
		return nil
	}
	for _, t := range targets {
		if row.OKOn(t.st.ID) == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(stored), 0o700); err != nil {
			return err
		}
		if err := t.backend.Download(ctx, row.Path, stored, nil); err != nil {
			continue
		}
		if sum, err := fileSHA256(stored); err == nil && sum == row.SHA256 {
			return nil
		}
	}
	return errors.New("the local encoded copy is gone and could not be fetched from another storage")
}

// encodeLogFile compresses/encrypts the first size bytes of src into dst
// like a backup file.
func encodeLogFile(src, dst string, size int64, o pipeline.ChainOptions) (stored int64, sum, rawSum string, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, "", "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, "", "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", "", err
	}
	defer out.Close()
	ch, err := pipeline.NewWriteChain(out, o)
	if err != nil {
		return 0, "", "", err
	}
	rh := sha256.New()
	n, err := io.Copy(ch, io.TeeReader(io.LimitReader(in, size), rh))
	if err == nil && n != size {
		err = fmt.Errorf("%s: read %d of %d bytes", filepath.Base(src), n, size)
	}
	if err != nil {
		ch.Close()
		return 0, "", "", err
	}
	if err := ch.Close(); err != nil {
		return 0, "", "", err
	}
	if err := out.Sync(); err != nil {
		return 0, "", "", err
	}
	return ch.OutBytes(), ch.SHA256(), hex.EncodeToString(rh.Sum(nil)), nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// putFile uploads a local file and checks its size on the storage.
func putFile(ctx context.Context, b storage.Backend, local, rel string, size int64, op *storage.Op) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := b.PutStream(ctx, rel, f, op); err != nil {
		return err
	}
	entries, err := b.List(ctx, path.Dir(rel))
	if err != nil {
		return fmt.Errorf("post-upload check: %w", err)
	}
	for _, e := range entries {
		if e.Name == path.Base(rel) {
			if e.Size >= 0 && e.Size != size {
				return fmt.Errorf("wrong size after upload (%d != %d)", e.Size, size)
			}
			return nil
		}
	}
	return errors.New("file not found on the storage after upload")
}

// ---- deletion ----

// DeleteJobLogs removes every archived log file of a job from all storages.
func (r *Runner) DeleteJobLogs(ctx context.Context, jobUUID string) (int, error) {
	files, err := r.Store.ListLogFiles(jobUUID)
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, f := range files {
		for _, l := range f.Locations {
			if l.StorageID != nil && l.Status == store.LocOK {
				st, err := r.Store.GetStorage(*l.StorageID)
				if err == nil {
					b, err := r.Storage.Get(st)
					if err == nil {
						err = b.DeleteFile(ctx, f.Path)
					}
					if err != nil {
						errs = append(errs, fmt.Errorf("%s %s: %w", l.StorageName, f.FileName, err))
						continue
					}
				}
			}
			_ = r.Store.DeleteLogLocation(l.ID)
		}
		r.Store.DeleteLogIfOrphan(f.ID)
		n++
	}
	return n, errors.Join(errs...)
}
