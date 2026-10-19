package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// memSink keeps the files in memory and remembers aborted ones.
type memSink struct {
	mu      sync.Mutex
	files   map[string]*bytes.Buffer
	aborted map[string]error
}

type memFile struct {
	s    *memSink
	name string
	*bytes.Buffer
}

func (f memFile) Close() error { return nil }
func (f memFile) Abort(err error) {
	f.s.mu.Lock()
	f.s.aborted[f.name] = err
	f.s.mu.Unlock()
}

func (s *memSink) Create(name string, o FileOpts) (io.WriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &bytes.Buffer{}
	s.files[name] = b
	return memFile{s, name, b}, nil
}

func newMemSink() *memSink {
	return &memSink{files: map[string]*bytes.Buffer{}, aborted: map[string]error{}}
}

func TestPhysicalArgs(t *testing.T) {
	spec := &BackupSpec{Flavor: "mariadb", Server: &store.Server{Datadir: "/var/lib/mysql"}, IncrementalLSN: "4242",
		Job: &store.Job{Threads: 3, Options: store.JobOptions{FTWRLWaitTimeout: 60, SlaveInfo: true, ExtraArgs: "--galera-info --use-memory=1G"}}}
	po := PhysicalOptionsFor(spec, true)
	if po.Tool != "mariabackup" || !po.LowPriority || strings.Join(po.ToolGroups(), ",") != "mariabackup,mariadb-backup" {
		t.Fatalf("options: %+v", po)
	}
	got := strings.Join(po.Args("/tmp/x", "/tmp/lsn"), " ")
	want := "--backup --stream=xbstream --target-dir=/tmp/x --extra-lsndir=/tmp/lsn --parallel=3 --datadir=/var/lib/mysql " +
		"--ftwrl-wait-timeout=60 --slave-info --incremental-lsn=4242 --galera-info --use-memory=1G"
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
	spec.Flavor = "mysql"
	if po := PhysicalOptionsFor(spec, false); po.Tool != "xtrabackup" || po.ToolGroups()[0] != "xtrabackup" {
		t.Fatalf("mysql: %+v", po)
	}
}

// The tool runs on the database host: its stream lands in the sink like a
// local backup's, and the result comes from the checkpoint files it sent.
func TestPhysicalRemote(t *testing.T) {
	spec := &BackupSpec{Flavor: "mariadb", Server: &store.Server{Datadir: "/var/lib/mysql"}, Job: &store.Job{Threads: 2}}
	env := &Env{Ctx: context.Background(), Log: tasklog.Discard()}
	stream := bytes.Repeat([]byte("xbstream"), 100000)
	var got PhysicalOptions
	spec.Remote = func(ctx context.Context, o PhysicalOptions, stdout, stderr io.Writer) (map[string]string, string, error) {
		got = o
		_, _ = stdout.Write(stream)
		_, _ = io.WriteString(stderr, "[00] Connecting\n[00] completed OK!\n")
		return map[string]string{
			"mariadb_backup_checkpoints": "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 999\nlast_lsn = 999\n",
			"mariadb_backup_binlog_info": "mysql-bin.000007\t1234\t0-1-55\n",
		}, "mariabackup 11.4.13", nil
	}
	sink := newMemSink()
	res, err := Physical(env, spec, sink)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tool != "mariabackup" || got.Datadir != "/var/lib/mysql" || got.Parallel != 2 {
		t.Fatalf("options sent: %+v", got)
	}
	if !bytes.Equal(sink.files["backup.xbstream"].Bytes(), stream) || len(sink.aborted) != 0 {
		t.Fatal("the stream must be stored as backup.xbstream")
	}
	if res.Meta["to_lsn"] != "999" || res.Meta["binlog_file"] != "mysql-bin.000007" || res.Meta["binlog_pos"] != int64(1234) ||
		res.Meta["gtid"] != "0-1-55" || res.Meta["tool"] != "mariabackup 11.4.13" {
		t.Fatalf("meta: %v", res.Meta)
	}

	// no "completed OK!": the stream is incomplete and must never be stored
	spec.Remote = func(ctx context.Context, o PhysicalOptions, stdout, stderr io.Writer) (map[string]string, string, error) {
		_, _ = stdout.Write(stream[:100])
		return map[string]string{"mariadb_backup_checkpoints": "to_lsn = 1"}, "mariabackup", nil
	}
	sink = newMemSink()
	if _, err := Physical(env, spec, sink); err == nil || sink.aborted["backup.xbstream"] == nil {
		t.Fatalf("missing completion marker: %v, aborted %v", err, sink.aborted)
	}

	// the tool failed on the database host: its last lines explain why
	spec.Remote = func(ctx context.Context, o PhysicalOptions, stdout, stderr io.Writer) (map[string]string, string, error) {
		_, _ = io.WriteString(stderr, "[ERROR] mariabackup: Got error 1045: Access denied\n")
		return nil, "", errors.New("exit status 1")
	}
	sink = newMemSink()
	if _, err := Physical(env, spec, sink); err == nil || !strings.Contains(err.Error(), "Access denied") || sink.aborted["backup.xbstream"] == nil {
		t.Fatalf("tool failure: %v", err)
	}
}

// --extra-lsndir holds the checkpoints and the info file, not *_binlog_info:
// the binlog coordinates that point-in-time recovery starts from come from the
// info file's binlog_pos line.
func TestPhysicalResultInfoBinlogPos(t *testing.T) {
	env := &Env{Ctx: context.Background(), Log: tasklog.Discard()}
	spec := &BackupSpec{}
	cp := "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 999\nlast_lsn = 999\n"
	cases := []struct {
		name, info, file, gtid string
		pos                    int64
	}{
		{"mariabackup", "tool_name = mariabackup\nbinlog_pos = filename 'mysql-bin.000007', position '1234', GTID of the last change '0-1-55'\n",
			"mysql-bin.000007", "0-1-55", 1234},
		{"xtrabackup, GTID set over two lines", "tool_name = xtrabackup\nbinlog_pos = filename 'binlog.000002', position '157', GTID of the last change '3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5,\n8f2a6c1e-0b8e-11ee-b8a4-0242ac120002:1-3'\ninnodb_version = 8.0.36\n",
			"binlog.000002", "3e11fa47-71ca-11e1-9e33-c80aa9429562:1-5,8f2a6c1e-0b8e-11ee-b8a4-0242ac120002:1-3", 157},
		{"no GTID", "binlog_pos = filename 'binlog.000009', position '4'\n", "binlog.000009", "", 4},
	}
	for _, c := range cases {
		name := "mariadb_backup_info"
		if strings.HasPrefix(c.name, "xtrabackup") {
			name = "xtrabackup_info"
		}
		res, err := physicalResult(env, spec, "test", map[string]string{"xtrabackup_checkpoints": cp, name: c.info})
		if err != nil {
			t.Fatal(c.name, err)
		}
		if res.Meta["binlog_file"] != c.file || res.Meta["binlog_pos"] != c.pos {
			t.Errorf("%s: binlog %v %v", c.name, res.Meta["binlog_file"], res.Meta["binlog_pos"])
		}
		if g, _ := res.Meta["gtid"].(string); g != c.gtid {
			t.Errorf("%s: gtid %q, want %q", c.name, g, c.gtid)
		}
	}

	// binlogs off: no binlog_pos line, nothing recorded
	res, err := physicalResult(env, spec, "test", map[string]string{"xtrabackup_checkpoints": cp, "xtrabackup_info": "tool_name = xtrabackup\n"})
	if err != nil || res.Meta["binlog_file"] != nil {
		t.Fatalf("no binlog: %v %v", err, res.Meta)
	}
}
