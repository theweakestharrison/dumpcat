package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"dumpcat/internal/execx"
)

// PhysicalOptions are the settings of a physical backup that do not depend on
// where the tool runs. They are part of the agent protocol: an agent started
// with --allow-physical receives them and builds the same command line.
type PhysicalOptions struct {
	Tool             string   `json:"tool"` // mariabackup or xtrabackup
	Datadir          string   `json:"datadir,omitempty"`
	Parallel         int      `json:"parallel,omitempty"`
	IncrementalLSN   string   `json:"incremental_lsn,omitempty"`
	FTWRLWaitTimeout int      `json:"ftwrl_wait_timeout,omitempty"`
	SlaveInfo        bool     `json:"slave_info,omitempty"`
	SafeSlaveBackup  bool     `json:"safe_slave_backup,omitempty"`
	ExtraArgs        []string `json:"extra_args,omitempty"`
	LowPriority      bool     `json:"low_priority,omitempty"`
}

// PhysicalOptionsFor returns the options of a physical backup job.
func PhysicalOptionsFor(spec *BackupSpec, lowPriority bool) PhysicalOptions {
	bname, _ := PhysicalToolNames(spec.Flavor)
	o := spec.Job.Options
	threads := spec.Job.Threads
	if threads <= 0 {
		threads = 4
	}
	po := PhysicalOptions{Tool: bname, Datadir: spec.Server.Datadir, Parallel: threads, IncrementalLSN: spec.IncrementalLSN,
		FTWRLWaitTimeout: o.FTWRLWaitTimeout, SlaveInfo: o.SlaveInfo, SafeSlaveBackup: o.SafeSlaveBackup, LowPriority: lowPriority}
	if o.ExtraArgs != "" {
		po.ExtraArgs = execx.SplitArgs(o.ExtraArgs)
	}
	return po
}

// ToolGroups are the option-file groups the backup tool reads.
func (o PhysicalOptions) ToolGroups() []string {
	if o.Tool == "mariabackup" {
		return []string{"mariabackup", "mariadb-backup"}
	}
	return []string{"xtrabackup"}
}

