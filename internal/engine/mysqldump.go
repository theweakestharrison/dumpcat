package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/execx"
)

var (
	reChangeMaster = regexp.MustCompile(`(?:MASTER|SOURCE)_LOG_FILE='([^']+)',\s*(?:MASTER|SOURCE)_LOG_POS=(\d+)`)
	reGtidPurged   = regexp.MustCompile(`GTID_PURGED='([^']*)'`)
	reMariaGtid    = regexp.MustCompile(`gtid_slave_pos='([^']*)'`)
)

// mysqldumpArgs builds the common non-locking dump options.
func mysqldumpArgs(env *Env, toolName, cnf string, spec *BackupSpec) []string {
	o := spec.Job.Options
	args := []string{
		"--defaults-extra-file=" + cnf,
		"--single-transaction", // consistent InnoDB snapshot without locking
		"--quick",              // stream rows, constant memory
		"--skip-lock-tables",
		"--hex-blob",
		"--default-character-set=utf8mb4",
		"--max-allowed-packet=1G",
		"--no-tablespaces",
	}
	if o.Routines {
		args = append(args, "--routines")
	}
	if o.Triggers {
		args = append(args, "--triggers")
	} else {
		args = append(args, "--skip-triggers")
	}
	if o.Events {
		args = append(args, "--events")
	}
	t := env.Tools
	if t.HasFlag(toolName, "column-statistics") {
		args = append(args, "--column-statistics=0")
	}
	if t.HasFlag(toolName, "set-gtid-purged") {
		v := strings.ToUpper(o.GTIDPurged)
		if v != "ON" && v != "AUTO" {
			v = "OFF"
		}
		args = append(args, "--set-gtid-purged="+v)
	}
	if o.RecordBinlog {
		switch {
		case t.HasFlag(toolName, "source-data"):
			args = append(args, "--source-data=2")
		case t.HasFlag(toolName, "master-data"):
			args = append(args, "--master-data=2")
		}
		if spec.Flavor == "mariadb" && t.HasFlag(toolName, "gtid") {
			args = append(args, "--gtid")
		}
	}
	if o.ExtraArgs != "" {
		args = append(args, execx.SplitArgs(o.ExtraArgs)...)
	}
	return args
}

