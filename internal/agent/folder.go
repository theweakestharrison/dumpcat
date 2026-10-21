package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"dumpcat/internal/engine"
	"dumpcat/internal/sysinfo"
)

// SQL Server writes its backups on its own host (BACKUP DATABASE … TO DISK)
// and reads restores there, so DumpCat needs a folder it shares with SQL
// Server. An agent started with --allow-mssql-dir DIR is that folder's
// other end: the server sends a "file" message (read, write, remove or stat
// a file of DumpCat's own in DIR) and the bytes travel over a data
// connection (PathStream) in the frames of physical backups:
//
//	read:  agent → server 'i' (FileResult: the size), 'o'…, 'r' (FileResult)
//	write: server → agent 'o'…, 'f' (FileResult: the size); agent → server 'r'
//	stat, remove: agent → server 'r'
//
// Pings ('p') go both ways while a file is on its way; the server cancels
// with 'c' or by closing. The agent only reaches DIR itself, only names
// starting with "dumpcat-" and only regular files: a DumpCat server cannot
// use it to read or write anything else on the host.
const (
	frameInfo = 'i' // agent → server (read): FileResult with the file's size, before its bytes
	frameEnd  = 'f' // server → agent (write): the file is complete, FileResult with its size
)

// FolderCaps describes SQL Server's backup folder on the agent's host.
type FolderCaps struct {
	Dir   string `json:"dir,omitempty"`   // --allow-mssql-dir ("": not allowed)
	User  string `json:"user,omitempty"`  // the OS user the agent runs as
	Error string `json:"error,omitempty"` // why the agent could not use Dir when it connected
}

// describe sums the folder up for the agent's log.
func (f *FolderCaps) describe() string {
	switch {
	case f.Dir == "":
		return "SQL Server backup folder: off, see --allow-mssql-dir"
	case f.Error != "":
		return "SQL Server backup folder: " + f.Error
	}
	s := "SQL Server backup folder " + f.Dir
	if f.User != "" {
		s += ", as " + f.User
	}
	return s
}

// FolderProblem tells why DumpCat cannot reach SQL Server's backup folder
// through the agent ("" when it can). dir is the folder SQL Server writes
// to, for the hint. A folder the agent could not open when it connected is
// no problem here: the agent checks it again for every file.
func (in Info) FolderProblem(dir string) string {
	if !in.Online {
		return "the agent is offline"
	}
	if in.Caps == nil || in.Caps.MSSQL == nil {
		return fmt.Sprintf("the agent runs DumpCat %s, which cannot reach SQL Server's backup folder: update it", in.Version)
	}
	if in.Caps.MSSQL.Dir == "" {
		if dir == "" {
			dir = "DIR"
		}
		return "the agent does not give DumpCat SQL Server's backup folder: start it with --allow-mssql-dir " + dir
	}
	return ""
}

// FileRequest asks the agent for a file of its folder (--allow-mssql-dir).
type FileRequest struct {
	Op   string `json:"op"`             // read, write, remove, stat
	Name string `json:"name,omitempty"` // a file name in the folder: dumpcat-…
}

// FileResult answers a FileRequest.
type FileResult struct {
	Error string `json:"error,omitempty"`
	Size  int64  `json:"size"`           // read: the file's size; write: the bytes written
	Free  int64  `json:"free,omitempty"` // stat: free bytes in the folder (-1: unknown)
}

// reFileName is a file of DumpCat's own: the names of internal/mssql
// (dumpcat-<random>-<database>.bak) and nothing that leaves the folder.
var reFileName = regexp.MustCompile(`^dumpcat-[A-Za-z0-9_@-][A-Za-z0-9_.@-]{0,200}$`)

// FolderFile returns the path of the file name in dir, refusing any name
// that is not one of DumpCat's own files directly in dir.
func FolderFile(dir, name string) (string, error) {
	if !reFileName.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, ".") {
		return "", fmt.Errorf("the agent only reads and writes files named dumpcat-… in its folder, not %q", name)
	}
	p := filepath.Join(dir, name)
	if filepath.Dir(p) != filepath.Clean(dir) {
		return "", fmt.Errorf("%q is not a file of the folder", name)
	}
	return p, nil
}

