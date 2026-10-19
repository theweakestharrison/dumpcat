package engine

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"dumpcat/internal/store"
)

type SQLRestoreOpts struct {
	Database string // target database; empty when the dump contains USE statements
	NoBinlog bool   // SET sql_log_bin=0 (needs SUPER / SYSTEM_VARIABLES_ADMIN)
	Force    bool   // continue on SQL errors (used for users/grants)
}

const sandboxPrefix = "/*M!999999\\- enable the sandbox mode"

// stripSandbox removes the MariaDB >= 10.5.25/11.x "sandbox mode" first
// line which the MySQL client does not understand.
func stripSandbox(r io.Reader) io.Reader {
	br := bufio.NewReaderSize(r, 64*1024)
	peek, _ := br.Peek(len(sandboxPrefix))
	if string(peek) == sandboxPrefix {
		_, _ = br.ReadString('\n')
	}
	return br
}

// RestoreSQL pipes an SQL stream into the mysql/mariadb client.
func RestoreSQL(env *Env, target *store.Server, flavor string, r io.Reader, o SQLRestoreOpts) error {
	name := ClientToolName(flavor)
	tool := env.Tools.Get(name)
	if err := requireTool(tool); err != nil {
		return err
	}
	cnf, extraEnv, err := WriteCnf(env.TmpDir, target, CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: tool.MariaDB})
	if err != nil {
		return err
	}
	args := []string{
		"--defaults-extra-file=" + cnf,
		"--default-character-set=utf8mb4",
		"--max-allowed-packet=1G",
		"--binary-mode",    // no client commands (\!, source, tee) from the dump
		"--local-infile=0", // and no LOAD DATA LOCAL reading files of this host
		"--batch",
	}
	if o.Force {
		args = append(args, "--force")
	}
	if o.Database != "" {
		args = append(args, "--", o.Database)
	}
	var prelude strings.Builder
	if o.NoBinlog {
		prelude.WriteString("SET SESSION sql_log_bin=0;\n")
	}
	prelude.WriteString("SET SESSION foreign_key_checks=0;\nSET SESSION unique_checks=0;\n")
	in := r
	if !tool.MariaDB {
		in = stripSandbox(r)
	}
	cmd := env.command(tool.Path, args, extraEnv)
	cmd.Stdin = io.MultiReader(strings.NewReader(prelude.String()), in)
	return env.runCmd(cmd, "["+name+"] ")
}

type MyloaderOpts struct {
	Dir          string
	SourceDB     string
	TargetDB     string
	Threads      int
	Overwrite    bool
	EnableBinlog bool
	OptimizeKeys bool
}

// Myloader restores a mydumper directory in parallel.
func Myloader(env *Env, target *store.Server, o MyloaderOpts) error {
	tool := env.Tools.Get("myloader")
	if err := requireTool(tool); err != nil {
		return err
	}
	cnf, extraEnv, err := WriteCnf(env.TmpDir, target, CnfOptions{GlibGroups: []string{"myloader"}})
	if err != nil {
		return err
	}
	h := env.Tools.Help("myloader")
	has := func(f string) bool { return strings.Contains(h, "--"+f) }
	args := []string{}
	if has("defaults-file") {
		args = append(args, "--defaults-file="+cnf)
	} else {
		args = append(args, "--user="+target.Username)
		host, port, socket, err := target.Addr()
		if err != nil {
			return err
		}
		if socket != "" {
			args = append(args, "--socket="+socket)
		} else {
			args = append(args, "--host="+host, "--port="+strconv.Itoa(port))
		}
	}
	threads := o.Threads
	if threads <= 0 {
		threads = 4
	}
	if threads < 2 {
		threads = 2 // myloader minimum
	}
	args = append(args, mydumperTLSArgs(h, target, env.TmpDir)...)
	args = append(args, "--directory="+o.Dir, "--threads="+strconv.Itoa(threads), "--verbose=3")
	// Existing tables are replaced (same semantics as a mysqldump file with
	// DROP TABLE IF EXISTS). Newer myloader defaults to FAIL otherwise.
	switch {
	case has("drop-table"):
		args = append(args, "--drop-table=DROP")
	case has("overwrite-tables"):
		args = append(args, "--overwrite-tables")
	}
	if o.SourceDB != "" {
		args = append(args, "--source-db="+o.SourceDB)
	}
	if o.TargetDB != "" {
		args = append(args, "--database="+o.TargetDB)
	}
	if o.EnableBinlog {
		args = append(args, "--enable-binlog")
	}
	if o.OptimizeKeys && has("innodb-optimize-keys") {
		args = append(args, "--innodb-optimize-keys")
	}
	cmd := env.command(tool.Path, args, extraEnv)
	return env.runCmd(cmd, "[myloader] ")
}
