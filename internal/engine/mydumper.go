package engine

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"dumpcat/internal/execx"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/store"
)

func reCompressTok(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + name + ``)
}

// mydumperTLSArgs maps the server TLS mode to mydumper/myloader options.
func mydumperTLSArgs(help string, s *store.Server, dir string) []string {
	var args []string
	switch s.TLSMode {
	case "required", "verify":
		mode := "REQUIRED"
		if s.TLSMode == "verify" {
			mode = "VERIFY_IDENTITY"
		}
		if strings.Contains(help, "--ssl-mode") {
			args = append(args, "--ssl-mode="+mode)
		} else if strings.Contains(help, "--ssl") {
			args = append(args, "--ssl")
		}
		if s.TLSMode == "verify" && strings.TrimSpace(s.CACert) != "" && strings.Contains(help, "--ca") {
			ca := filepath.Join(dir, "mydumper-ca.pem")
			if os.WriteFile(ca, []byte(s.CACert), 0o600) == nil {
				args = append(args, "--ca="+ca)
			}
		}
	case "disabled":
		if strings.Contains(help, "--ssl-mode") {
			args = append(args, "--ssl-mode=DISABLED")
		}
	}
	return args
}

// pcreQuote escapes regex metacharacters (PCRE compatible).
func pcreQuote(s string) string { return regexp.QuoteMeta(s) }

// mydumperRegex selects db.table names: included databases minus excluded tables.
func mydumperRegex(dbs []string, excl []string) string {
	q := make([]string, len(dbs))
	for i, d := range dbs {
		q[i] = pcreQuote(d)
	}
	re := `^(?=(?:` + strings.Join(q, "|") + `)\.)`
	var ex []string
	for _, e := range excl {
		d, t, ok := strings.Cut(strings.TrimSpace(e), ".")
		if ok && d != "" && t != "" {
			ex = append(ex, pcreQuote(d)+`\.`+pcreQuote(t)+`$`)
		}
	}
	if len(ex) > 0 {
		re += `(?!(?:` + strings.Join(ex, "|") + `))`
	}
	return re
}

