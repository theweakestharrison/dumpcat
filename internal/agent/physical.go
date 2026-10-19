package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
)

// Physical backups through an agent: the server sends a "physical" message
// with the backup to run, the agent checks it, writes the credentials into a
// private option file and opens a data connection (PathStream) like for a
// tunneled database connection. That connection carries binary WebSocket
// messages whose first byte says what follows:
const (
	frameStdout = 'o' // agent → server: xbstream bytes
	frameStderr = 'e' // agent → server: the tool's log
	frameResult = 'r' // agent → server: PhysicalResult (JSON), the last frame
	framePing   = 'p' // both ways every pingEvery: the other side is alive
	frameCancel = 'c' // server → agent: kill the tool (closing does too)

	maxFrame = 256 << 10
)

// Caps is what an agent can do besides forwarding connections. Agents from
// before physical backups send none: they only forward connections.
type Caps struct {
	Physical *PhysicalCaps `json:"physical,omitempty"`
}

// PhysicalCaps describes physical backups on the agent's host.
type PhysicalCaps struct {
	Allowed bool      `json:"allowed"`         // started with --allow-physical
	Tools   []ToolCap `json:"tools,omitempty"` // mariabackup, xtrabackup
	User    string    `json:"user,omitempty"`  // the OS user the tool runs as
	// Datadir is the data directory checked when the agent connected
	// (--datadir, else the usual location); DatadirError says why the
	// agent cannot read it.
	Datadir      string `json:"datadir,omitempty"`
	DatadirError string `json:"datadir_error,omitempty"`
}

// ToolCap is a backup tool on the agent's host.
type ToolCap struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Version string `json:"version,omitempty"`
	Hint    string `json:"hint,omitempty"` // how to install it
}

// describe sums the capabilities up for the agent's log.
func (p *PhysicalCaps) describe() string {
	if !p.Allowed {
		return "physical backups: off, see --allow-physical"
	}
	var found []string
	for _, t := range p.Tools {
		if t.Found {
			found = append(found, t.Version)
		}
	}
	s := "physical backups: no mariabackup/xtrabackup found"
	if len(found) > 0 {
		s = "physical backups: " + strings.Join(found, ", ")
	}
	if p.User != "" {
		s += ", as " + p.User
	}
	switch {
	case p.DatadirError != "":
		s += "; " + p.DatadirError
	case p.Datadir != "":
		s += ", datadir " + p.Datadir + " readable"
	}
	return s
}

func (p *PhysicalCaps) Tool(name string) *ToolCap {
	for i := range p.Tools {
		if p.Tools[i].Name == name {
			return &p.Tools[i]
		}
	}
	return nil
}

// PhysicalProblem tells why the agent cannot run a physical backup with tool
// now ("" when it can).
func (in Info) PhysicalProblem(tool string) string {
	if !in.Online {
		return "the agent is offline"
	}
	if in.Caps == nil || in.Caps.Physical == nil {
		return fmt.Sprintf("the agent runs DumpCat %s, which cannot run physical backups: update it", in.Version)
	}
	p := in.Caps.Physical
	if !p.Allowed {
		return "the agent does not allow physical backups: start it with --allow-physical"
	}
	if t := p.Tool(tool); t == nil || !t.Found {
		msg := tool + " is not installed on " + in.Hostname
		if t != nil && t.Hint != "" {
			msg += " (install: " + t.Hint + ")"
		}
		return msg
	}
	return ""
}

// DatadirProblem tells why the agent could not read datadir when it
// connected ("" when it could, or did not check that directory; the agent
// checks again before each backup).
func (in Info) DatadirProblem(datadir string) string {
	if in.Caps == nil || in.Caps.Physical == nil || !SameDir(in.Caps.Physical.Datadir, datadir) {
		return ""
	}
	return in.Caps.Physical.DatadirError
}

// SameDir compares paths reported by different machines (trailing
// separators; Windows paths ignore case).
func SameDir(a, b string) bool {
	norm := func(p string) string {
		p = strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/")
		if len(p) >= 2 && p[1] == ':' {
			p = strings.ToLower(p)
		}
		return p
	}
	return a != "" && norm(a) == norm(b)
}

