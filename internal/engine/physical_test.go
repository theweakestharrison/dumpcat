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
