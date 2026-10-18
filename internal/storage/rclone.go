package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/auth"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
)

// Manager creates backends for configured storages.
type Manager struct {
	Store  *store.Store
	TmpDir string
	Rclone func() string   // returns path to rclone binary or ""
	Flags  func() []string // extra global rclone flags

	obsMu    sync.Mutex
	obsCache map[string]string
	syncMu   sync.Mutex
}

func NewManager(st *store.Store, tmpDir string, rcloneBin func() string, flags func() []string) *Manager {
	os.MkdirAll(tmpDir, 0o700)
	// remove stale temp configs from previous runs
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "op-") {
				os.Remove(filepath.Join(tmpDir, e.Name()))
			}
		}
	}
	return &Manager{Store: st, TmpDir: tmpDir, Rclone: rcloneBin, Flags: flags, obsCache: map[string]string{}}
}

// Get returns a backend for a stored storage definition.
func (m *Manager) Get(st *store.Storage) (Backend, error) {
	if st.Type == "local" {
		return newLocal(st.Config)
	}
	bin := ""
	if m.Rclone != nil {
		bin = m.Rclone()
	}
	if bin == "" {
		return nil, errors.New("rclone is not installed — it is required for storage type " + st.Type + " (see the System page)")
	}
	section, root, err := m.buildSection(st.Type, st.Config, bin)
	if err != nil {
		return nil, err
	}
	cp := *st
	return &rcloneBackend{m: m, st: &cp, section: section, root: root, bin: bin}, nil
}

func (m *Manager) obscure(bin, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	key := auth.SHA256Hex(bin + "\x00" + value)
	m.obsMu.Lock()
	if v, ok := m.obsCache[key]; ok {
		m.obsMu.Unlock()
		return v, nil
	}
	m.obsMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := execx.Command(ctx, bin, "obscure", "-")
	cmd.Stdin = strings.NewReader(value)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("rclone obscure: %w", err)
	}
	v := strings.TrimSpace(string(out))
	m.obsMu.Lock()
	m.obsCache[key] = v
	m.obsMu.Unlock()
	return v, nil
}

// NormalizeToken extracts the JSON token from pasted `rclone authorize` output.
func NormalizeToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		s = s[i : j+1]
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err == nil {
		return buf.String()
	}
	return strings.Join(strings.Fields(s), "")
}

