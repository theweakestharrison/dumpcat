package engine

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"dumpcat/internal/store"
)

// BinlogStreamArgs builds the arguments that copy binary logs from a server
// byte for byte (--raw) into dir, starting at startFile. With follow the
// process keeps waiting for new events (--stop-never) and connects with
// serverID, which must be unique among the replicas of the server.
func BinlogStreamArgs(t *Tools, toolName, cnf, dir, startFile string, serverID uint32, follow bool) []string {
	args := []string{
		"--defaults-extra-file=" + cnf,
		"--read-from-remote-server",
		"--raw",
		"--result-file=" + dir + string(os.PathSeparator),
	}
	if follow {
		args = append(args, "--stop-never")
		id := strconv.FormatUint(uint64(serverID), 10)
		switch {
		case t.HasFlag(toolName, "connection-server-id"):
			args = append(args, "--connection-server-id="+id)
		case t.HasFlag(toolName, "stop-never-slave-server-id"):
			args = append(args, "--stop-never-slave-server-id="+id)
		}
	}
	return append(args, startFile)
}

// BinlogStep replays one local binlog file (or part of it).
type BinlogStep struct {
	File     string // raw binlog file on disk
	StartPos int64  // 0 = from the beginning
	StopPos  int64  // 0 = to the end; events at or after StopPos are skipped
	Database string // replay only changes of this database ("" = everything)
	TargetDB string // apply Database's changes to this database instead
}

type BinlogApplyOpts struct {
	BinlogTool   string // mysqlbinlog / mariadb-binlog (must match the source flavor)
	ClientFlavor string // client used against the target server
	NoBinlog     bool   // SET sql_log_bin=0 on the target session
	// SkipGTIDs replays transactions under new GTIDs: needed when the target
	// already executed the original GTIDs (logical restore onto the source
	// server) or they would be skipped / rejected.
	SkipGTIDs bool
	// OnStep is called after each step has been fed to the client.
	OnStep func(i int)
}

// rewriteSink is a database name nothing is ever applied to: changes of a
// database that already carries the rename target's name are mapped there
// and filtered out.
const rewriteSink = "__dumpcat_skip__"

// binlogReplayArgs builds the mysqlbinlog arguments of one step. The two
// tools disagree on --database vs --rewrite-db: MySQL filters on the
// rewritten name, mariadb-binlog (verified on 11.4) on the original one.
func binlogReplayArgs(st BinlogStep, mariadb, skipGTIDs bool) []string {
	args := []string{"--no-defaults"}
	if st.StartPos > 4 {
		args = append(args, "--start-position="+strconv.FormatInt(st.StartPos, 10))
	}
	if st.StopPos > 0 {
		args = append(args, "--stop-position="+strconv.FormatInt(st.StopPos, 10))
	}
	if st.Database != "" {
		db := st.Database
		switch {
		case st.TargetDB == "" || st.TargetDB == st.Database:
		case mariadb:
			args = append(args, "--rewrite-db="+st.Database+"->"+st.TargetDB)
		default:
			// filtering sees rewritten names: keep the real TargetDB out
			args = append(args, "--rewrite-db="+st.TargetDB+"->"+rewriteSink, "--rewrite-db="+st.Database+"->"+st.TargetDB)
			db = st.TargetDB
		}
		args = append(args, "--database="+db)
	}
	if skipGTIDs && !mariadb {
		args = append(args, "--skip-gtids")
	}
	return append(args, st.File)
}