// checkFolder tells whether the agent can use dir.
func checkFolder(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("the folder %s is not an absolute path", dir)
	}
	st, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("the folder %s does not exist: create it and give SQL Server's service account access (Linux: chown mssql:mssql)", dir)
	case os.IsPermission(err):
		return fmt.Errorf("the agent may not open %s: run it as the folder's owner (install.sh --allow-mssql-dir does, usually mssql)", dir)
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	}
	f, err := os.Open(dir)
	if err == nil {
		_, err = f.Readdirnames(1)
		f.Close()
		if err == io.EOF {
			err = nil
		}
	}
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("the agent may not read %s: run it as the folder's owner (install.sh --allow-mssql-dir does, usually mssql)", dir)
		}
		return fmt.Errorf("the agent cannot read %s: %v", dir, err)
	}
	return nil
}

// folderCaps probes the folder for hello.
func (c *Client) folderCaps() *FolderCaps {
	f := &FolderCaps{Dir: c.MSSQLDir}
	if f.Dir == "" {
		return f
	}
	if u, err := user.Current(); err == nil {
		f.User = u.Username
	}
	if err := checkFolder(f.Dir); err != nil {
		f.Error = err.Error()
	}
	return f
}

// openFolderFile opens a file SQL Server wrote: a regular file, not a link.
func openFolderFile(p string) (*os.File, int64, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", filepath.Base(p))
	}
	f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || !os.SameFile(fi, st)) {
		err = fmt.Errorf("%s changed while it was opened", filepath.Base(p))
	}
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// removeFolderFile deletes a regular file; one that is gone is no error.
func removeFolderFile(p string) error {
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(p))
	}
	return os.Remove(p)
}

// ---- agent side ----

// file serves a FileRequest.
func (c *Client) file(ctx context.Context, m Message, send func(Message) error) {
	req := m.File
	refuse := func(err error) {
		if req != nil && req.Op != "stat" {
			c.logf("SQL Server backup folder: %s %s refused: %v", req.Op, req.Name, err)
		}
		_ = send(Message{Type: "error", ID: m.ID, Error: err.Error()})
	}
	if c.MSSQLDir == "" {
		refuse(errors.New("this agent does not give DumpCat SQL Server's backup folder: start it with --allow-mssql-dir DIR"))
		return
	}
	if req == nil {
		refuse(errors.New("empty request"))
		return
	}
	dir := c.MSSQLDir
	if req.Op == "stat" {
		if err := checkFolder(dir); err != nil {
			refuse(err)
			return
		}
		free := int64(-1)
		if _, f, err := sysinfo.DiskUsage(dir); err == nil {
			free = f
		}
		c.fileReply(m.ID, FileResult{Free: free})
		return
	}
	p, err := FolderFile(dir, req.Name)
	if err != nil {
		refuse(err)
		return
	}
	switch req.Op {
	case "remove":
		if err := removeFolderFile(p); err != nil {
			refuse(err)
			return
		}
		c.fileReply(m.ID, FileResult{})
	case "read":
		f, size, err := openFolderFile(p)
		if err != nil {
			refuse(err)
			return
		}
		defer f.Close()
		c.sendFile(ctx, m.ID, req.Name, f, size, refuse)
	case "write":
		if err := checkFolder(dir); err != nil {
			refuse(err)
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			refuse(err)
			return
		}
		c.receiveFile(ctx, m.ID, p, f, refuse)
	default:
		refuse(fmt.Errorf("unknown file operation %q", req.Op))
	}
}

// fileStream opens the data connection of a file request.
func (c *Client) fileStream(id string) (*websocket.Conn, error) {
	cfg, err := c.config(PathStream, "id="+url.QueryEscape(id))
	if err != nil {
		return nil, err
	}
	ws, err := dial(cfg)
	if err != nil {
		return nil, err
	}
	ws.PayloadType = websocket.BinaryFrame
	return ws, nil
}