func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(strings.TrimSpace(s))); err == nil {
		return buf.String()
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", "")
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (m *Manager) buildSection(t string, cfg map[string]string, bin string) (map[string]string, string, error) {
	sec := map[string]string{}
	g := func(k string) string { return strings.TrimSpace(cfg[k]) }
	setIf := func(k, v string) {
		if v != "" {
			sec[k] = v
		}
	}
	var root string
	switch t {
	case "s3":
		sec["type"] = "s3"
		prov := g("provider")
		if prov == "" {
			prov = "AWS"
		}
		sec["provider"] = prov
		if g("access_key_id") != "" {
			sec["env_auth"] = "false"
			sec["access_key_id"] = g("access_key_id")
			sec["secret_access_key"] = g("secret_access_key")
		} else {
			sec["env_auth"] = "true"
		}
		region := g("region")
		if region == "" && prov == "Cloudflare" {
			region = "auto"
		}
		setIf("region", region)
		setIf("endpoint", g("endpoint"))
		setIf("storage_class", g("storage_class"))
		setIf("server_side_encryption", g("sse"))
		if isTrue(g("force_path_style")) {
			sec["force_path_style"] = "true"
		}
		if isTrue(g("no_check_bucket")) {
			sec["no_check_bucket"] = "true"
		}
		sec["chunk_size"] = "64M"
		sec["upload_concurrency"] = "4"
		if g("bucket") == "" {
			return nil, "", errors.New("bucket is missing")
		}
		root = joinRel(g("bucket"), g("prefix"))
	case "gdrive":
		sec["type"] = "drive"
		scope := g("scope")
		if scope == "" {
			scope = "drive"
		}
		sec["scope"] = scope
		setIf("client_id", g("client_id"))
		setIf("client_secret", g("client_secret"))
		if g("auth_mode") == "service_account" {
			sec["service_account_credentials"] = compactJSON(g("service_account_credentials"))
		} else {
			sec["token"] = NormalizeToken(g("token"))
		}
		setIf("team_drive", g("team_drive"))
		setIf("root_folder_id", g("root_folder_id"))
		sec["chunk_size"] = "64M"
		root = g("folder")
	case "onedrive":
		sec["type"] = "onedrive"
		setIf("client_id", g("client_id"))
		setIf("client_secret", g("client_secret"))
		sec["token"] = NormalizeToken(g("token"))
		setIf("drive_id", g("drive_id"))
		dt := g("drive_type")
		if dt == "" {
			dt = "personal"
		}
		sec["drive_type"] = dt
		if r := g("region"); r != "" && r != "global" {
			sec["region"] = r
		}
		root = g("folder")
	case "dropbox":
		sec["type"] = "dropbox"
		setIf("client_id", g("client_id"))
		setIf("client_secret", g("client_secret"))
		sec["token"] = NormalizeToken(g("token"))
		root = g("folder")
	case "ftp":
		sec["type"] = "ftp"
		sec["host"] = g("host")
		sec["user"] = g("user")
		setIf("port", g("port"))
		p, err := m.obscure(bin, cfg["pass"])
		if err != nil {
			return nil, "", err
		}
		setIf("pass", p)
		switch g("tls") {
		case "implicit":
			sec["tls"] = "true"
		case "explicit":
			sec["explicit_tls"] = "true"
		}
		if isTrue(g("no_check_certificate")) {
			sec["no_check_certificate"] = "true"
		}
		root = g("folder")
	case "sftp":
		sec["type"] = "sftp"
		sec["host"] = g("host")
		sec["user"] = g("user")
		setIf("port", g("port"))
		p, err := m.obscure(bin, cfg["pass"])
		if err != nil {
			return nil, "", err
		}
		setIf("pass", p)
		if k := strings.TrimSpace(cfg["key_pem"]); k != "" {
			k = strings.ReplaceAll(k, "\r\n", "\n")
			sec["key_pem"] = strings.ReplaceAll(k, "\n", `\n`)
		}
		kp, err := m.obscure(bin, cfg["key_file_pass"])
		if err != nil {
			return nil, "", err
		}
		setIf("key_file_pass", kp)
		sec["disable_hashcheck"] = "true"
		root = g("folder")
	case "webdav":
		sec["type"] = "webdav"
		sec["url"] = g("url")
		v := g("vendor")
		if v == "" {
			v = "other"
		}
		sec["vendor"] = v
		setIf("user", g("user"))
		p, err := m.obscure(bin, cfg["pass"])
		if err != nil {
			return nil, "", err
		}
		setIf("pass", p)
		root = g("folder")
	case "b2":
		sec["type"] = "b2"
		sec["account"] = g("account")
		sec["key"] = g("key")
		sec["hard_delete"] = "true"
		root = joinRel(g("bucket"), g("prefix"))
	case "azureblob":
		sec["type"] = "azureblob"
		setIf("account", g("account"))
		setIf("key", g("key"))
		setIf("sas_url", g("sas_url"))
		root = joinRel(g("container"), g("prefix"))
	case "rclone":
		for _, ln := range strings.Split(cfg["config"], "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") || strings.HasPrefix(ln, ";") || strings.HasPrefix(ln, "[") {
				continue
			}
			k, v, ok := strings.Cut(ln, "=")
			if !ok {
				continue
			}
			sec[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		if sec["type"] == "" {
			return nil, "", errors.New("the rclone configuration is missing the line: type = ...")
		}
		root = g("path")
	default:
		return nil, "", fmt.Errorf("unsupported storage type: %s", t)
	}
	for k, v := range sec {
		sec[k] = strings.ReplaceAll(strings.ReplaceAll(v, "\r", ""), "\n", "")
	}
	// keep a leading "/" (absolute path on SFTP/FTP/local); only trailing
	// slashes are cosmetic
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	return sec, root, nil
}

// remoteName is the section name in generated configs. Single-letter names
// would be parsed as drive letters by rclone on Windows.
const remoteName = "dumpcat"

type rcloneBackend struct {
	m       *Manager
	st      *store.Storage
	section map[string]string
	root    string
	bin     string
}

func (b *rcloneBackend) Type() string                { return b.st.Type }
func (b *rcloneBackend) IsLocal() bool               { return false }
func (b *rcloneBackend) LocalPath(rel string) string { return "" }

func (b *rcloneBackend) Describe() string {
	if strings.HasPrefix(b.root, "/") {
		return b.st.Type + ":" + b.root
	}
	return b.st.Type + ":/" + b.root
}

func (b *rcloneBackend) remote(rel string) string {
	p := joinRel(b.root, rel) // joinRel strips slashes at both ends
	if strings.HasPrefix(b.root, "/") {
		p = "/" + p
	}
	return remoteName + ":" + p
}

func (b *rcloneBackend) writeConfig() (string, error) {
	keys := make([]string, 0, len(b.section))
	for k := range b.section {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("[" + remoteName + "]\n")
	for _, k := range keys {
		sb.WriteString(k + " = " + b.section[k] + "\n")
	}
	// CreateTemp uses mode 0600, so credentials are readable by our user only.
	f, err := os.CreateTemp(b.m.TmpDir, "op-*.conf")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

type tokenJSON struct {
	Expiry time.Time `json:"expiry"`
}

// syncToken copies a refreshed OAuth token from the temporary config file
// back to the database ("newest expiry wins").
func (b *rcloneBackend) syncToken(cfgPath string) {
	old := b.section["token"]
	if old == "" {
		return
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var newTok string
	for _, ln := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(ln, "=")
		if ok && strings.TrimSpace(k) == "token" {
			newTok = strings.TrimSpace(v)
		}
	}
	if newTok == "" || newTok == old {
		return
	}
	var o, n tokenJSON
	_ = json.Unmarshal([]byte(old), &o)
	if json.Unmarshal([]byte(newTok), &n) != nil || !n.Expiry.After(o.Expiry) {
		return
	}
	b.m.syncMu.Lock()
	defer b.m.syncMu.Unlock()
	cur, err := b.m.Store.GetStorage(b.st.ID)
	if err != nil {
		return
	}
	var c tokenJSON
	_ = json.Unmarshal([]byte(NormalizeToken(cur.Config["token"])), &c)
	if !n.Expiry.After(c.Expiry) {
		return
	}
	cur.Config["token"] = newTok
	_ = b.m.Store.UpdateStorageConfig(cur.ID, cur.Config)
	b.section["token"] = newTok
}

var pctRe = regexp.MustCompile(`,\s*(\d{1,3})%,`)

type progressWriter struct {
	w  io.Writer
	op *Op
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if p.op != nil && p.op.OnProgress != nil {
		if m := pctRe.FindAllSubmatch(b, -1); len(m) > 0 {
			if v, err := strconv.Atoi(string(m[len(m)-1][1])); err == nil {
				p.op.OnProgress(v)
			}
		}
	}
	return p.w.Write(b)
}

// tailBuffer keeps the last N bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func (b *rcloneBackend) prepare(ctx context.Context, op *Op, transfer bool, args ...string) (*exec.Cmd, *tailBuffer, func(), error) {
	cfg, err := b.writeConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	full := []string{"--config", cfg}
	full = append(full, args...)
	full = append(full, "--retries", "3", "--low-level-retries", "10")
	if transfer {
		full = append(full, "--stats", "20s", "--stats-one-line", "--stats-log-level", "NOTICE")
	}
	if b.m.Flags != nil {
		full = append(full, b.m.Flags()...)
	}
	if op != nil && op.BwLimit != "" {
		full = append(full, "--bwlimit", op.BwLimit)
	}
	cmd := execx.Command(ctx, b.bin, full...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG_PASS=") // never prompt
	tail := &tailBuffer{max: 4096}
	var lw io.WriteCloser
	if op != nil && op.Log != nil {
		lw = op.Log.Writer("[rclone] ")
		cmd.Stderr = io.MultiWriter(tail, &progressWriter{w: lw, op: op})
	} else {
		cmd.Stderr = tail
	}
	cleanup := func() {
		if lw != nil {
			lw.Close()
		}
		b.syncToken(cfg)
		os.Remove(cfg)
	}
	return cmd, tail, cleanup, nil
}

func rcloneErr(err error, tail *tailBuffer) error {
	msg := tail.String()
	if msg == "" {
		return fmt.Errorf("rclone: %w", err)
	}
	lines := strings.Split(msg, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return fmt.Errorf("rclone: %v: %s", err, strings.Join(lines, " | "))
}

func (b *rcloneBackend) run(ctx context.Context, op *Op, transfer bool, args ...string) ([]byte, error) {
	cmd, tail, cleanup, err := b.prepare(ctx, op, transfer, args...)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, rcloneErr(err, tail)
	}
	return out.Bytes(), nil
}

func (b *rcloneBackend) UploadDir(ctx context.Context, localDir, rel string, op *Op) error {
	_, err := b.run(ctx, op, true, "copy", localDir, b.remote(rel), "--transfers", "4", "--checkers", "8")
	return err
}

func (b *rcloneBackend) PutStream(ctx context.Context, rel string, r io.Reader, op *Op) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, tail, cleanup, err := b.prepare(cctx, op, true, "rcat", b.remote(rel))
	if err != nil {
		return err
	}
	defer cleanup()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, copyErr := io.Copy(stdin, r)
	if copyErr != nil {
		// Kill rclone BEFORE closing stdin so a truncated stream is never committed.
		cancel()
		_ = cmd.Wait()
		return fmt.Errorf("stream aborted: %w", copyErr)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return rcloneErr(err, tail)
	}
	return nil
}

type procReader struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	tail    *tailBuffer
	cleanup func()
	cancel  context.CancelFunc
	once    sync.Once
	err     error
	eof     bool
}

func (p *procReader) wait() error {
	p.once.Do(func() {
		if err := p.cmd.Wait(); err != nil {
			p.err = rcloneErr(err, p.tail)
		}
		p.cleanup()
		p.cancel()
	})
	return p.err
}

func (p *procReader) Read(b []byte) (int, error) {
	if p.eof {
		// the pipe is closed after Wait; keep reporting EOF (or the exit error)
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.EOF
	}
	n, err := p.stdout.Read(b)
	if err == io.EOF {
		p.eof = true
		if werr := p.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (p *procReader) Close() error {
	p.cancel()
	_ = p.wait()
	return nil
}

func (b *rcloneBackend) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	cctx, cancel := context.WithCancel(ctx)
	cmd, tail, cleanup, err := b.prepare(cctx, nil, false, "cat", b.remote(rel))
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		cancel()
		return nil, err
	}
	return &procReader{cmd: cmd, stdout: stdout, tail: tail, cleanup: cleanup, cancel: cancel}, nil
}

func (b *rcloneBackend) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	return b.run(ctx, nil, false, "cat", b.remote(rel))
}

type lsItem struct {
	Path    string    `json:"Path"`
	Name    string    `json:"Name"`
	Size    int64     `json:"Size"`
	ModTime time.Time `json:"ModTime"`
	IsDir   bool      `json:"IsDir"`
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "directory not found") || strings.Contains(s, "not found") || strings.Contains(s, "doesn't exist") || strings.Contains(s, "no such file")
}

