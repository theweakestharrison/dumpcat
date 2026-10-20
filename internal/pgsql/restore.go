package pgsql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"dumpcat/internal/engine"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Restore loads pg_dump archives into spec.Target: each database is
// extracted to the staging directory, created when missing and filled by
// pg_restore --jobs.
func (Driver) Restore(env *engine.Env, src engine.RestoreSource, spec *engine.RestoreSpec) error {
	if spec.Run.Method != store.MethodPgDump {
		return fmt.Errorf("this backup (%s) cannot be restored by the PostgreSQL driver", spec.Run.Method)
	}
	ctx, target := env.Ctx, spec.Target
	files := src.Files()
	dbs := spec.Databases
	if len(dbs) == 0 {
		return errors.New("no databases selected")
	}
	var check []store.BackupFile
	var total int64
	for _, d := range dbs {
		f, ok := files[pipeline.StripExt(d.File)]
		if !ok {
			return fmt.Errorf("file for database %s not found", d.Name)
		}
		check = append(check, f)
		total += f.Size
	}
	if f, ok := files[GlobalsFile]; ok && spec.RestoreUsers {
		check = append(check, f)
	}
	// pg_restore reads archives of its own version and older ones
	dumpMajor := metaInt(spec.Run.Meta["pg_dump_major"])
	tool, err := env.Tools.PGFor("pg_restore", max(dumpMajor, spec.Info.Major))
	if err != nil && dumpMajor > 0 {
		tool, err = env.Tools.PGFor("pg_restore", dumpMajor)
	}
	if err != nil {
		return err
	}
	env.Log.Printf("Source: %s → server %s (PostgreSQL %s)", src.Describe(), target.Name, spec.Info.Version)
	env.Log.Printf("Tool: %s (%s)", tool.Path, tool.Version)
	// nothing is dropped before every file proved readable with this passphrase
	src.SetPhase("prepare")
	if err := src.Preflight(ctx, check); err != nil {
		return err
	}
	pwEnv, err := WritePassFile(env.TmpDir, target)
	if err != nil {
		return err
	}
	if spec.RestoreUsers {
		// roles first: the objects name them as owners and in GRANTs
		if err := restoreGlobals(env, src, spec, files); err != nil {
			return err
		}
	}
	threads := spec.Threads
	if threads <= 0 {
		threads = 4
	}
	src.SetPhase("restore")
	src.SetEstimate(total)
	env.Log.Printf("Restoring %d database(s) (%s compressed), pg_restore --jobs=%d", len(dbs), engine.HumanBytes(total), threads)
	for i, d := range dbs {
		f := files[pipeline.StripExt(d.File)]
		name := spec.TargetName(d.Name)
		t0 := time.Now()
		dir := filepath.Join(env.TmpDir, "restore-"+strconv.Itoa(i))
		env.Log.Printf("Downloading and extracting %s (%s)", f.Name, engine.HumanBytes(f.Size))
		rd, done, err := src.Open(ctx, f)
		if err != nil {
			return err
		}
		n, err := engine.UntarTo(rd, dir)
		if derr := done(err == nil); err == nil && derr != nil {
			return fmt.Errorf("%s: %w", d.Name, derr) // not the file that was backed up
		}
		if err != nil {
			return fmt.Errorf("%s: %w", d.Name, err)
		}
		if n == 0 {
			return fmt.Errorf("%s: the archive is empty", d.Name)
		}
		existed, err := prepareDatabase(ctx, target, d, name, spec.DropExisting, env.Log)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		conn, err := ConnInfo(target, name, env.TmpDir)
		if err != nil {
			return err
		}
		args := []string{"--jobs=" + strconv.Itoa(threads), "--exit-on-error", "--no-password"}
		if existed {
			// like DROP TABLE IF EXISTS in a MySQL dump: the backup's objects
			// replace their namesakes, other objects stay
			args = append(args, "--clean", "--if-exists")
		}
		if spec.Opts.NoOwner {
			args = append(args, "--no-owner")
		}
		if spec.Opts.NoPrivileges {
			args = append(args, "--no-privileges")
		}
		args = append(args, "--dbname="+conn, "--", dir)
		env.Log.Printf("Restore %s → %s (%s)", d.Name, name, engine.HumanBytes(f.Size))
		cmd := env.Command(tool.Path, args, pwEnv)
		err = env.RunCmd(cmd, "[pg_restore] ")
		os.RemoveAll(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		env.Log.Printf("Done %s in %s", name, engine.HumanDuration(time.Since(t0)))
	}
	return nil
}

func metaInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

// prepareDatabase drops the target database when asked and creates it when
// missing, with the source's encoding and locale. existed tells whether it
// is still there from before.
func prepareDatabase(ctx context.Context, s *store.Server, d store.DBEntry, name string, drop bool, lg *tasklog.Logger) (existed bool, err error) {
	c, err := adminConn(ctx, s)
	if err != nil {
		return false, err
	}
	defer c.Close(context.Background())
	var one int
	switch err := c.QueryRow(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", name).Scan(&one); {
	case err == nil:
		existed = true
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	}
	if existed && drop {
		q := "DROP DATABASE " + QuoteIdent(name)
		lg.Printf("%s", q)
		if _, err := c.Exec(ctx, q); err != nil {
			return true, err
		}
		existed = false
	}
	if existed {
		return true, nil
	}
	base := "CREATE DATABASE " + QuoteIdent(name) + " TEMPLATE template0"
	if d.Charset != "" {
		base += " ENCODING " + QuoteLiteral(d.Charset)
	}
	tries := []string{base}
	if d.Collation != "" {
		tries = []string{base + " LC_COLLATE " + QuoteLiteral(d.Collation) + " LC_CTYPE " + QuoteLiteral(d.Collation), base, base + " LC_COLLATE 'C' LC_CTYPE 'C'"}
	}
	for i, q := range tries {
		lg.Printf("%s", q)
		_, err = c.Exec(ctx, q)
		if err == nil {
			if i > 0 {
				lg.Printf("NOTE: %s is created without the source's locale %s (the target does not know it): text sorts in the target's order", name, d.Collation)
			}
			return false, nil
		}
		if ctx.Err() != nil {
			return false, err
		}
		lg.Printf("%v", err)
	}
	return false, err
}