// sendResult sends a FileResult. The agent sets no write deadlines (a
// deadline would cut a long file short): a server that is gone shows as a
// failed read, which closes the connection.
func sendResult(ws *websocket.Conn, kind byte, res FileResult) error {
	data, _ := json.Marshal(res)
	return writeFrame(ws, kind, data)
}

// awaitClose lets the server close first: closing with its frames unread
// could reset the connection before it has read the result.
func awaitClose(ws *websocket.Conn) {
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := readFrame(ws); err != nil {
			return
		}
	}
}

// fileReply answers stat and remove.
func (c *Client) fileReply(id string, res FileResult) {
	ws, err := c.fileStream(id)
	if err != nil {
		return
	}
	defer ws.Close()
	if sendResult(ws, frameResult, res) == nil {
		awaitClose(ws)
	}
}

// pinger keeps the server's read deadline away while the disk is slow.
func pinger(ctx context.Context, ws *websocket.Conn, cancel func()) {
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
}

// sendFile streams a file SQL Server wrote to the server.
func (c *Client) sendFile(ctx context.Context, id, name string, f *os.File, size int64, refuse func(error)) {
	ws, err := c.fileStream(id)
	if err != nil {
		refuse(err)
		return
	}
	defer ws.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverGone := make(chan struct{})
	go func() {
		// the server pings while it reads and cancels with a frame or by closing
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
		<-ctx.Done()
		ws.Close() // unblocks a write to a server that is gone
	}()
	go pinger(ctx, ws, cancel)
	if sendResult(ws, frameInfo, FileResult{Size: size}) != nil {
		return
	}
	start := time.Now()
	out := bufio.NewWriterSize(frameWriter{ws: ws, kind: frameStdout}, maxFrame)
	n, err := io.Copy(out, readerCtx{ctx, f})
	if err == nil {
		err = out.Flush()
	}
	if ctx.Err() != nil {
		c.logf("SQL Server backup folder: sending %s canceled", name)
		return
	}
	res := FileResult{Size: n}
	if err == nil && n != size {
		err = fmt.Errorf("read %d of %d bytes", n, size)
	}
	if err != nil {
		res.Error = err.Error()
	}
	if sendResult(ws, frameResult, res) != nil {
		return
	}
	if err != nil {
		c.logf("SQL Server backup folder: sending %s failed: %v", name, err)
	} else {
		c.logf("SQL Server backup folder: sent %s (%s) in %s", name, engine.HumanBytes(n), time.Since(start).Round(time.Second))
	}
	select {
	case <-serverGone:
	case <-time.After(10 * time.Second):
	}
}

// readerCtx stops a copy when ctx ends.
type readerCtx struct {
	ctx context.Context
	r   io.Reader
}

func (r readerCtx) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// receiveFile writes the file the server sends into the folder (restores,
// the probe of Test connection); anything short of a complete file is
// removed again.
func (c *Client) receiveFile(ctx context.Context, id, p string, f *os.File, refuse func(error)) {
	name := filepath.Base(p)
	done := false
	defer func() {
		if !done {
			f.Close()
			os.Remove(p)
		}
	}()
	ws, err := c.fileStream(id)
	if err != nil {
		refuse(err)
		return
	}
	defer ws.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		ws.Close() // the agent is stopping
	}()
	go pinger(ctx, ws, cancel)
	start := time.Now()
	var n int64
	fail := func(err error) {
		c.logf("SQL Server backup folder: receiving %s failed: %v", name, err)
		if sendResult(ws, frameResult, FileResult{Error: err.Error(), Size: n}) == nil {
			awaitClose(ws)
		}
	}
	for {
		_ = ws.SetReadDeadline(time.Now().Add(idleTimeout))
		kind, data, err := readFrame(ws)
		if err != nil {
			c.logf("SQL Server backup folder: receiving %s stopped: %v", name, err)
			return
		}
		switch kind {
		case frameStdout:
			w, err := f.Write(data)
			n += int64(w)
			if err != nil {
				fail(err)
				return
			}
		case frameCancel:
			c.logf("SQL Server backup folder: receiving %s canceled", name)
			return
		case frameEnd:
			var want FileResult
			if err := json.Unmarshal(data, &want); err != nil || want.Size != n {
				fail(fmt.Errorf("received %d bytes, the server sent %d", n, want.Size))
				return
			}
			err := f.Sync()
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			done = true
			if err == nil {
				err = shareWithServer(p, filepath.Dir(p))
			}
			if err != nil {
				os.Remove(p)
				fail(err)
				return
			}
			if sendResult(ws, frameResult, FileResult{Size: n}) == nil {
				if !strings.Contains(name, "-probe.") { // Test connection's check, every backup has one
					c.logf("SQL Server backup folder: received %s (%s) in %s", name, engine.HumanBytes(n), time.Since(start).Round(time.Second))
				}
				awaitClose(ws)
			}
			return
		}
	}
}