// PhysicalRequest asks the agent to back up the database at Net/Addr (as
// seen from the agent; it must be on the allow list). The credentials only
// live in this message and in a private option file the agent removes when
// the tool exits.
type PhysicalRequest struct {
	engine.PhysicalOptions
	Net      string `json:"net"`
	Addr     string `json:"addr"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

// PhysicalResult ends the data connection of a physical backup.
type PhysicalResult struct {
	Error string            `json:"error,omitempty"` // the tool failed
	Tool  string            `json:"tool,omitempty"`  // its version line
	Files map[string]string `json:"files,omitempty"` // engine.PhysicalCheckpointFiles
	Bytes int64             `json:"bytes"`           // xbstream bytes sent
}

func writeFrame(ws *websocket.Conn, kind byte, data []byte) error {
	b := make([]byte, 1+len(data))
	b[0] = kind
	copy(b[1:], data)
	return websocket.Message.Send(ws, b)
}

func readFrame(ws *websocket.Conn) (byte, []byte, error) {
	var b []byte
	if err := websocket.Message.Receive(ws, &b); err != nil {
		return 0, nil, err
	}
	if len(b) == 0 {
		return 0, nil, errors.New("empty frame")
	}
	return b[0], b[1:], nil
}

// frameWriter sends what is written as frames of one kind.
type frameWriter struct {
	ws   *websocket.Conn
	kind byte
	n    *atomic.Int64
}

func (w frameWriter) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		chunk := p[done:min(len(p), done+maxFrame)]
		if err := writeFrame(w.ws, w.kind, chunk); err != nil {
			return done, err
		}
		done += len(chunk)
		if w.n != nil {
			w.n.Add(int64(len(chunk)))
		}
	}
	return done, nil
}

// ---- server side ----

// Physical runs a physical backup on the agent's host: the xbstream goes to
// stdout and the tool's log to stderr as they arrive. Cancelling ctx kills
// the tool on the agent.
func (m *Manager) Physical(ctx context.Context, agentID int64, req *PhysicalRequest, stdout, stderr io.Writer) (*PhysicalResult, error) {
	if st := m.Status(agentID); !st.Online {
		return nil, ErrOffline
	} else if p := st.PhysicalProblem(req.Tool); p != "" {
		return nil, errors.New(p)
	}
	c, err := m.request(agentID, Message{Type: "physical", Physical: req})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.streams[agentID]++
	m.active[c] = agentID
	m.mu.Unlock()
	defer func() {
		c.Close()
		m.mu.Lock()
		m.streams[agentID]--
		delete(m.active, c)
		m.mu.Unlock()
	}()
	ws := c.Conn
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = writeFrame(ws, frameCancel, nil)
				c.Close()
				return
			case <-t.C:
				_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if writeFrame(ws, framePing, nil) != nil {
					c.Close()
					return
				}
			}
		}
	}()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(idleTimeout))
		kind, data, err := readFrame(ws)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("the connection to the agent was lost during the backup: %w", err)
		}
		switch kind {
		case frameStdout:
			if _, err := stdout.Write(data); err != nil {
				return nil, err // closing the connection stops the tool
			}
		case frameStderr:
			_, _ = stderr.Write(data)
		case frameResult:
			var res PhysicalResult
			if err := json.Unmarshal(data, &res); err != nil {
				return nil, fmt.Errorf("invalid result from the agent: %w", err)
			}
			return &res, nil
		}
	}
}

// ---- agent side ----

func (c *Client) tools() *engine.Tools {
	if c.Tools == nil {
		c.Tools = engine.NewTools(nil)
	}
	return c.Tools
}

// caps probes this host for what the agent can do (sent with hello).
func (c *Client) caps() *Caps {
	p := &PhysicalCaps{Allowed: c.AllowPhysical}
	if !p.Allowed {
		return &Caps{Physical: p}
	}
	if u, err := user.Current(); err == nil {
		p.User = u.Username
	}
	c.tools().Refresh() // a tool installed since the last connection
	for _, name := range []string{"mariabackup", "xtrabackup"} {
		t := c.tools().Get(name)
		p.Tools = append(p.Tools, ToolCap{Name: name, Found: t.Found, Version: t.Version, Hint: t.Hint})
	}
	p.Datadir = c.Datadir
	if p.Datadir == "" {
		p.Datadir = defaultDatadir()
	}
	if p.Datadir != "" {
		if err := checkDatadir(p.Datadir); err != nil {
			p.DatadirError = err.Error()
		}
	}
	return &Caps{Physical: p}
}

// defaultDatadir is the data directory of a usual MySQL/MariaDB install on
// this OS ("" when there is none).
func defaultDatadir() string {
	cands := []string{"/var/lib/mysql", "/var/lib/mariadb", "/usr/local/mysql/data"}
	if runtime.GOOS == "windows" {
		cands = nil
		var patterns []string
		if d := os.Getenv("ProgramData"); d != "" {
			patterns = append(patterns, filepath.Join(d, "MySQL", "MySQL Server *", "Data"))
		}
		if d := os.Getenv("ProgramFiles"); d != "" {
			patterns = append(patterns, filepath.Join(d, "MariaDB *", "data"))
		}
		for _, p := range patterns {
			m, _ := filepath.Glob(p)
			sort.Sort(sort.Reverse(sort.StringSlice(m))) // newest version first
			cands = append(cands, m...)
		}
	}
	for _, d := range cands {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return ""
}

// checkDatadir tells whether this process, and the tool it starts, can read
// the datadir: the system schema and the InnoDB system tablespace are
// private to the database's own OS user.
func checkDatadir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("the datadir %s is not an absolute path", dir)
	}
	fail := func(err error) error {
		if os.IsPermission(err) {
			return fmt.Errorf("cannot read the datadir (%v): run the agent as the user that owns it, e.g. mysql (install.sh --agent --allow-physical does)", err)
		}
		return fmt.Errorf("cannot read the datadir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fail(err)
	}
	if !st.IsDir() {
		return fmt.Errorf("the datadir %s is not a directory", dir)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fail(err)
	}
	if _, err := os.ReadDir(filepath.Join(dir, "mysql")); err != nil && !os.IsNotExist(err) {
		return fail(err)
	}
	if f, err := os.Open(filepath.Join(dir, "ibdata1")); err == nil {
		f.Close()
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	return nil
}

// unsafeArgWords mark options that name files, directories, programs or
// other servers, or change how the remaining options are read. The agent
// never passes them on, so a DumpCat server cannot use the backup tool to
// read or run anything else on the database host.
var unsafeArgWords = []string{"dir", "file", "path", "plugin", "defaults", "login", "socket", "host", "port", "user",
	"password", "protocol", "stream", "target", "innobackupex", "keyring", "key", "exec", "rsync", "prepare",
	"copy-back", "move-back", "decrypt", "decompress", "remove", "incremental", "log-bin"}

// SafeExtraArg checks an extra argument of a physical backup job before the
// agent passes it to the tool: only --name[=value] options that point at
// nothing on the database host.
func SafeExtraArg(a string) error {
	name, value, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
	name = strings.ToLower(name)
	ok := strings.HasPrefix(a, "--") && name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-_") == "" &&
		!strings.ContainsAny(value, `/\`) && !strings.Contains(value, "..")
	for _, w := range unsafeArgWords {
		ok = ok && !strings.Contains(name, w)
	}
	if !ok {
		return fmt.Errorf("the agent does not pass %q to the backup tool: extra arguments must be --name[=value] options that name no file, directory, program or server", a)
	}
	return nil
}

// checkPhysical validates a request before anything runs.
func (c *Client) checkPhysical(req *PhysicalRequest) (*engine.Tool, error) {
	if req.Tool != "mariabackup" && req.Tool != "xtrabackup" {
		return nil, fmt.Errorf("unknown backup tool %q", req.Tool)
	}
	tool := c.tools().Get(req.Tool)
	if !tool.Found {
		return nil, fmt.Errorf("%s is not installed on this host (install: %s)", req.Tool, tool.Hint)
	}
	if req.Datadir != "" {
		if err := checkDatadir(req.Datadir); err != nil {
			return nil, err
		}
	}
	if strings.Trim(req.IncrementalLSN, "0123456789") != "" {
		return nil, fmt.Errorf("invalid LSN %q", req.IncrementalLSN)
	}
	if req.Parallel < 1 || req.Parallel > 256 || req.FTWRLWaitTimeout < 0 {
		return nil, errors.New("invalid backup options")
	}
	for _, a := range req.ExtraArgs {
		if err := SafeExtraArg(a); err != nil {
			return nil, err
		}
	}
	return tool, nil
}

// physical runs the backup the server asked for and streams it back.
func (c *Client) physical(ctx context.Context, m Message, send func(Message) error) {
	req := m.Physical
	refuse := func(err error) {
		c.logf("physical backup refused: %v", err)
		_ = send(Message{Type: "error", ID: m.ID, Error: err.Error()})
	}
	if !c.AllowPhysical {
		refuse(errors.New("this agent does not allow physical backups: start it with --allow-physical"))
		return
	}
	if req == nil {
		refuse(errors.New("empty request"))
		return
	}
	t, err := NormalizeTarget(Target{Net: req.Net, Addr: req.Addr})
	if err != nil {
		refuse(err)
		return
	}
	if !c.Allow.Allows(t) {
		refuse(fmt.Errorf("%s is not on this agent's allow list (start the agent with -allow %s)", t, t))
		return
	}
	tool, err := c.checkPhysical(req)
	if err != nil {
		refuse(err)
		return
	}
	dir, err := os.MkdirTemp("", "dumpcat-physical-")
	if err != nil {
		refuse(err)
		return
	}
	defer os.RemoveAll(dir) // the option file with the password, the checkpoints
	sv := &store.Server{Name: t.String(), Username: req.User, Password: req.Password}
	if t.Net == "unix" {
		sv.Host, sv.Socket = "localhost", t.Addr
	} else {
		host, port, _ := net.SplitHostPort(t.Addr)
		sv.Host = host
		sv.Port, _ = strconv.Atoi(port)
	}
	cnf, extraEnv, err := engine.WriteCnf(dir, sv, engine.CnfOptions{Groups: req.ToolGroups()})
	lsnDir, tmpTarget := filepath.Join(dir, "lsn"), filepath.Join(dir, "xbtmp")
	for _, d := range []string{lsnDir, tmpTarget} {
		if err == nil {
			err = os.Mkdir(d, 0o700)
		}
	}
	if err != nil {
		refuse(err)
		return
	}
	cfg, err := c.config(PathStream, "id="+url.QueryEscape(m.ID))
	if err != nil {
		refuse(err)
		return
	}
	ws, err := dial(cfg)
	if err != nil {
		refuse(err)
		return
	}
	defer ws.Close()
	ws.PayloadType = websocket.BinaryFrame

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverGone := make(chan struct{})
	go func() {
		// the server pings every pingEvery and cancels with a frame or by closing
		defer close(serverGone)
		defer cancel()
		for {
			_ = ws.SetReadDeadline(time.Now().Add(idleTimeout))
			if kind, _, err := readFrame(ws); err != nil || kind == frameCancel {
				return
			}
		}
	}()
	go func() {
		// keep the server's read deadline away while the tool prints nothing
		// (waiting for a lock, copying a large file)
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if writeFrame(ws, framePing, nil) != nil {
					cancel()
					return
				}
			}
		}
	}()

	name, args := tool.Path, append([]string{"--defaults-extra-file=" + cnf}, req.Args(tmpTarget, lsnDir)...)
	if req.LowPriority {
		name, args = execx.LowPriority(name, args)
	}
	cmd := execx.Command(ctx, name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Dir = dir
	var sent atomic.Int64
	out := bufio.NewWriterSize(frameWriter{ws: ws, kind: frameStdout, n: &sent}, maxFrame)
	cmd.Stdout = out
	cmd.Stderr = frameWriter{ws: ws, kind: frameStderr}
	c.logf("physical backup of %s started (%s)", t, tool.Version)
	start := time.Now()
	runErr := cmd.Run()
	if runErr == nil {
		runErr = out.Flush()
	}
	took := time.Since(start).Round(time.Second)
	if ctx.Err() != nil {
		c.logf("physical backup of %s canceled after %s: the tool was stopped", t, took)
		return
	}
	res := PhysicalResult{Tool: tool.Version, Files: engine.ReadCheckpoints(lsnDir), Bytes: sent.Load()}
	if runErr != nil {
		res.Error = runErr.Error()
	}
	data, _ := json.Marshal(res)
	if err := writeFrame(ws, frameResult, data); err != nil {
		c.logf("physical backup of %s: could not send the result: %v", t, err)
		return
	}
	if runErr != nil {
		c.logf("physical backup of %s failed after %s: %v", t, took, runErr)
	} else {
		c.logf("physical backup of %s finished in %s (%d MiB sent)", t, took, res.Bytes>>20)
	}
	// let the server close first: closing with its unread frames pending
	// could reset the connection before it has read the result
	select {
	case <-serverGone:
	case <-time.After(10 * time.Second):
	}
}