// ApplyBinlogs converts binlog files to SQL with mysqlbinlog and feeds all of
// them, in order, into one client session on the target: temporary tables
// and session state survive from one file to the next.
func ApplyBinlogs(env *Env, target *store.Server, steps []BinlogStep, o BinlogApplyOpts) error {
	bt := env.Tools.Get(o.BinlogTool)
	if err := requireTool(bt); err != nil {
		return err
	}
	cname := ClientToolName(o.ClientFlavor)
	ct := env.Tools.Get(cname)
	if err := requireTool(ct); err != nil {
		return err
	}
	cnf, extraEnv, err := WriteCnf(env.TmpDir, target, CnfOptions{Groups: []string{"client"}, TLS: true, MariaDBClient: ct.MariaDB})
	if err != nil {
		return err
	}
	// an OS pipe: mysqlbinlog writes straight into the client, and a client
	// that dies makes the writer fail instead of blocking forever
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pw.Close()
	client := env.command(ct.Path, []string{"--defaults-extra-file=" + cnf, "--max-allowed-packet=1G", "--binary-mode", "--batch"}, extraEnv)
	client.Stdin = pr
	clw := env.Log.Writer("[" + cname + "] ")
	ctail := newTail(2048)
	client.Stdout = clw
	client.Stderr = io.MultiWriter(clw, ctail)
	if err := client.Start(); err != nil {
		pr.Close()
		clw.Close()
		return err
	}
	pr.Close()
	clientDone := make(chan error, 1)
	go func() {
		err := client.Wait()
		clw.Close()
		clientDone <- err
	}()

	feedErr := func() error {
		if o.NoBinlog {
			if _, err := io.WriteString(pw, "SET SESSION sql_log_bin=0;\n"); err != nil {
				return err
			}
		}
		for i, st := range steps {
			cmd := env.command(bt.Path, binlogReplayArgs(st, bt.MariaDB, o.SkipGTIDs), nil)
			var filter *replayFilter
			renamed := st.Database != "" && st.TargetDB != "" && st.TargetDB != st.Database
			if (o.SkipGTIDs && bt.MariaDB) || renamed {
				filter = &replayFilter{w: pw, stripGTID: o.SkipGTIDs && bt.MariaDB}
				if renamed {
					filter.from, filter.to = st.Database, st.TargetDB
				}
				cmd.Stdout = filter
			} else {
				cmd.Stdout = pw
			}
			lw := env.Log.Writer("[" + o.BinlogTool + "] ")
			tl := newTail(2048)
			cmd.Stderr = io.MultiWriter(lw, tl)
			err := cmd.Run()
			lw.Close()
			if err == nil && filter != nil {
				err = filter.Flush()
			}
			if err != nil {
				if env.Ctx.Err() != nil {
					return env.Ctx.Err()
				}
				if m := lastLines(tl.String(), 4); m != "" {
					return fmt.Errorf("%s %s: %v: %s", o.BinlogTool, filepath.Base(st.File), err, m)
				}
				return fmt.Errorf("%s %s: %w", o.BinlogTool, filepath.Base(st.File), err)
			}
			if o.OnStep != nil {
				o.OnStep(i)
			}
		}
		return nil
	}()
	pw.Close()
	cerr := <-clientDone
	if env.Ctx.Err() != nil {
		return env.Ctx.Err()
	}
	if cerr != nil {
		// the client's own error explains a failing writer better
		if m := lastLines(ctail.String(), 4); m != "" {
			return fmt.Errorf("%s: %v: %s", cname, cerr, m)
		}
		return fmt.Errorf("%s: %w", cname, cerr)
	}
	return feedErr
}

// replayFilter edits mysqlbinlog output on its way to the client:
//   - stripGTID drops "SET @@session.gtid_seq_no=N" so the target assigns
//     its own sequence numbers (mariadb-binlog has no --skip-gtids);
//   - from/to (renamed restores) rewrite database-level DDL. --rewrite-db
//     only changes the default database: a replayed `DROP DATABASE shop`
//     would otherwise drop the original database instead of the copy.
//
// Only the first bytes of each line are buffered to decide; other long
// lines pass through unchanged.
type replayFilter struct {
	w         io.Writer
	stripGTID bool
	from, to  string
	pend      []byte
	mode      int // 0 deciding, 1 passing, 2 dropping, 3 buffering a DDL line
}

const filterProbe = 64