// ---- server side ----

// Folder is SQL Server's backup folder on an agent's host: the agent reads
// the files SQL Server writes there and writes the ones it restores.
type Folder struct {
	m       *Manager
	agentID int64
	agent   string // its name, for messages
	host    string
	dir     string // the folder on the agent's host
}

// Folder returns the backup folder of an agent started with
// --allow-mssql-dir; name names the agent in messages and dir is the folder
// SQL Server writes to (for the hint when the agent does not allow it).
func (m *Manager) Folder(agentID int64, name, dir string) (*Folder, error) {
	in := m.Status(agentID)
	if p := in.FolderProblem(dir); p != "" {
		return nil, errors.New(p)
	}
	return &Folder{m: m, agentID: agentID, agent: name, host: in.Hostname, dir: in.Caps.MSSQL.Dir}, nil
}

// Dir is the folder as the agent opens it.
func (f *Folder) Dir() string { return f.dir }

// wrap says which agent an error comes from.
func (f *Folder) wrap(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("agent %s: %w", f.agent, err)
}

func (f *Folder) String() string {
	s := "agent " + f.agent + " (" + f.dir
	if f.host != "" {
		s += " on " + f.host
	}
	return s + ")"
}

// open sends a file request and waits for its data connection; release
// closes it.
func (f *Folder) open(req FileRequest) (*streamConn, func(), error) {
	c, err := f.m.request(f.agentID, Message{Type: "file", File: &req})
	if err != nil {
		return nil, nil, f.wrap(err)
	}
	m := f.m
	m.mu.Lock()
	m.streams[f.agentID]++
	m.active[c] = f.agentID // a revoked key closes it
	m.mu.Unlock()
	var once sync.Once
	return c, func() {
		once.Do(func() {
			c.Close()
			m.mu.Lock()
			m.streams[f.agentID]--
			delete(m.active, c)
			m.mu.Unlock()
		})
	}, nil
}

// result reads the frames of the agent up to its FileResult.
func result(ws *websocket.Conn, want byte) (*FileResult, error) {
	for {
		_ = ws.SetReadDeadline(time.Now().Add(idleTimeout))
		kind, data, err := readFrame(ws)
		if err != nil {
			return nil, fmt.Errorf("the connection to the agent was lost: %w", err)
		}
		switch kind {
		case framePing:
			continue
		case want, frameResult:
			var res FileResult
			if err := json.Unmarshal(data, &res); err != nil {
				return nil, fmt.Errorf("invalid answer from the agent: %w", err)
			}
			if res.Error != "" {
				return nil, errors.New(res.Error)
			}
			return &res, nil
		}
		return nil, fmt.Errorf("unexpected frame %q from the agent", kind)
	}
}

