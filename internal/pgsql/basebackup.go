package pgsql

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
)

// BaseFile is the tar stream of a pg_basebackup backup: the data directory,
// the WAL of the backup (pg_wal/) and backup_manifest.
const BaseFile = "base.tar"

// cEnv makes the client tools print the English messages DumpCat reads.
var cEnv = []string{"LC_ALL=C", "LC_MESSAGES=C", "LANG=C", "LANGUAGE=C"}

// baseInfo is what DumpCat reads from the stream while it passes.
type baseInfo struct {
	mu        sync.Mutex
	label     string // backup_label
	manifest  []byte
	sysID     uint64
	version   string // PG_VERSION
	tblspcMap bool
	err       error
}

// scanBase reads the tar stream of a base backup until its end (it must
// never stop reading: the backup itself waits on the same stream).
func (b *baseInfo) scan(r io.Reader) {
	defer io.Copy(io.Discard, r)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				b.mu.Lock()
				b.err = err
				b.mu.Unlock()
			}
			return
		}
		name := strings.TrimPrefix(h.Name, "./")
		read := func(limit int64) []byte {
			data, _ := io.ReadAll(io.LimitReader(tr, limit))
			return data
		}
		b.mu.Lock()
		switch name {
		case "backup_label":
			b.label = string(read(64 << 10))
		case "backup_manifest":
			b.manifest = read(256 << 20)
		case "global/pg_control":
			if d := read(8192); len(d) >= 8 {
				b.sysID = binary.LittleEndian.Uint64(d)
			}
		case "PG_VERSION":
			b.version = strings.TrimSpace(string(read(64)))
		case "tablespace_map":
			b.tblspcMap = len(bytes.TrimSpace(read(1<<20))) > 0
		}
		b.mu.Unlock()
	}
}

var (
	reStartPoint = regexp.MustCompile(`write-ahead log start point: ([0-9A-F]+/[0-9A-F]+) on timeline (\d+)`)
	reEndPoint   = regexp.MustCompile(`write-ahead log end point: ([0-9A-F]+/[0-9A-F]+)`)
	reLabelStart = regexp.MustCompile(`(?m)^START WAL LOCATION: ([0-9A-F]+/[0-9A-F]+)`)
	reLabelTLI   = regexp.MustCompile(`(?m)^START TIMELINE: (\d+)`)
)

// lineLog keeps what pg_basebackup prints (small).
type lineLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() < 1<<20 {
		l.buf.Write(p)
	}
	return len(p), nil
}

