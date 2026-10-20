package pgsql

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
)

// ArchiveSuffix ends the name of every database's archive in a backup: the
// pg_dump directory, as a tar stream.
const ArchiveSuffix = ".pgdump.tar"

// GlobalsFile holds pg_dumpall --globals-only (roles, tablespaces).
const GlobalsFile = "_globals.sql"

// Backup runs pg_dump --format=directory for each database, then stores
// each directory as one tar file in the sink, and the roles in _globals.sql.
func (Driver) Backup(env *engine.Env, spec *engine.BackupSpec, sink engine.Sink) (*engine.Result, error) {
	if spec.Job.Method != store.MethodPgDump {
		return nil, fmt.Errorf("unsupported method: %s", spec.Job.Method)
	}
	if len(spec.Databases) == 0 {
		return nil, errors.New("no databases to back up")
	}
	srvMajor := serverMajor(spec.Info, spec.ServerVersion)
	dump, err := env.Tools.PGFor("pg_dump", srvMajor)
	if err != nil {
		return nil, err
	}
	restore, err := env.Tools.PGFor("pg_restore", dump.Major)
	if err != nil {
		return nil, fmt.Errorf("pg_restore is needed to read the archives' table list: %w", err)
	}
	env.Log.Printf("Tool: %s (%s)", dump.Path, dump.Version)
	pwEnv, err := WritePassFile(env.TmpDir, spec.Server)
	if err != nil {
		return nil, err
	}
	o := spec.Job.Options
	threads := spec.Job.Threads
	if threads < 1 {
		threads = 1
	}
	res := &engine.Result{Meta: map[string]any{"tool": dump.Version, "pg_dump_major": dump.Major}, Databases: engine.ToEntries(spec.Databases)}
	counted := store.TableStats{}
	used := map[string]bool{}
	var latest time.Time
	for i, d := range spec.Databases {
		fn := engine.SafeFileName(d.Name)
		for k := 2; used[fn]; k++ {
			fn = engine.SafeFileName(d.Name) + "_" + strconv.Itoa(k)
		}
		used[fn] = true
		name := fn + ArchiveSuffix
		res.Databases[i].File = name
		dir := filepath.Join(env.TmpDir, "pgdump-"+strconv.Itoa(i))
		conn, err := ConnInfo(spec.Server, d.Name, env.TmpDir)
		if err != nil {
			return nil, err
		}
		args := []string{"--format=directory", "--jobs=" + strconv.Itoa(threads), "--compress=0", "--no-password", "--file=" + dir}
		for _, t := range tablesFor(spec.Job.ExcludeTables, d.Name) {
			args = append(args, "--exclude-table="+t)
		}
		if o.ExtraArgs != "" {
			args = append(args, execx.SplitArgs(o.ExtraArgs)...)
		}
		args = append(args, "--dbname="+conn)
		env.Log.Printf("Dumping database %s (%d job(s))", d.Name, threads)
		// the snapshot is taken when pg_dump starts: the data is not newer than this
		start := time.Now()
		cmd := env.Command(dump.Path, args, pwEnv)
		if err := env.RunCmd(cmd, "[pg_dump] "); err != nil {
			return nil, fmt.Errorf("database %s: %w", d.Name, err)
		}
		latest = start
		names, err := relationNames(env.Ctx, spec.Server, d.Name)
		if err != nil {
			env.Log.Printf("WARNING: database %s: could not read the table names (%v): they are taken from the archive's list", d.Name, err)
		}
		toc, err := tableEntries(env, restore.Path, dir, names)
		if err != nil {
			return nil, fmt.Errorf("database %s: reading the archive's table list: %w", d.Name, err)
		}
		w, err := sink.Create(name, engine.FileOpts{Kind: "pgdump", Database: d.Name, Compress: true})
		if err != nil {
			return nil, err
		}
		rows, err := tarDump(dir, w, toc)
		if err := engine.Finish(w, err); err != nil {
			return nil, fmt.Errorf("database %s: %w", d.Name, err)
		}
		os.RemoveAll(dir) // the staging disk holds one database at a time
		counted[d.Name] = rows
		res.Databases[i].Tables = len(rows)
		env.Log.Printf("Finished database %s: %d tables", d.Name, len(rows))
	}
	res.Meta["consistent_at"] = latest.Unix()
	res.SetRows(counted)
	if o.IncludeUsers {
		if err := dumpGlobals(env, spec, sink, pwEnv); err != nil {
			env.Log.Printf("WARNING: could not dump roles: %v", err)
		}
	}
	return res, nil
}

// tablesFor turns the job's excluded tables of db ("db.table" or
// "db.schema.table") into pg_dump --exclude-table patterns.
func tablesFor(excl []string, db string) []string {
	var out []string
	for _, e := range excl {
		e = strings.TrimSpace(e)
		d, t, ok := strings.Cut(e, ".")
		if ok && d == db && t != "" {
			out = append(out, t)
		}
	}
	return out
}

// reTOC matches a TABLE DATA line of pg_restore --list (dump id, the
// table's oid, then schema, table and owner unquoted):
// "4827; 0 16390 TABLE DATA public items shop_owner".
var reTOC = regexp.MustCompile(`^(\d+); \d+ (\d+) TABLE DATA (.*)$`)

