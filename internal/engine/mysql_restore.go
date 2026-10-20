package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
	"dumpcat/internal/tasklog"
)

// Restore loads a mysqldump or mydumper backup into spec.Target.
func (MySQL) Restore(env *Env, src RestoreSource, spec *RestoreSpec) error {
	flavor := Flavor(spec.Target, spec.Info)
	switch spec.Run.Method {
	case store.MethodMysqldump:
		return restoreMysqldump(env, src, spec, flavor)
	case store.MethodMydumper:
		return restoreMydumper(env, src, spec, flavor)
	}
	return errNoMethod
}

// CharsetRe: character set and collation names, written unquoted into CREATE DATABASE.
var CharsetRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// entry adapts a database's default collation to the target when
// collations are converted.
func (spec *RestoreSpec) entry(d store.DBEntry) store.DBEntry {
	if spec.Opts.ConvertCollations {
		d.Collation = PortableCollation(d.Collation)
	}
	return d
}

// CheckNoBinlog fails early when the restore user may not disable binary
// logging, so a "no binlog" restore never silently replicates.
func CheckNoBinlog(ctx context.Context, s *store.Server) error {
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
		return fmt.Errorf("the restore account is not allowed to disable binlog (requires SUPER / SYSTEM_VARIABLES_ADMIN / BINLOG ADMIN): %w", err)
	}
	return nil
}

func prepareDatabase(ctx context.Context, s *store.Server, d store.DBEntry, name string, drop, noBinlog bool, lg *tasklog.Logger) error {
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return err
	}
	defer db.Close()
	// a single session so that sql_log_bin=0 also covers DROP/CREATE DATABASE
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if noBinlog {
		if _, err := conn.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
			return err
		}
	}
	if drop {
		lg.Printf("DROP DATABASE IF EXISTS %s", dbconn.QuoteIdent(name))
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbconn.QuoteIdent(name)); err != nil {
			return err
		}
	}
	q := "CREATE DATABASE IF NOT EXISTS " + dbconn.QuoteIdent(name)
	// names from the catalog, written unquoted: only plain identifiers
	if d.Charset != "" && CharsetRe.MatchString(d.Charset) {
		q += " CHARACTER SET " + d.Charset
	}
	if d.Collation != "" && CharsetRe.MatchString(d.Collation) {
		q += " COLLATE " + d.Collation
	}
	_, err = conn.ExecContext(ctx, q)
	return err
}