var (
	gtidSeqSet = []byte("SET @@session.gtid_seq_no=")
	dbDDLRe    = regexp.MustCompile(`(?i)^\s*(?:create|drop|alter)\s+(?:database|schema)\b`)
	dbDDLName  = regexp.MustCompile(`(?is)^(\s*(?:create|drop|alter)\s+(?:database|schema)\s+(?:if\s+(?:not\s+)?exists\s+)?)` +
		"(`(?:[^`]|``)+`|[0-9A-Za-z_$]+)(.*)$")
)

const (
	modeDecide = iota
	modePass
	modeDrop
	modeDDL
)

// decide classifies a line from its first bytes (complete when it ends in \n).
func (f *replayFilter) decide(probe []byte) int {
	if f.stripGTID && bytes.HasPrefix(probe, []byte("/*")) && bytes.Contains(probe, gtidSeqSet) {
		return modeDrop
	}
	if f.from != "" && dbDDLRe.Match(probe) {
		return modeDDL
	}
	return modePass
}

// rewriteDDL renames the source database in a database-level statement and
// neutralizes one naming any other database.
func (f *replayFilter) rewriteDDL(line []byte) []byte {
	nl := ""
	if bytes.HasSuffix(line, []byte("\n")) {
		line, nl = line[:len(line)-1], "\n"
	}
	if m := dbDDLName.FindSubmatch(line); m != nil {
		name := string(m[2])
		if strings.HasPrefix(name, "`") {
			name = strings.ReplaceAll(name[1:len(name)-1], "``", "`")
		}
		if name == f.from {
			return []byte(string(m[1]) + "`" + strings.ReplaceAll(f.to, "`", "``") + "`" + string(m[3]) + nl)
		}
	}
	return []byte("DO 0" + nl)
}

func (f *replayFilter) emit(b []byte) error {
	_, err := f.w.Write(b)
	return err
}

func (f *replayFilter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		switch f.mode {
		case modePass, modeDrop:
			i := bytes.IndexByte(p, '\n')
			chunk := p
			if i >= 0 {
				chunk = p[:i+1]
			}
			if f.mode == modePass {
				if err := f.emit(chunk); err != nil {
					return 0, err
				}
			}
			p = p[len(chunk):]
			if i >= 0 {
				f.mode = modeDecide
			}
		case modeDDL:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				f.pend = append(f.pend, p...)
				p = nil
				continue
			}
			f.pend = append(f.pend, p[:i+1]...)
			p = p[i+1:]
			if err := f.emit(f.rewriteDDL(f.pend)); err != nil {
				return 0, err
			}
			f.pend, f.mode = f.pend[:0], modeDecide
		default:
			i := bytes.IndexByte(p, '\n')
			if i >= 0 && len(f.pend)+i < filterProbe {
				f.pend = append(f.pend, p[:i+1]...)
				p = p[i+1:]
				switch f.decide(f.pend) {
				case modePass:
					if err := f.emit(f.pend); err != nil {
						return 0, err
					}
				case modeDDL:
					if err := f.emit(f.rewriteDDL(f.pend)); err != nil {
						return 0, err
					}
				}
				f.pend = f.pend[:0]
				continue
			}
			take := min(filterProbe-len(f.pend), len(p))
			f.pend = append(f.pend, p[:take]...)
			p = p[take:]
			if len(f.pend) >= filterProbe {
				switch f.mode = f.decide(f.pend); f.mode {
				case modePass:
					if err := f.emit(f.pend); err != nil {
						return 0, err
					}
					f.pend = f.pend[:0]
				case modeDrop:
					f.pend = f.pend[:0]
				}
			}
		}
	}
	return n, nil
}

// Flush writes a pending unterminated last line.
func (f *replayFilter) Flush() error {
	var err error
	if len(f.pend) > 0 {
		switch f.decide(f.pend) {
		case modePass:
			err = f.emit(f.pend)
		case modeDDL:
			err = f.emit(f.rewriteDDL(f.pend))
		}
	}
	f.pend, f.mode = f.pend[:0], modeDecide
	return err
}

// CheckBinlogName rejects binlog names that could be taken for options or paths.
func CheckBinlogName(name string) error {
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "/\\\x00\n") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid binlog name: %q", name)
	}
	return nil
}