func (b *rcloneBackend) lsjson(ctx context.Context, rel string, extra ...string) ([]Entry, error) {
	args := append([]string{"lsjson", b.remote(rel)}, extra...)
	out, err := b.run(ctx, nil, false, args...)
	if err != nil {
		if isNotFound(err) {
			return []Entry{}, nil
		}
		return nil, err
	}
	var items []lsItem
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("lsjson: %w", err)
	}
	res := make([]Entry, 0, len(items))
	for _, it := range items {
		res = append(res, Entry{Path: joinRel(rel, it.Path), Name: it.Name, Size: it.Size, IsDir: it.IsDir, ModTime: it.ModTime})
	}
	return res, nil
}

func (b *rcloneBackend) List(ctx context.Context, rel string) ([]Entry, error) {
	return b.lsjson(ctx, rel)
}

func (b *rcloneBackend) FindFiles(ctx context.Context, rel, name string, maxDepth int) ([]Entry, error) {
	return b.lsjson(ctx, rel, "--recursive", "--files-only", "--include", name, "--max-depth", strconv.Itoa(maxDepth), "--fast-list")
}

func (b *rcloneBackend) DeleteDir(ctx context.Context, rel string) error {
	rel, err := CleanRel(rel)
	if err != nil || rel == "" {
		return errors.New("refusing to delete the storage root directory")
	}
	_, err = b.run(ctx, nil, false, "purge", b.remote(rel))
	if isNotFound(err) {
		return nil
	}
	return err
}