func restoreMysqldump(env *Env, src RestoreSource, spec *RestoreSpec, flavor string) error {
	ctx, target := env.Ctx, spec.Target
	env.Log.Printf("Source: %s → server %s (%s)", src.Describe(), target.Name, flavor)
	files := src.Files()
	var total int64
	if spec.NoBinlog {
		if err := CheckNoBinlog(ctx, target); err != nil {
			return err
		}
	}
	single := spec.Run.Meta["single_file"] == true
	dbs := spec.Databases
	var check []store.BackupFile
	if single {
		f, ok := files["all-databases.sql"]
		if !ok {
			return errors.New("all-databases.sql not found in the backup")
		}
		check = append(check, f)
	} else {
		if len(dbs) == 0 {
			return errors.New("no databases selected")
		}
		for _, d := range dbs {
			f, ok := files[pipeline.StripExt(d.File)]
			if !ok {
				return fmt.Errorf("file for database %s not found", d.Name)
			}
			check = append(check, f)
		}
	}
	if f, ok := files["_users.sql"]; ok && spec.RestoreUsers {
		check = append(check, f)
	}
	// nothing is dropped before every file proved readable with this passphrase
	src.SetPhase("prepare")
	if err := src.Preflight(ctx, check); err != nil {
		return err
	}
	if spec.FastMode != nil {
		defer spec.FastMode()()
	}
	src.SetPhase("restore")
	if single {
		f := files["all-databases.sql"]
		src.SetEstimate(f.Size)
		if spec.DropExisting {
			for _, d := range spec.Run.Databases {
				if err := prepareDatabase(ctx, target, spec.entry(d), d.Name, true, spec.NoBinlog, env.Log); err != nil {
					return err
				}
			}
		}
		env.Log.Printf("Restore file %s (%s)", f.Name, HumanBytes(f.Size))
		rd, done, err := src.Open(ctx, f)
		if err != nil {
			return err
		}
		err = RestoreSQL(env, target, flavor, PortableSQL(rd, spec.Opts.Portable()), SQLRestoreOpts{NoBinlog: spec.NoBinlog})
		if derr := done(err == nil); err == nil && derr != nil {
			return derr // the file is not the one that was backed up: the restore must not pass
		}
		if err != nil {
			return err
		}
	} else {
		for _, d := range dbs {
			if f, ok := files[pipeline.StripExt(d.File)]; ok {
				total += f.Size
			}
		}
		src.SetEstimate(total)
		threads := spec.Threads
		if threads <= 0 {
			threads = 1
		}
		env.Log.Printf("Restoring %d database(s) (%s compressed), %d in parallel", len(dbs), HumanBytes(total), threads)
		err := Parallel(ctx, threads, dbs, func(ctx context.Context, d store.DBEntry) error {
			f, ok := files[pipeline.StripExt(d.File)]
			if !ok {
				return fmt.Errorf("file for database %s not found", d.Name)
			}
			name := spec.TargetName(d.Name)
			if err := prepareDatabase(ctx, target, spec.entry(d), name, spec.DropExisting, spec.NoBinlog, env.Log); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			env.Log.Printf("Restore %s → %s (%s)", d.Name, name, HumanBytes(f.Size))
			t0 := time.Now()
			rd, done, err := src.Open(ctx, f)
			if err != nil {
				return err
			}
			e2 := *env
			e2.Ctx = ctx
			popts := spec.Opts.Portable()
			popts.RenameFrom, popts.RenameTo = d.Name, name // definitions naming the source database follow the rename
			err = RestoreSQL(&e2, target, flavor, PortableSQL(rd, popts), SQLRestoreOpts{Database: name, NoBinlog: spec.NoBinlog})
			if derr := done(err == nil); err == nil && derr != nil {
				return fmt.Errorf("%s: %w", name, derr)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			env.Log.Printf("Done %s in %s", name, HumanDuration(time.Since(t0)))
			return nil
		})
		if err != nil {
			return err
		}
	}
	return restoreUsers(env, src, spec, flavor, files)
}

func restoreUsers(env *Env, src RestoreSource, spec *RestoreSpec, flavor string, files map[string]store.BackupFile) error {
	if !spec.RestoreUsers {
		return nil
	}
	f, ok := files["_users.sql"]
	if !ok {
		env.Log.Printf("The backup contains no users/grants — skipping")
		return nil
	}
	env.Log.Printf("Restoring users & grants (--force, errors in individual statements are ignored)")
	rd, done, err := src.Open(env.Ctx, f)
	if err != nil {
		return err
	}
	err = RestoreSQL(env, spec.Target, flavor, rd, SQLRestoreOpts{NoBinlog: spec.NoBinlog, Force: true})
	if derr := done(err == nil); err == nil && derr != nil {
		return derr
	}
	if err != nil {
		env.Log.Printf("Some users/grants statements failed: %v", err)
	}
	return nil
}

func restoreMydumper(env *Env, src RestoreSource, spec *RestoreSpec, flavor string) error {
	ctx, target, workDir := env.Ctx, spec.Target, env.TmpDir
	files := src.Files()
	f, ok := files["mydumper.tar"]
	if !ok {
		return errors.New("mydumper.tar not found in the backup")
	}
	if spec.NoBinlog {
		if err := CheckNoBinlog(ctx, target); err != nil {
			return err
		}
	}
	dir := filepath.Join(workDir, "dump")
	if total, free, err := diskUsage(workDir); err == nil {
		env.Log.Printf("Staging directory %s: %s free / %s, needs ~%s", workDir, HumanBytes(free), HumanBytes(total), HumanBytes(f.Size))
	}
	src.SetPhase("download")
	src.SetEstimate(f.Size)
	env.Log.Printf("Downloading and extracting %s (%s) from %s", f.Name, HumanBytes(f.Size), src.Describe())
	rd, done, err := src.Open(ctx, f)
	if err != nil {
		return err
	}
	n, err := UntarTo(rd, dir)
	if derr := done(err == nil); err == nil && derr != nil {
		return derr
	}
	if err != nil {
		return err
	}
	env.Log.Printf("Extracted %d files", n)
	if o := spec.Opts.Portable(); o.Any() {
		n, err := RewriteMydumperSchemas(dir, o)
		if err != nil {
			return fmt.Errorf("rewriting the schema files: %w", err)
		}
		env.Log.Printf("Rewrote %d schema file(s) for the target (%s)", n, strings.Join(o.Labels(), ", "))
	}
	if n, err := RenameMydumperSchemas(dir, spec.Rename); err != nil {
		return fmt.Errorf("rewriting the schema files: %w", err)
	} else if n > 0 {
		env.Log.Printf("Pointed %d schema file(s) at the renamed database(s)", n)
	}
	if spec.FastMode != nil {
		defer spec.FastMode()()
	}
	src.SetPhase("restore")
	threads := spec.Threads
	if threads <= 0 {
		threads = 4
	}
	dbs := spec.Databases
	all := len(dbs) == len(spec.Run.Databases) && !spec.Renamed()
	for _, d := range dbs {
		name := spec.TargetName(d.Name)
		if spec.DropExisting {
			if err := prepareDatabase(ctx, target, spec.entry(d), name, true, spec.NoBinlog, env.Log); err != nil {
				return err
			}
		}
	}
	base := MyloaderOpts{Dir: dir, Threads: threads, Overwrite: spec.DropExisting, EnableBinlog: !spec.NoBinlog, OptimizeKeys: true}
	if all {
		env.Log.Printf("myloader: all %d database(s), %d threads", len(dbs), threads)
		if err := Myloader(env, target, base); err != nil {
			return err
		}
	} else {
		for _, d := range dbs {
			o := base
			o.SourceDB = d.Name
			o.TargetDB = spec.TargetName(d.Name)
			if !spec.DropExisting {
				if err := prepareDatabase(ctx, target, spec.entry(d), o.TargetDB, false, spec.NoBinlog, env.Log); err != nil {
					return err
				}
			}
			env.Log.Printf("myloader %s → %s, %d threads", d.Name, o.TargetDB, threads)
			if err := Myloader(env, target, o); err != nil {
				return err
			}
		}
	}
	os.RemoveAll(dir)
	return restoreUsers(env, src, spec, flavor, files)
}

func diskUsage(p string) (int64, int64, error) {
	_ = os.MkdirAll(p, 0o750)
	return sysinfo.DiskUsage(p)
}

// HumanBytes formats a size for the logs (1.50 GiB).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// HumanDuration formats a duration for the logs (1h02m03s).
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