// watch pings the agent while a file is on its way and cancels it when
// ctx ends; the returned func stops it.
func watch(ctx context.Context, c *streamConn, release func()) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_ = writeFrame(c.Conn, frameCancel, nil)
				release()
				return
			case <-t.C:
				// the deadline of the data frames too: a shorter one would cut
				// a frame short that waits for a slow disk on the agent
				_ = c.SetWriteDeadline(time.Now().Add(idleTimeout))
				if writeFrame(c.Conn, framePing, nil) != nil {
					release()
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// Free checks the folder and returns its free bytes (-1: unknown).
func (f *Folder) Free(ctx context.Context) (int64, error) {
	c, release, err := f.open(FileRequest{Op: "stat"})
	if err != nil {
		return 0, err
	}
	defer release()
	res, err := result(c.Conn, frameResult)
	if err != nil {
		return 0, f.wrap(err)
	}
	return res.Free, nil
}

// Remove deletes a file of the folder; one that is gone is no error.
func (f *Folder) Remove(ctx context.Context, name string) error {
	c, release, err := f.open(FileRequest{Op: "remove", Name: name})
	if err != nil {
		return err
	}
	defer release()
	_, err = result(c.Conn, frameResult)
	return f.wrap(err)
}

// Open streams a file SQL Server wrote from the agent; the reader fails
// unless the whole file arrived.
func (f *Folder) Open(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	c, release, err := f.open(FileRequest{Op: "read", Name: name})
	if err != nil {
		return nil, 0, err
	}
	stop := watch(ctx, c, release)
	info, err := result(c.Conn, frameInfo)
	if err != nil {
		stop()
		release()
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, f.wrap(err)
	}
	return &folderReader{ctx: ctx, c: c, size: info.Size, close: func() { stop(); release() }}, info.Size, nil
}

type folderReader struct {
	ctx   context.Context
	c     *streamConn
	size  int64
	n     int64
	buf   []byte
	err   error
	close func()
}

func (r *folderReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		_ = r.c.SetReadDeadline(time.Now().Add(idleTimeout))
		kind, data, err := readFrame(r.c.Conn)
		switch {
		case err != nil:
			if r.ctx.Err() != nil {
				r.err = r.ctx.Err()
			} else {
				r.err = fmt.Errorf("the connection to the agent was lost: %w", err)
			}
		case kind == frameStdout:
			r.buf = data
			r.n += int64(len(data))
			if r.n > r.size {
				r.err = fmt.Errorf("the agent sent more than the file's %d bytes", r.size)
				r.buf = nil
			}
		case kind == frameResult:
			var res FileResult
			switch {
			case json.Unmarshal(data, &res) != nil:
				r.err = errors.New("invalid answer from the agent")
			case res.Error != "":
				r.err = fmt.Errorf("the agent could not read the file: %s", res.Error)
			case r.n != r.size:
				r.err = fmt.Errorf("the agent sent %d of %d bytes", r.n, r.size)
			default:
				r.err = io.EOF
			}
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *folderReader) Close() error {
	r.close()
	return nil
}

// Create writes a new file into the folder on the agent's host; it is
// there, complete and synced, when Create returns without an error.
func (f *Folder) Create(ctx context.Context, name string, src io.Reader) (int64, error) {
	c, release, err := f.open(FileRequest{Op: "write", Name: name})
	if err != nil {
		return 0, err
	}
	defer release()
	stop := watch(ctx, c, release)
	defer stop()
	// the agent answers once: at the end, or when it cannot write
	answer := make(chan error, 1)
	go func() {
		_, err := result(c.Conn, frameResult)
		answer <- err
		if err != nil {
			release() // unblocks the copy below
		}
	}()
	var n int64
	buf := make([]byte, maxFrame)
	var werr error
	for werr == nil {
		k, rerr := io.ReadFull(src, buf)
		if k > 0 {
			_ = c.SetWriteDeadline(time.Now().Add(idleTimeout))
			if werr = writeFrame(c.Conn, frameStdout, buf[:k]); werr == nil {
				n += int64(k)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil && werr == nil {
			return n, rerr // closing the connection makes the agent remove the file
		}
	}
	if werr == nil {
		data, _ := json.Marshal(FileResult{Size: n})
		_ = c.SetWriteDeadline(time.Now().Add(idleTimeout))
		werr = writeFrame(c.Conn, frameEnd, data)
	}
	if werr != nil {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		select {
		case err := <-answer: // the agent said why it stopped
			if err != nil {
				return n, f.wrap(err)
			}
		case <-time.After(5 * time.Second):
		}
		return n, f.wrap(fmt.Errorf("the connection was lost: %w", werr))
	}
	select {
	case err := <-answer:
		if err != nil && ctx.Err() != nil {
			return n, ctx.Err()
		}
		return n, f.wrap(err)
	case <-ctx.Done():
		return n, ctx.Err()
	}
}
