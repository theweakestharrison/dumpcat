package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dumpcat/internal/execx"
)

// Physical runs a hot, non-blocking physical backup with Percona XtraBackup
// (MySQL/Percona) or MariaBackup (MariaDB), streamed as xbstream.
// InnoDB is copied without locks; backup locks / BACKUP STAGE are used only
// for the short final phase (non-InnoDB tables + binlog position).
func Physical(env *Env, spec *BackupSpec, sink Sink) (*Result, error) {
	bname, _ := PhysicalToolNames(spec.Flavor)
	tool := env.Tools.Get(bname)
	if err := requireTool(tool); err != nil {
		return nil, err
	}
	env.Log.Printf("Tool: %s (%s)", tool.Path, tool.Version)
	groups := []string{"xtrabackup"}
	if bname == "mariabackup" {
		groups = []string{"mariabackup", "mariadb-backup"}
	}
	cnf, extraEnv, err := WriteCnf(env.TmpDir, spec.Server, CnfOptions{Groups: groups})
	if err != nil {
		return nil, err
	}
	lsnDir := filepath.Join(env.TmpDir, "lsn")
	tmpTarget := filepath.Join(env.TmpDir, "xbtmp")
	for _, d := range []string{lsnDir, tmpTarget} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	o := spec.Job.Options
	threads := spec.Job.Threads
	if threads <= 0 {
		threads = 4
	}
	args := []string{
		"--defaults-extra-file=" + cnf,
		"--backup",
		"--stream=xbstream",
		"--target-dir=" + tmpTarget,
		"--extra-lsndir=" + lsnDir,
		"--parallel=" + strconv.Itoa(threads),
	}
	if spec.Server.Datadir != "" {
		args = append(args, "--datadir="+spec.Server.Datadir)
	}
	if o.FTWRLWaitTimeout > 0 {
		// wait for long queries to finish instead of blocking all writers
		args = append(args, "--ftwrl-wait-timeout="+strconv.Itoa(o.FTWRLWaitTimeout))
	}
	if o.SlaveInfo {
		args = append(args, "--slave-info")
	}
	if o.SafeSlaveBackup {
		args = append(args, "--safe-slave-backup")
	}
	incremental := spec.IncrementalLSN != ""
	if incremental {
		args = append(args, "--incremental-lsn="+spec.IncrementalLSN)
		env.Log.Printf("Incremental from LSN %s", spec.IncrementalLSN)
	}
	if o.ExtraArgs != "" {
		args = append(args, execx.SplitArgs(o.ExtraArgs)...)
	}
	w, err := sink.Create("backup.xbstream", FileOpts{Kind: "xbstream", Compress: true})
	if err != nil {
		return nil, err
	}
	errTail := newTail(4096)
	cmd := env.command(tool.Path, args, extraEnv)
	cmd.Stdout = w
	cmd.Stderr = errTail
	runErr := env.runCmd(cmd, "["+bname+"] ")
	if runErr == nil && !strings.Contains(errTail.String(), "completed OK") {
		runErr = errors.New(bname + " did not report 'completed OK!' — the backup is invalid")
	}
	if err := finish(w, runErr); err != nil {
		return nil, err
	}
	res := &Result{Meta: map[string]any{"tool": tool.Version}, Databases: toEntries(spec.Databases)}
	cp := readFirst(lsnDir, "xtrabackup_checkpoints", "mariadb_backup_checkpoints")
	if cp == "" {
		return nil, errors.New("could not read the checkpoints file (extra-lsndir)")
	}
	kv := parseKV(cp)
	res.Meta["backup_type"] = kv["backup_type"]
	res.Meta["from_lsn"] = kv["from_lsn"]
	res.Meta["to_lsn"] = kv["to_lsn"]
	res.Meta["last_lsn"] = kv["last_lsn"]
	if info := readFirst(lsnDir, "xtrabackup_info", "mariadb_backup_info"); info != "" {
		ik := parseKV(info)
		if v := ik["binlog_pos"]; v != "" {
			res.Meta["binlog_info"] = v
		}
		if v := ik["tool_version"]; v != "" {
			res.Meta["tool_version"] = v
		}
	}
	if bi := readFirst(lsnDir, "xtrabackup_binlog_info", "mariadb_backup_binlog_info"); bi != "" {
		f := strings.Fields(bi)
		if len(f) >= 2 {
			res.Meta["binlog_file"] = f[0]
			if p, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				res.Meta["binlog_pos"] = p
			}
			if len(f) >= 3 {
				res.Meta["gtid"] = strings.Join(f[2:], " ")
			}
		}
	}
	env.Log.Printf("LSN: from=%s to=%s", kv["from_lsn"], kv["to_lsn"])
	return res, nil
}

func readFirst(dir string, names ...string) string {
	for _, n := range names {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			return string(b)
		}
	}
	return ""
}

func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(ln, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// ExtractXbstream unpacks an xbstream from r into dir.
func ExtractXbstream(env *Env, flavor string, r io.Reader, dir string, threads int) error {
	_, sname := PhysicalToolNames(flavor)
	tool := env.Tools.Get(sname)
	if err := requireTool(tool); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	args := []string{"-x", "-C", dir}
	if threads > 1 && env.Tools.HasFlag(sname, "parallel") {
		args = append(args, "--parallel="+strconv.Itoa(threads))
	}
	cmd := env.command(tool.Path, args, nil)
	cmd.Stdin = r
	return env.runCmd(cmd, "["+sname+"] ")
}

// Prepare applies redo logs. For incremental chains call with incDir set.
func Prepare(env *Env, flavor, fullDir, incDir string, applyLogOnly bool, useMemory string) error {
	bname, _ := PhysicalToolNames(flavor)
	tool := env.Tools.Get(bname)
	if err := requireTool(tool); err != nil {
		return err
	}
	args := []string{"--prepare", "--target-dir=" + fullDir}
	if incDir != "" {
		args = append(args, "--incremental-dir="+incDir)
	}
	// MariaBackup >= 10.2 must not use --apply-log-only.
	if applyLogOnly && bname == "xtrabackup" {
		args = append(args, "--apply-log-only")
	}
	if useMemory != "" {
		args = append(args, "--use-memory="+useMemory)
	}
	cmd := env.command(tool.Path, args, nil)
	errTail := newTail(4096)
	cmd.Stderr = errTail
	if err := env.runCmd(cmd, "["+bname+" prepare] "); err != nil {
		return err
	}
	if !strings.Contains(errTail.String(), "completed OK") {
		return fmt.Errorf("%s --prepare did not report 'completed OK!'", bname)
	}
	return nil
}

// CopyBack copies (or moves) a prepared backup into an empty datadir.
func CopyBack(env *Env, flavor, fullDir, datadir string, move bool, threads int) error {
	bname, _ := PhysicalToolNames(flavor)
	tool := env.Tools.Get(bname)
	if err := requireTool(tool); err != nil {
		return err
	}
	mode := "--copy-back"
	if move {
		mode = "--move-back"
	}
	args := []string{mode, "--target-dir=" + fullDir, "--datadir=" + datadir}
	if threads > 1 {
		args = append(args, "--parallel="+strconv.Itoa(threads))
	}
	cmd := env.command(tool.Path, args, nil)
	return env.runCmd(cmd, "["+bname+" "+strings.TrimPrefix(mode, "--")+"] ")
}