// Mydumper runs a parallel, chunked logical dump and stores the dump
// directory as a single tar stream (files inside are compressed by mydumper).
func Mydumper(env *Env, spec *BackupSpec, sink Sink) (*Result, error) {
	tool := env.Tools.Get("mydumper")
	if err := requireTool(tool); err != nil {
		return nil, err
	}
	env.Log.Printf("Tool: %s (%s)", tool.Path, tool.Version)
	if len(spec.Databases) == 0 {
		return nil, errors.New("no databases to back up")
	}
	outDir := filepath.Join(env.TmpDir, "mydumper")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}
	// mydumper >= 0.11 parses its defaults file with GKeyFile: write only a
	// [mydumper] group with GKeyFile escaping; TLS goes on the command line.
	cnf, extraEnv, err := WriteCnf(env.TmpDir, spec.Server, CnfOptions{GlibGroups: []string{"mydumper"}})
	if err != nil {
		return nil, err
	}
	o := spec.Job.Options
	threads := spec.Job.Threads
	if threads <= 0 {
		threads = 4
	}
	if threads < 2 {
		threads = 2 // mydumper minimum
	}
	h := env.Tools.Help("mydumper")
	has := func(f string) bool { return strings.Contains(h, "--"+f) }
	args := []string{}
	if has("defaults-file") {
		args = append(args, "--defaults-file="+cnf)
	} else {
		s := spec.Server
		args = append(args, "--user="+s.Username)
		host, port, socket, err := s.Addr()
		if err != nil {
			return nil, err
		}
		if socket != "" {
			args = append(args, "--socket="+socket)
		} else {
			args = append(args, "--host="+host, "--port="+strconv.Itoa(port))
		}
	}
	args = append(args, mydumperTLSArgs(h, spec.Server, env.TmpDir)...)
	args = append(args, "--outputdir="+outDir, "--threads="+strconv.Itoa(threads), "--verbose=3")
	rows := o.ChunkRows
	if rows <= 0 {
		rows = 500000
	}
	if has("rows") {
		args = append(args, "--rows="+strconv.Itoa(rows))
	}
	// Transactional consistency without holding a global lock: FTWRL (or backup
	// lock) only for the instant needed to start all snapshot transactions.
	switch {
	case has("trx-tables"):
		args = append(args, "--trx-tables")
	case has("trx-consistency-only"):
		args = append(args, "--trx-consistency-only")
	}
	if m := strings.ToUpper(strings.TrimSpace(o.LockMode)); m != "" && m != "AUTO" && has("sync-thread-lock-mode") {
		args = append(args, "--sync-thread-lock-mode="+m)
	}
	if o.LongQueryGuard > 0 && has("long-query-guard") {
		args = append(args, "--long-query-guard="+strconv.Itoa(o.LongQueryGuard))
	}
	if o.Triggers {
		args = append(args, "--triggers")
	}
	if o.Events {
		args = append(args, "--events")
	}
	if o.Routines {
		args = append(args, "--routines")
	}
	// --compress takes GZIP/ZSTD (0.12-0.15) or gzip/zstd (newer); 0.10 is a plain gzip flag.
	comp := pipeline.NormalizeCompression(spec.Job.Compression)
	if comp != pipeline.CompNone && has("compress") {
		want := "gzip"
		if comp == pipeline.CompZstd {
			want = "zstd"
		}
		if tok := reCompressTok(want).FindString(h); tok != "" {
			args = append(args, "--compress="+tok)
		} else if tok := reCompressTok("gzip").FindString(h); tok != "" {
			args = append(args, "--compress="+tok)
		} else {
			args = append(args, "--compress")
		}
	}
	names := make([]string, len(spec.Databases))
	for i, d := range spec.Databases {
		names[i] = d.Name
	}
	args = append(args, "--regex="+mydumperRegex(names, spec.Job.ExcludeTables))
	if o.ExtraArgs != "" {
		args = append(args, execx.SplitArgs(o.ExtraArgs)...)
	}
	cmd := env.command(tool.Path, args, extraEnv)
	if err := env.runCmd(cmd, "[mydumper] "); err != nil {
		return nil, err
	}
	res := &Result{Meta: map[string]any{"tool": tool.Version}, Databases: toEntries(spec.Databases)}
	if md, err := os.ReadFile(filepath.Join(outDir, "metadata")); err == nil {
		s := string(md)
		if len(s) > 4096 {
			s = s[:4096]
		}
		res.Meta["mydumper_metadata"] = s
		parseMydumperMetadata(s, res.Meta)
	} else {
		return nil, errors.New("mydumper did not write a metadata file — the dump is incomplete")
	}
	for i := range res.Databases {
		res.Databases[i].File = "mydumper.tar"
	}
	env.Log.Printf("Packing the dump directory into mydumper.tar")
	w, err := sink.Create("mydumper.tar", FileOpts{Kind: "tar", Compress: false})
	if err != nil {
		return nil, err
	}
	if err := finish(w, TarDir(outDir, w)); err != nil {
		return nil, err
	}
	os.RemoveAll(outDir)
	if o.IncludeUsers {
		if err := dumpUsers(env, spec, sink); err != nil {
			env.Log.Printf("WARNING: could not dump users/grants: %v", err)
		}
	}
	return res, nil
}

var (
	reMdLog  = regexp.MustCompile(`(?m)^\s*(?:Log|File|SOURCE_LOG_FILE|MASTER_LOG_FILE)\s*[:=]\s*'?([^'\s]+)'?`)
	reMdPos  = regexp.MustCompile(`(?m)^\s*(?:Pos|Position|SOURCE_LOG_POS|MASTER_LOG_POS)\s*[:=]\s*'?(\d+)'?`)
	reMdGtid = regexp.MustCompile(`(?m)^\s*(?:GTID|Executed_Gtid_Set|executed_gtid_set)\s*[:=]\s*'?([^'\n]*)'?`)
)

func parseMydumperMetadata(s string, meta map[string]any) {
	if m := reMdLog.FindStringSubmatch(s); m != nil {
		meta["binlog_file"] = m[1]
	}
	if m := reMdPos.FindStringSubmatch(s); m != nil {
		if p, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			meta["binlog_pos"] = p
		}
	}
	if m := reMdGtid.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		meta["gtid"] = strings.TrimSpace(m[1])
	}
}

// TarDir writes all regular files of dir (recursively) to w as a tar stream.
func TarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uname, hdr.Gname = "", ""
		hdr.Uid, hdr.Gid = 0, 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// UntarTo extracts a tar stream into dir (rejecting path traversal).
func UntarTo(r io.Reader, dir string) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	base, err := filepath.Abs(dir)
	if err != nil {
		return 0, err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(base, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, base+string(os.PathSeparator)) {
			return n, fmt.Errorf("invalid path in tar: %s", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return n, err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
		if err != nil {
			return n, err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return n, err
		}
		if err := f.Close(); err != nil {
			return n, err
		}
		n++
	}
}