// Mysqldump runs mysqldump/mariadb-dump: one file per database (restorable
// individually and under another name) or a single cross-database file.
func Mysqldump(env *Env, spec *BackupSpec, sink Sink) (*Result, error) {
	toolName := DumpToolName(spec.Flavor)
	tool := env.Tools.Get(toolName)
	if err := requireTool(tool); err != nil {
		return nil, err
	}
	env.Log.Printf("Tool: %s (%s)", tool.Path, tool.Version)
	cnf, extraEnv, err := WriteCnf(env.TmpDir, spec.Server, CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: tool.MariaDB})
	if err != nil {
		return nil, err
	}
	base := mysqldumpArgs(env, toolName, cnf, spec)
	res := &Result{Meta: map[string]any{"tool": tool.Version}, Databases: toEntries(spec.Databases)}
	if len(spec.Databases) == 0 {
		return nil, errors.New("no databases to back up")
	}

	// The binlog coordinates are printed right after the snapshot is taken:
	// the moment they appear is an upper bound of the snapshot time, which
	// point-in-time recovery uses to pick a base backup.
	dumpOne := func(ctx context.Context, fileName string, dbArgs []string, dbName string) (*head, error) {
		// bind the process to the worker context so a failing sibling stops it
		we := *env
		we.Ctx = ctx
		env := &we
		args := append(append([]string{}, base...), dbArgs...)
		w, err := sink.Create(fileName, FileOpts{Kind: "sql", Database: dbName, Compress: true})
		if err != nil {
			return nil, err
		}
		hd := &head{max: 64 * 1024, mark: []byte("_LOG_POS=")}
		tl := newTail(512)
		cmd := env.command(tool.Path, args, extraEnv)
		cmd.Stdout = io.MultiWriter(w, hd, tl)
		runErr := env.runCmd(cmd, "["+toolName+"] ")
		if runErr == nil && !strings.Contains(tl.String(), "Dump completed") {
			runErr = fmt.Errorf("dump %s did not complete (missing 'Dump completed' line)", fileName)
		}
		if err := finish(w, runErr); err != nil {
			return nil, err
		}
		return hd, nil
	}

	o := spec.Job.Options
	if o.SingleFile {
		var args []string
		for _, d := range spec.Databases {
			for _, t := range tablesFor(spec.Job.ExcludeTables, d.Name) {
				args = append(args, "--ignore-table="+d.Name+"."+t)
			}
		}
		args = append(args, "--databases", "--") // names after "--" are never options
		for _, d := range spec.Databases {
			args = append(args, d.Name)
		}
		env.Log.Printf("Dumping %d database(s) into one consistent file (single snapshot)", len(spec.Databases))
		hd, err := dumpOne(env.Ctx, "all-databases.sql", args, "")
		if err != nil {
			return nil, err
		}
		for i := range res.Databases {
			res.Databases[i].File = "all-databases.sql"
		}
		res.Meta["single_file"] = true
		parseBinlogHeader(hd.String(), res.Meta)
		if t := hd.MarkedAt(); !t.IsZero() {
			res.Meta["consistent_at"] = t.Unix()
		}
	} else {
		n := spec.Job.Threads
		if n <= 0 {
			n = 1
		}
		if n > len(spec.Databases) {
			n = len(spec.Databases)
		}
		if n > 1 {
			env.Log.Printf("Dumping %d databases, %d in parallel", len(spec.Databases), n)
		}
		used := map[string]bool{}
		names := make([]string, len(spec.Databases))
		for i, d := range spec.Databases {
			fn := SafeFileName(d.Name)
			for k := 2; used[fn]; k++ {
				fn = SafeFileName(d.Name) + "_" + strconv.Itoa(k)
			}
			used[fn] = true
			names[i] = fn + ".sql"
			res.Databases[i].File = names[i]
		}
		type item struct {
			idx int
			db  dbconn.DBInfo
		}
		items := make([]item, len(spec.Databases))
		for i, d := range spec.Databases {
			items[i] = item{i, d}
		}
		heads := make([]*head, len(items))
		err := Parallel(env.Ctx, n, items, func(ctx context.Context, it item) error {
			var args []string
			for _, t := range tablesFor(spec.Job.ExcludeTables, it.db.Name) {
				args = append(args, "--ignore-table="+it.db.Name+"."+t)
			}
			// No --databases: the dump has no CREATE DATABASE/USE, so it can be
			// restored into a database with a different name.
			args = append(args, "--", it.db.Name)
			env.Log.Printf("Dumping database %s", it.db.Name)
			hd, err := dumpOne(ctx, names[it.idx], args, it.db.Name)
			if err != nil {
				return fmt.Errorf("database %s: %w", it.db.Name, err)
			}
			heads[it.idx] = hd
			env.Log.Printf("Finished database %s", it.db.Name)
			return nil
		})
		if err != nil {
			return nil, err
		}
		parseBinlogHeader(heads[0].String(), res.Meta)
		// every database has its own snapshot and therefore its own position
		var latest time.Time
		for i, hd := range heads {
			m := map[string]any{}
			parseBinlogHeader(hd.String(), m)
			if f, ok := m["binlog_file"].(string); ok {
				res.Databases[i].BinlogFile = f
				res.Databases[i].BinlogPos, _ = m["binlog_pos"].(int64)
			}
			if t := hd.MarkedAt(); t.After(latest) {
				latest = t
			}
		}
		if !latest.IsZero() {
			res.Meta["consistent_at"] = latest.Unix()
		}
	}

	if o.IncludeUsers {
		if err := dumpUsers(env, spec, sink); err != nil {
			env.Log.Printf("WARNING: could not dump users/grants: %v", err)
		}
	}
	return res, nil
}

func parseBinlogHeader(hd string, meta map[string]any) {
	if m := grepFirst(reChangeMaster, hd); m != nil {
		meta["binlog_file"] = m[1]
		if p, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			meta["binlog_pos"] = p
		}
	}
	if m := grepFirst(reGtidPurged, hd); m != nil {
		meta["gtid"] = m[1]
	} else if m := grepFirst(reMariaGtid, hd); m != nil {
		meta["gtid"] = m[1]
	}
}

func dumpUsers(env *Env, spec *BackupSpec, sink Sink) error {
	sql, n, err := dbconn.DumpUsers(env.Ctx, spec.Server)
	if err != nil {
		return err
	}
	w, err := sink.Create("_users.sql", FileOpts{Kind: "users", Compress: true})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, sql)
	if err := finish(w, err); err != nil {
		return err
	}
	env.Log.Printf("Dumped %d accounts (users & grants)", n)
	return nil
}
