package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Env is the execution environment of one backup/restore task.
type Env struct {
	Ctx         context.Context
	Log         *tasklog.Logger
	Tools       *Tools
	TmpDir      string // private scratch dir for this task (removed afterwards)
	LowPriority bool
}

type FileOpts struct {
	Kind     string
	Database string
	Compress bool
}

// Sink receives backup output files. Implementations add compression,
// encryption and hashing and must be safe for concurrent Create calls.
type Sink interface {
	Create(name string, o FileOpts) (io.WriteCloser, error)
}

// Aborter is implemented by sink writers that can discard a partial file
// (e.g. a streaming upload that must never be committed truncated).
type Aborter interface {
	Abort(err error)
}

// Finish closes w on success or aborts it when the producer failed.
func Finish(w io.WriteCloser, runErr error) error { return finish(w, runErr) }

func finish(w io.WriteCloser, runErr error) error {
	if runErr != nil {
		if a, ok := w.(Aborter); ok {
			a.Abort(runErr)
		} else {
			w.Close()
		}
		return runErr
	}
	return w.Close()
}

type BackupSpec struct {
	Server         *store.Server
	Flavor         string
	ServerVersion  string
	Info           *dbconn.ServerInfo // the server, inspected just before
	Job            *store.Job
	Databases      []dbconn.DBInfo
	IncrementalLSN string
	// Remote runs a physical backup on the database host (servers reached
	// through an agent started with --allow-physical).
	Remote RemotePhysical
}

type Result struct {
	Meta      map[string]any
	Databases []store.DBEntry
	Tables    store.TableStats // rows per table, counted from the dump (nil: unknown)
}

// SetRows records the counted tables and the rows of each database (only for
// databases where the dump held something the counter understood).
func (res *Result) SetRows(t store.TableStats) { res.setRows(t) }

func (res *Result) setRows(t store.TableStats) {
	if t == nil {
		return
	}
	res.Tables = store.TableStats{}
	for i := range res.Databases {
		d := &res.Databases[i]
		ts, ok := t[d.Name]
		if !ok && d.Tables > 0 {
			continue // nothing recognized: leave this database uncounted
		}
		if ts == nil {
			ts = map[string]int64{}
		}
		res.Tables[d.Name] = ts
		d.Rows = 0
		for _, n := range ts {
			d.Rows += n
		}
	}
}

// Command prepares a tool run: logged, low priority when asked, bound to the task.
func (e *Env) Command(name string, args []string, extraEnv []string) *exec.Cmd {
	return e.command(name, args, extraEnv)
}

// RunCmd runs cmd with stderr logged under prefix and returns a useful error.
func (e *Env) RunCmd(cmd *exec.Cmd, prefix string) error { return e.runCmd(cmd, prefix) }

func (e *Env) command(name string, args []string, extraEnv []string) *exec.Cmd {
	e.Log.Printf("$ %s", execx.Quote(name, args))
	if e.LowPriority {
		name, args = execx.LowPriority(name, args)
	}
	cmd := execx.Command(e.Ctx, name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	return cmd
}

// tail keeps the last bytes written (for completion markers / errors).
type tail struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func newTail(n int) *tail { return &tail{max: n} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// head keeps the first bytes written. With mark set it also records when
// that text first appeared in the output.
type head struct {
	mu       sync.Mutex
	b        []byte
	max      int
	mark     []byte
	markedAt time.Time
}

func (h *head) Write(p []byte) (int, error) {
	h.mu.Lock()
	if room := h.max - len(h.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		h.b = append(h.b, p[:room]...)
		if h.mark != nil && h.markedAt.IsZero() && bytes.Contains(h.b, h.mark) {
			h.markedAt = time.Now()
		}
	}
	h.mu.Unlock()
	return len(p), nil
}

func (h *head) MarkedAt() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.markedAt
}

func (h *head) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.b)
}

// runCmd runs cmd with stderr logged under prefix; returns a useful error.
func (e *Env) runCmd(cmd *exec.Cmd, prefix string) error {
	lw := e.Log.Writer(prefix)
	errTail := newTail(2048)
	if cmd.Stderr == nil {
		cmd.Stderr = io.MultiWriter(lw, errTail)
	} else {
		cmd.Stderr = io.MultiWriter(cmd.Stderr, lw, errTail)
	}
	if cmd.Stdout == nil {
		cmd.Stdout = lw
	}
	err := cmd.Run()
	lw.Close()
	if err != nil {
		if e.Ctx.Err() != nil {
			return e.Ctx.Err()
		}
		msg := lastLines(errTail.String(), 4)
		if msg != "" {
			return fmt.Errorf("%s: %v: %s", strings.Trim(prefix, "[] "), err, msg)
		}
		return fmt.Errorf("%s: %w", strings.Trim(prefix, "[] "), err)
	}
	return nil
}

func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.\-]`)

// SafeFileName converts a database name to a portable file name.
func SafeFileName(s string) string {
	out := unsafeName.ReplaceAllStringFunc(s, func(m string) string {
		var b strings.Builder
		for _, c := range []byte(m) {
			fmt.Fprintf(&b, "@%02x", c)
		}
		return b.String()
	})
	if out == "" || strings.HasPrefix(out, ".") {
		out = "_" + out
	}
	return out
}

// tablesFor returns the excluded tables belonging to db.
func tablesFor(excl []string, db string) []string {
	var out []string
	for _, e := range excl {
		e = strings.TrimSpace(e)
		d, t, ok := strings.Cut(e, ".")
		if ok && d == db && t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Parallel runs fn for each item with up to n workers, stopping at first error.
func Parallel[T any](ctx context.Context, n int, items []T, fn func(ctx context.Context, it T) error) error {
	if n < 1 {
		n = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(it T) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, it); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}(it)
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// ToEntries turns inspected databases into catalog entries.
func ToEntries(dbs []dbconn.DBInfo) []store.DBEntry { return toEntries(dbs) }

func toEntries(dbs []dbconn.DBInfo) []store.DBEntry {
	out := make([]store.DBEntry, 0, len(dbs))
	for _, d := range dbs {
		out = append(out, store.DBEntry{Name: d.Name, Charset: d.Charset, Collation: d.Collation, SizeBytes: d.SizeBytes, Tables: d.Tables})
	}
	return out
}

var ErrToolMissing = errors.New("missing tool")

// RequireTool fails with an install hint when t was not found.
func RequireTool(t *Tool) error { return requireTool(t) }

func requireTool(t *Tool) error {
	if t == nil || !t.Found {
		name := ""
		hint := ""
		if t != nil {
			name, hint = t.Name, t.Hint
		}
		return fmt.Errorf("%w %s on the server (install: %s)", ErrToolMissing, name, hint)
	}
	return nil
}

// grepFirst returns the first submatch of re in s.
func grepFirst(re *regexp.Regexp, s string) []string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	return m
}