func (l *lineLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// baseBackup copies the whole cluster with pg_basebackup as one tar stream
// straight into the sink. The WAL the backup needs is fetched at its end;
// a temporary replication slot, held by DumpCat's connection while the
// backup runs, keeps the server from removing it before.
func baseBackup(env *engine.Env, spec *engine.BackupSpec, sink engine.Sink) (*engine.Result, error) {
	ctx := env.Ctx
	srvMajor := serverMajor(spec.Info, spec.ServerVersion)
	tool, err := env.Tools.PGFor("pg_basebackup", srvMajor)
	if err != nil {
		return nil, err
	}
	env.Log.Printf("Tool: %s (%s)", tool.Path, tool.Version)
	pwEnv, err := WritePassFile(env.TmpDir, spec.Server)
	if err != nil {
		return nil, err
	}
	conn, err := ConnInfo(spec.Server, "postgres", env.TmpDir)
	if err != nil {
		return nil, err
	}
	c, err := Connect(ctx, spec.Server, "")
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	var segSize int64
	if err := c.QueryRow(ctx, "SELECT pg_size_bytes(current_setting('wal_segment_size'))").Scan(&segSize); err != nil {
		return nil, fmt.Errorf("reading wal_segment_size: %w", err)
	}
	slot := fmt.Sprintf("dumpcat_base_%d", time.Now().UnixNano()%1_000_000_000_000)
	if _, err := c.Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true, true)", slot); err != nil {
		env.Log.Printf("WARNING: could not create a temporary replication slot (%v): the WAL of the backup must stay on the server until it ends (wal_keep_size)", err)
	} else {
		env.Log.Printf("Temporary replication slot %s keeps the WAL of the backup until it is fetched", slot)
	}

	label := "dumpcat " + time.Now().Format("2006-01-02 15:04:05")
	// Windows' pg_basebackup writes stdout in text mode (each LF becomes
	// CRLF), so there the tar goes through the staging folder
	staged := runtime.GOOS == "windows"
	stage := filepath.Join(env.TmpDir, "pgbase")
	pgdata := "-"
	if staged {
		pgdata = stage
	}
	args := []string{"--pgdata=" + pgdata, "--format=tar", "--wal-method=fetch", "--checkpoint=fast", "--label=" + label, "--no-password", "--verbose"}
	if o := spec.Job.Options.ExtraArgs; o != "" {
		args = append(args, execx.SplitArgs(o)...)
	}
	args = append(args, "--dbname="+conn)
	w, err := sink.Create(BaseFile, engine.FileOpts{Kind: "pgbase", Compress: true})
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	info := &baseInfo{}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		info.scan(pr)
	}()
	out := &lineLog{}
	cmd := env.Command(tool.Path, args, append(pwEnv, cEnv...))
	if !staged {
		cmd.Stdout = io.MultiWriter(w, pw)
	}
	cmd.Stderr = out
	if staged {
		env.Log.Printf("Copying the cluster into the staging folder (data directory and the WAL of the backup)")
	} else {
		env.Log.Printf("Copying the cluster (data directory and the WAL of the backup, one tar stream)")
	}
	runErr := env.RunCmd(cmd, "[pg_basebackup] ")
	end := time.Now()
	msgs := out.String()
	if runErr == nil && staged {
		runErr = copyStaged(stage, io.MultiWriter(w, pw), sink)
	}
	pw.Close()
	<-scanned
	if runErr != nil && strings.Contains(msgs, "single tablespace") {
		runErr = fmt.Errorf("%w — clusters with additional tablespaces cannot be written as one stream; DumpCat does not back them up with pg_basebackup yet (use pg_dump)", runErr)
	}
	if err := engine.Finish(w, runErr); err != nil {
		return nil, err
	}
	if staged {
		if info.manifest == nil {
			info.manifest, _ = os.ReadFile(filepath.Join(stage, "backup_manifest"))
		}
		os.RemoveAll(stage)
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	if info.err != nil {
		return nil, fmt.Errorf("reading the backup stream: %w", info.err)
	}
	if info.label == "" || info.sysID == 0 {
		return nil, errors.New("the backup stream holds no backup_label or pg_control")
	}
	if info.tblspcMap {
		return nil, errors.New("the backup has a tablespace_map: tablespaces are not supported with pg_basebackup yet")
	}
	meta := map[string]any{"tool": tool.Version, "system_id": strconv.FormatUint(info.sysID, 10), "wal_segment_size": segSize,
		"pg_major": major(info.version), "consistent_at": end.Unix()}
	start, tli := "", 0
	if m := reStartPoint.FindStringSubmatch(msgs); m != nil {
		start = m[1]
		tli, _ = strconv.Atoi(m[2])
	} else if m := reLabelStart.FindStringSubmatch(info.label); m != nil {
		start = m[1]
		if m := reLabelTLI.FindStringSubmatch(info.label); m != nil {
			tli, _ = strconv.Atoi(m[1])
		}
	}
	stop := ""
	if m := reEndPoint.FindStringSubmatch(msgs); m != nil {
		stop = m[1]
	}
	if mstart, mstop, mtli := manifestWAL(info.manifest); mstop != "" {
		stop = mstop
		if start == "" {
			start, tli = mstart, mtli
		}
	}
	if start == "" || stop == "" {
		return nil, errors.New("pg_basebackup did not report where the backup's WAL starts and ends")
	}
	if _, err := ParseLSN(start); err != nil {
		return nil, err
	}
	if _, err := ParseLSN(stop); err != nil {
		return nil, err
	}
	meta["start_lsn"], meta["end_lsn"], meta["timeline"] = start, stop, tli
	env.Log.Printf("Backup of PostgreSQL %s (system %d): WAL %s → %s on timeline %d", info.version, info.sysID, start, stop, tli)
	return &engine.Result{Meta: meta, Databases: engine.ToEntries(spec.Databases)}, nil
}

// copyStaged writes the staged base.tar to w and backup_manifest as a file
// of its own (written to stdout, the manifest is inside the tar).
func copyStaged(stage string, w io.Writer, sink engine.Sink) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".tar") && n != BaseFile {
			return fmt.Errorf("the cluster has an additional tablespace (%s): DumpCat does not back up tablespaces with pg_basebackup yet (use pg_dump)", n)
		}
	}
	f, err := os.Open(filepath.Join(stage, BaseFile))
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	f.Close()
	if err != nil {
		return err
	}
	m, err := os.Open(filepath.Join(stage, "backup_manifest"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer m.Close()
	mw, err := sink.Create("backup_manifest", engine.FileOpts{Kind: "manifest", Compress: true})
	if err != nil {
		return err
	}
	_, err = io.Copy(mw, m)
	return engine.Finish(mw, err)
}

// manifestWAL reads the WAL range of backup_manifest (PostgreSQL 13+).
func manifestWAL(data []byte) (start, end string, tli int) {
	var m struct {
		Ranges []struct {
			Timeline int    `json:"Timeline"`
			Start    string `json:"Start-LSN"`
			End      string `json:"End-LSN"`
		} `json:"WAL-Ranges"`
	}
	if len(data) == 0 || json.Unmarshal(data, &m) != nil || len(m.Ranges) == 0 {
		return "", "", 0
	}
	first, last := m.Ranges[0], m.Ranges[len(m.Ranges)-1]
	return first.Start, last.End, first.Timeline
}