// relationNames maps the oids of a database's tables to "schema.table".
func relationNames(ctx context.Context, s *store.Server, db string) (map[string]string, error) {
	c, err := Connect(ctx, s, db)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(ctx, `SELECT c.oid::text, n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r','p','f','m')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var oid, nsp, rel string
		if err := rows.Scan(&oid, &nsp, &rel); err != nil {
			return nil, err
		}
		out[oid] = nsp + "." + rel
	}
	return out, rows.Err()
}

// tableEntries maps the data files of a directory archive ("4827.dat") to
// their tables ("schema.table"), from the archive's table of contents.
func tableEntries(env *engine.Env, pgRestore, dir string, names map[string]string) (map[string]string, error) {
	var out bytes.Buffer
	cmd := env.Command(pgRestore, []string{"--list", "--", dir}, nil)
	cmd.Stdout = &out
	if err := env.RunCmd(cmd, "[pg_restore] "); err != nil {
		return nil, err
	}
	toc := map[string]string{}
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		m := reTOC.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		name, ok := names[m[2]]
		if !ok {
			// a table renamed since: the list prints "schema table owner"
			// unquoted, exact unless a name holds a space
			f := strings.Fields(m[3])
			if len(f) < 3 {
				continue
			}
			name = f[0] + "." + strings.Join(f[1:len(f)-1], " ")
		}
		toc[m[1]+".dat"] = name
	}
	return toc, sc.Err()
}

// tarDump writes the archive directory as a tar stream and counts the rows
// of every table data file (COPY text: one line per row, up to "\.").
func tarDump(dir string, w io.Writer, toc map[string]string) (map[string]int64, error) {
	rows := map[string]int64{}
	for _, t := range toc {
		rows[t] = 0
	}
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
		defer f.Close()
		table, counted := toc[hdr.Name]
		if !counted {
			_, err = io.Copy(tw, f)
			return err
		}
		lc := &lineCounter{}
		_, err = io.Copy(io.MultiWriter(tw, lc), f)
		rows[table] = lc.rows()
		return err
	})
	if err != nil {
		return nil, err
	}
	return rows, tw.Close()
}

// lineCounter counts the rows of a COPY text stream: lines before the
// terminating "\." line (a row never reads "\." since backslashes are
// escaped).
type lineCounter struct {
	n    int64
	line []byte // start of the current line, up to 3 bytes
	done bool
}

func (c *lineCounter) Write(p []byte) (int, error) {
	if c.done {
		return len(p), nil
	}
	for _, b := range p {
		if b != '\n' {
			if len(c.line) < 3 {
				c.line = append(c.line, b)
			}
			continue
		}
		if string(c.line) == `\.` || string(c.line) == "\\.\r" {
			c.done = true
			return len(p), nil
		}
		c.n++
		c.line = c.line[:0]
	}
	return len(p), nil
}

func (c *lineCounter) rows() int64 { return c.n }

// dumpGlobals writes pg_dumpall --globals-only into _globals.sql.
func dumpGlobals(env *engine.Env, spec *engine.BackupSpec, sink engine.Sink, pwEnv []string) error {
	tool, err := env.Tools.PGFor("pg_dumpall", serverMajor(spec.Info, spec.ServerVersion))
	if err != nil {
		return err
	}
	conn, err := ConnInfo(spec.Server, "postgres", env.TmpDir)
	if err != nil {
		return err
	}
	args := []string{"--globals-only", "--no-password"}
	super := false
	if spec.Info != nil {
		for _, g := range spec.Info.Grants {
			super = super || g == "SUPERUSER"
		}
	}
	if !super {
		args = append(args, "--no-role-passwords") // pg_authid is for superusers only
	}
	// the first database to connect to; pg_dumpall would read a name holding '=' as options
	if first := spec.Databases[0].Name; !strings.ContainsAny(first, "=") && !strings.HasPrefix(first, "-") &&
		!strings.HasPrefix(first, "postgres://") && !strings.HasPrefix(first, "postgresql://") {
		args = append(args, "--database="+first)
	}
	args = append(args, "--dbname="+conn)
	w, err := sink.Create(GlobalsFile, engine.FileOpts{Kind: "users", Compress: true})
	if err != nil {
		return err
	}
	tl := &tailBuf{}
	cmd := env.Command(tool.Path, args, pwEnv)
	cmd.Stdout = io.MultiWriter(w, tl)
	runErr := env.RunCmd(cmd, "[pg_dumpall] ")
	if runErr == nil && !strings.Contains(tl.String(), "dump complete") {
		runErr = errors.New("pg_dumpall did not complete (missing 'dump complete' line)")
	}
	if err := engine.Finish(w, runErr); err != nil {
		return err
	}
	n := strings.Count(tl.all.String(), "\nCREATE ROLE ")
	env.Log.Printf("Dumped %d role(s) (roles and tablespaces)", n)
	return nil
}

// tailBuf keeps the globals output (small) for the completion check.
type tailBuf struct{ all bytes.Buffer }

func (t *tailBuf) Write(p []byte) (int, error) { return t.all.Write(p) }
func (t *tailBuf) String() string {
	b := t.all.Bytes()
	if len(b) > 512 {
		b = b[len(b)-512:]
	}
	return string(b)
}