// Args returns the backup command line after --defaults-extra-file (which
// must come first): stream to stdout, checkpoints into lsnDir.
func (o PhysicalOptions) Args(tmpTarget, lsnDir string) []string {
	args := []string{
		"--backup",
		"--stream=xbstream",
		"--target-dir=" + tmpTarget,
		"--extra-lsndir=" + lsnDir,
		"--parallel=" + strconv.Itoa(max(o.Parallel, 1)),
	}
	if o.Datadir != "" {
		args = append(args, "--datadir="+o.Datadir)
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
	if o.IncrementalLSN != "" {
		args = append(args, "--incremental-lsn="+o.IncrementalLSN)
	}
	return append(args, o.ExtraArgs...)
}

// PhysicalCheckpointFiles are the files --extra-lsndir receives: LSNs,
// binlog position and tool version of the finished backup.
var PhysicalCheckpointFiles = []string{
	"xtrabackup_checkpoints", "mariadb_backup_checkpoints",
	"xtrabackup_info", "mariadb_backup_info",
	"xtrabackup_binlog_info", "mariadb_backup_binlog_info",
}

// RemotePhysical runs the backup tool on the database host (through its
// agent): stdout receives the xbstream, stderr the tool's log. It returns the
// checkpoint files and the tool's version line.
type RemotePhysical func(ctx context.Context, o PhysicalOptions, stdout, stderr io.Writer) (files map[string]string, tool string, err error)

// Physical runs a hot, non-blocking physical backup with Percona XtraBackup
// (MySQL/Percona) or MariaBackup (MariaDB), streamed as xbstream.
// InnoDB is copied without locks; backup locks / BACKUP STAGE are used only
// for the short final phase (non-InnoDB tables + binlog position).
// With spec.Remote set, the tool runs on the database host instead.
func Physical(env *Env, spec *BackupSpec, sink Sink) (*Result, error) {
	po := PhysicalOptionsFor(spec, env.LowPriority)
	if po.IncrementalLSN != "" {
		env.Log.Printf("Incremental from LSN %s", po.IncrementalLSN)
	}
	if spec.Remote != nil {
		return physicalRemote(env, spec, po, sink)
	}
	bname := po.Tool
	tool := env.Tools.Get(bname)
	if err := requireTool(tool); err != nil {
		return nil, err
	}
	env.Log.Printf("Tool: %s (%s)", tool.Path, tool.Version)
	cnf, extraEnv, err := WriteCnf(env.TmpDir, spec.Server, CnfOptions{Groups: po.ToolGroups()})
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
	args := append([]string{"--defaults-extra-file=" + cnf}, po.Args(tmpTarget, lsnDir)...)
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
	return physicalResult(env, spec, tool.Version, ReadCheckpoints(lsnDir))
}

// ReadCheckpoints reads the checkpoint files of a finished backup.
func ReadCheckpoints(lsnDir string) map[string]string {
	files := map[string]string{}
	for _, n := range PhysicalCheckpointFiles {
		if b, err := os.ReadFile(filepath.Join(lsnDir, n)); err == nil {
			files[n] = string(b)
		}
	}
	return files
}

// physicalRemote stores the backup the agent runs on the database host
// exactly like the output of a local tool.
func physicalRemote(env *Env, spec *BackupSpec, po PhysicalOptions, sink Sink) (*Result, error) {
	env.Log.Printf("$ %s (on the database host)", execx.Quote(po.Tool, po.Args("<tmp>", "<tmp>")))
	w, err := sink.Create("backup.xbstream", FileOpts{Kind: "xbstream", Compress: true})
	if err != nil {
		return nil, err
	}
	lw := env.Log.Writer("[" + po.Tool + "] ")
	errTail := newTail(4096)
	files, version, runErr := spec.Remote(env.Ctx, po, w, io.MultiWriter(lw, errTail))
	lw.Close()
	switch {
	case runErr != nil && env.Ctx.Err() != nil:
		runErr = env.Ctx.Err()
	case runErr != nil:
		if msg := lastLines(errTail.String(), 4); msg != "" {
			runErr = fmt.Errorf("%s: %w: %s", po.Tool, runErr, msg)
		}
	case !strings.Contains(errTail.String(), "completed OK"):
		runErr = errors.New(po.Tool + " did not report 'completed OK!' — the backup is invalid")
	}
	if err := finish(w, runErr); err != nil {
		return nil, err
	}
	return physicalResult(env, spec, version, files)
}

// physicalResult reads the LSNs and the binlog position from the checkpoint files.
func physicalResult(env *Env, spec *BackupSpec, toolVersion string, files map[string]string) (*Result, error) {
	first := func(names ...string) string {
		for _, n := range names {
			if v, ok := files[n]; ok {
				return v
			}
		}
		return ""
	}
	res := &Result{Meta: map[string]any{"tool": toolVersion}, Databases: toEntries(spec.Databases)}
	cp := first("xtrabackup_checkpoints", "mariadb_backup_checkpoints")
	if cp == "" {
		return nil, errors.New("could not read the checkpoints file (extra-lsndir)")
	}
	kv := parseKV(cp)
	res.Meta["backup_type"] = kv["backup_type"]
	res.Meta["from_lsn"] = kv["from_lsn"]
	res.Meta["to_lsn"] = kv["to_lsn"]
	res.Meta["last_lsn"] = kv["last_lsn"]
	if info := first("xtrabackup_info", "mariadb_backup_info"); info != "" {
		ik := parseKV(info)
		if v := ik["binlog_pos"]; v != "" {
			res.Meta["binlog_info"] = v
		}
		if v := ik["tool_version"]; v != "" {
			res.Meta["tool_version"] = v
		}
	}
	if bi := first("xtrabackup_binlog_info", "mariadb_backup_binlog_info"); bi != "" {
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
	} else if m := infoBinlogPos.FindStringSubmatch(first("xtrabackup_info", "mariadb_backup_info")); m != nil {
		// --extra-lsndir gets the checkpoints and the info file, not *_binlog_info:
		// the info file's binlog_pos line carries the same coordinates
		res.Meta["binlog_file"] = m[1]
		if p, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			res.Meta["binlog_pos"] = p
		}
		if g := strings.Join(strings.Fields(m[3]), ""); g != "" {
			res.Meta["gtid"] = g
		}
	}
	env.Log.Printf("LSN: from=%s to=%s", kv["from_lsn"], kv["to_lsn"])
	return res, nil
}

// infoBinlogPos reads the binlog_pos line of xtrabackup_info / mariadb_backup_info:
//
//	binlog_pos = filename 'mysql-bin.000007', position '1234', GTID of the last change '0-1-55'
//
// A MySQL GTID set may span lines.
var infoBinlogPos = regexp.MustCompile(`binlog_pos = filename '([^']+)', position '(\d+)'(?:, GTID of the last change '([^']*)')?`)

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