func (b *rcloneBackend) DeleteFile(ctx context.Context, rel string) error {
	_, err := b.run(ctx, nil, false, "deletefile", b.remote(rel))
	if isNotFound(err) {
		return nil
	}
	return err
}

func (b *rcloneBackend) DirSize(ctx context.Context, rel string) (int64, error) {
	out, err := b.run(ctx, nil, false, "size", b.remote(rel), "--json")
	if err != nil {
		return 0, err
	}
	var r struct {
		Bytes int64 `json:"bytes"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return 0, err
	}
	return r.Bytes, nil
}

func (b *rcloneBackend) Test(ctx context.Context) (string, error) {
	if _, err := b.run(ctx, nil, false, "mkdir", b.remote("")); err != nil {
		return "", err
	}
	name := ".dumpcat-test-" + auth.RandomToken(6)
	if err := b.PutStream(ctx, name, strings.NewReader("dumpcat write test\n"), nil); err != nil {
		return "", fmt.Errorf("test write failed: %w", err)
	}
	data, err := b.ReadFile(ctx, name)
	if err != nil {
		return "", fmt.Errorf("test read failed: %w", err)
	}
	if !strings.HasPrefix(string(data), "dumpcat write test") {
		return "", errors.New("the content read back does not match")
	}
	if err := b.DeleteFile(ctx, name); err != nil {
		return "", fmt.Errorf("test delete failed: %w", err)
	}
	msg := "Write/read/delete OK"
	if u, err := b.About(ctx); err == nil && u.Total > 0 {
		msg += fmt.Sprintf(" — %s free / %s", humanBytes(u.Free), humanBytes(u.Total))
	}
	return msg, nil
}

func (b *rcloneBackend) About(ctx context.Context) (*Usage, error) {
	out, err := b.run(ctx, nil, false, "about", remoteName+":", "--json")
	if err != nil {
		return nil, err
	}
	var u struct {
		Total *int64 `json:"total"`
		Used  *int64 `json:"used"`
		Free  *int64 `json:"free"`
	}
	if err := json.Unmarshal(out, &u); err != nil {
		return nil, err
	}
	res := &Usage{Total: -1, Used: -1, Free: -1}
	if u.Total != nil {
		res.Total = *u.Total
	}
	if u.Used != nil {
		res.Used = *u.Used
	}
	if u.Free != nil {
		res.Free = *u.Free
	}
	return res, nil
}

func (b *rcloneBackend) Download(ctx context.Context, rel, localPath string, op *Op) error {
	_, err := b.run(ctx, op, true, "copyto", b.remote(rel), localPath, "--multi-thread-streams", "4")
	return err
}

// RcloneVersion returns the version line of the rclone binary.
func RcloneVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := execx.Command(ctx, bin, "version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}
