package pgsql

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
	"dumpcat/internal/tasklog"
)

// The physical restore of a pg_basebackup backup: the tar stream becomes a
// data directory on the DumpCat host, PostgreSQL itself recovers it (to a
// point in time with the fetched WAL), and a local cluster can be replaced
// by it.

const (
	walDirName   = "dumpcat_wal" // the fetched WAL, inside the data directory: restore_command is relative
	recoveryConf = "dumpcat-recovery.conf"
	recoveryHBA  = "dumpcat-recovery-hba.conf"
	recoveryID   = "dumpcat-recovery-ident.conf"
)

// datadirMarkers: a non-empty directory without PG_VERSION is not a data directory.
var datadirMarkers = []string{"PG_VERSION"}

// RestorePhysical prepares spec.Run as a data directory (see the package doc).
func (Driver) RestorePhysical(env *engine.Env, src engine.RestoreSource, spec *engine.PhysicalSpec) error {
	run := spec.Run
	if run.Method != store.MethodPgBase {
		return fmt.Errorf("this backup (%s) is not a physical PostgreSQL backup", run.Method)
	}
	lg := env.Log
	files := src.Files()
	base, ok := files[BaseFile]
	if !ok {
		return errors.New("the backup has no " + BaseFile)
	}
	pgMajor, _ := engine.MetaInt(run.Meta, "pg_major")
	osUser := strings.TrimSpace(spec.OSUser)
	if osUser == "" {
		osUser = "postgres"
	}
	asUser := ""
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		asUser = osUser // PostgreSQL refuses to run as root
	}
	// the server binaries of the backup's version run the recovery
	pg, binErr := env.Tools.PGExact("postgres", int(pgMajor))
	ctl, ctlErr := env.Tools.PGExact("pg_ctl", int(pgMajor))
	if binErr == nil && runtime.GOOS == "windows" && ctlErr != nil {
		binErr = ctlErr // stopping it on Windows takes pg_ctl
	}
	var rp *WALReplay
	if spec.Replay != nil {
		rp = spec.Replay.State.(*WALReplay)
	}

	dir := spec.Dir
	var service string
	if a := spec.Apply; a != nil {
		if binErr != nil {
			return fmt.Errorf("replacing the data directory needs the PostgreSQL %d server on this machine: %w", pgMajor, binErr)
		}
		var err error
		if service, err = detectService(a.Service, int(pgMajor)); err != nil {
			return err
		}
		if err := engine.CheckDatadir(a.Datadir, service, osUser, a.DataDir, datadirMarkers, "PostgreSQL"); err != nil {
			return err
		}
		if v, err := os.ReadFile(filepath.Join(a.Datadir, "PG_VERSION")); err == nil && major(string(v)) != int(pgMajor) {
			return fmt.Errorf("%s is a PostgreSQL %s data directory, the backup is PostgreSQL %d", a.Datadir, strings.TrimSpace(string(v)), pgMajor)
		}
		// prepared next to it: same filesystem, and PostgreSQL's user can reach it
		dir = a.Datadir + ".dumpcat-restore-" + time.Now().Format("20060102-150405")
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("target directory %s is not empty", dir)
	}
	if err := mkdirFor(dir, asUser); err != nil {
		return err
	}
	keep := false
	if spec.Apply != nil {
		defer func() {
			if !keep {
				os.RemoveAll(dir)
			}
		}()
	}
	lg.Printf("Data directory: %s (PostgreSQL %d, backup %s)", dir, pgMajor, run.DirName)
	if t, free, err := sysinfo.DiskUsage(dir); err == nil {
		lg.Printf("Free space: %s of %s (the cluster takes about %s)", engine.HumanBytes(free), engine.HumanBytes(t), engine.HumanBytes(base.RawSize))
	}

	// every file opens with the passphrase before anything is written
	src.SetPhase("prepare")
	if err := src.Preflight(env.Ctx, []store.BackupFile{base}); err != nil {
		return err
	}
	src.SetPhase("download")
	src.SetEstimate(base.Size)
	lg.Printf("Downloading & extracting %s (%s) from %s", base.Name, engine.HumanBytes(base.Size), src.Describe())
	rd, done, err := src.Open(env.Ctx, base)
	if err != nil {
		return err
	}
	n, err := extractTar(rd, dir)
	if derr := done(err == nil); err == nil && derr != nil {
		err = derr
	}
	if err != nil {
		return err
	}
	lg.Printf("Extracted %d files", n)
	if m, err := os.ReadFile(filepath.Join(dir, "tablespace_map")); err == nil && len(strings.TrimSpace(string(m))) > 0 {
		return errors.New("the backup has tablespaces: they cannot be restored by DumpCat yet")
	}
	if spec.Apply == nil {
		if err := addConfigFiles(dir, lg); err != nil {
			return err
		}
	}
	src.SetPhase("prepare")
	var settings [][2]string
	if rp != nil {
		if err := placeWAL(rp, filepath.Join(dir, walDirName)); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "recovery.signal"), nil, 0o600); err != nil {
			return err
		}
		settings = append([][2]string{{"restore_command", restoreCommand(false)}, {"recovery_target_timeline", "latest"},
			{"recovery_target_action", "promote"}}, rp.Settings...)
		lg.Printf("Recovery: %d WAL file(s) in %s, stopping at %s", len(rp.Files), walDirName, spec.Replay.Stop)
	}
	if asUser != "" {
		if err := engine.RunSimple(env.Ctx, lg, "chown", "-R", "--", asUser+":", dir); err != nil {
			return err
		}
	}
	recovered := false
	switch {
	case binErr != nil:
		lg.Printf("The PostgreSQL %d server is not installed on this machine (%v): the directory is left ready to recover", pgMajor, binErr)
	case asUser != "" && spec.Apply == nil:
		if err := execx.Reachable(dir, asUser); err != nil {
			lg.Printf("PostgreSQL would run as %s, which cannot reach %s (%v): the directory is left ready to recover — choose a directory %s can reach (e.g. /srv/dumpcat-restore) to let DumpCat recover it", asUser, dir, err, asUser)
			break
		}
		fallthrough
	default:
		src.SetPhase("recovery")
		if err := runRecovery(env, dir, pg, ctl, settings, asUser); err != nil {
			return err
		}
		recovered = true
	}
	if !recovered {
		if err := leaveReadyToRecover(dir, settings, asUser, lg); err != nil {
			return err
		}
		lg.Printf("To use it: copy %s to a machine with PostgreSQL %d (owned by the postgres user, mode 0700) and start the server on it, e.g. pg_ctl -D <dir> start — it replays the WAL%s and is then ready.", dir, pgMajor,
			map[bool]string{true: " up to the recovery target, promotes", false: " of the backup"}[rp != nil])
		return nil
	}
	if spec.Apply == nil {
		lg.Printf("The data directory is ready at %s: start PostgreSQL %d on it (it keeps the source's settings in postgresql.auto.conf).", dir, pgMajor)
		return nil
	}
	keep = true
	return replaceCluster(env, dir, spec.Apply.Datadir, service, asUser)
}

// mkdirFor creates dir (mode 0700) and its missing parents. When the
// recovery runs as another OS user, these get mode 0755 whatever the umask
// (the service's is 0027): that user must be able to enter them.
func mkdirFor(dir, asUser string) error {
	if asUser != "" {
		var missing []string
		for d := filepath.Dir(dir); d != filepath.Dir(d); d = filepath.Dir(d) {
			if _, err := os.Stat(d); err == nil {
				break
			}
			missing = append(missing, d)
		}
		for i := len(missing) - 1; i >= 0; i-- {
			if err := os.Mkdir(missing[i], 0o755); err != nil {
				if os.IsExist(err) {
					continue
				}
				return err
			}
			if err := os.Chmod(missing[i], 0o755); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// addConfigFiles writes the configuration files a prepared directory lacks:
// the source kept them outside its data directory (Debian/Ubuntu:
// /etc/postgresql/<version>/<cluster>/), and PostgreSQL does not start
// without them. PostgreSQL's defaults, local connections only.
func addConfigFiles(dir string, lg *tasklog.Logger) error {
	hba := "# TYPE  DATABASE  USER  ADDRESS       METHOD\n"
	if runtime.GOOS != "windows" {
		hba += "local   all       all                 peer\n"
	}
	hba += "host    all       all   127.0.0.1/32  scram-sha-256\nhost    all       all   ::1/128       scram-sha-256\n"
	files := [][2]string{
		{"postgresql.conf", "# PostgreSQL's defaults apply; the source's ALTER SYSTEM settings are in postgresql.auto.conf.\n"},
		{"pg_hba.conf", "# Local connections only.\n" + hba},
		{"pg_ident.conf", ""},
	}
	var added []string
	for _, f := range files {
		p := filepath.Join(dir, f[0])
		if _, err := os.Stat(p); err == nil {
			continue
		}
		head := "# Written by DumpCat: the source kept its " + f[0] + " outside the data directory (Debian/Ubuntu: /etc/postgresql/<version>/<cluster>/).\n"
		if err := os.WriteFile(p, []byte(head+f[1]), 0o600); err != nil {
			return err
		}
		added = append(added, f[0])
	}
	if len(added) > 0 {
		lg.Printf("The source kept its configuration outside the data directory (Debian/Ubuntu layout): wrote a minimal %s so that PostgreSQL starts on the directory (local connections only)", strings.Join(added, ", "))
	}
	return nil
}

// restoreCommand copies a fetched segment for the recovery; the path is
// relative to the data directory, where the server runs it. escaped: for a
// configuration file, where a backslash starts an escape.
func restoreCommand(escaped bool) string {
	if runtime.GOOS == "windows" {
		if escaped {
			return `copy "` + walDirName + `\\%f" "%p"`
		}
		return `copy "` + walDirName + `\%f" "%p"`
	}
	return `cp "` + walDirName + `/%f" "%p"`
}

// extractTar writes a tar stream into dir: regular files and directories
// only, every path kept inside dir.
func extractTar(r io.Reader, dir string) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
			return n, fmt.Errorf("unsafe path in the backup: %q", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return n, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return n, err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return n, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return n, err
			}
			n++
		default:
			// symbolic links (pg_wal, tablespaces) are not followed out of the directory
			return n, fmt.Errorf("the backup holds %s, which is not a file or directory (links are not restored)", h.Name)
		}
	}
}

// freePort finds a TCP port on 127.0.0.1 for the recovery server.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// recoveryLog follows the server's messages: they go to the task log, the
// last ones are kept for errors, and readiness is signaled.
type recoveryLog struct {
	lg    *tasklog.Logger
	ready chan struct{}
	once  sync.Once
	mu    sync.Mutex
	last  []string
}

func (l *recoveryLog) follow(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		l.lg.Printf("[postgres] %s", line)
		l.mu.Lock()
		l.last = append(l.last, line)
		if len(l.last) > 8 {
			l.last = l.last[len(l.last)-8:]
		}
		l.mu.Unlock()
		if strings.Contains(line, "database system is ready to accept connections") {
			l.once.Do(func() { close(l.ready) })
		}
	}
	io.Copy(io.Discard, r)
}

func (l *recoveryLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.last, " | ")
}

// runRecovery starts a temporary server on dir, waits until it accepts
// connections (crash recovery done, or archive recovery promoted), and
// stops it: dir is then a clean data directory. The server uses DumpCat's
// own configuration (no listening socket but a random local port, no
// archiving, no preloaded libraries) and runs as asUser when DumpCat is
// root.
func runRecovery(env *engine.Env, dir string, pg, ctl engine.PGTool, settings [][2]string, asUser string) error {
	lg := env.Log
	port, err := freePort()
	if err != nil {
		return err
	}
	own := map[string]string{
		recoveryConf: "# written by DumpCat for the recovery of this directory, removed afterwards\n",
		recoveryHBA:  "host all all 127.0.0.1/32 reject\n",
		recoveryID:   "",
	}
	for n, body := range own {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
			return err
		}
		if asUser != "" {
			if err := engine.RunSimple(env.Ctx, lg, "chown", "--", asUser+":", filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	defer func() {
		for n := range own {
			os.Remove(filepath.Join(dir, n))
		}
		os.Remove(filepath.Join(dir, "postmaster.opts"))
		os.RemoveAll(filepath.Join(dir, walDirName))
	}()
	opts := [][2]string{
		{"config_file", filepath.Join(dir, recoveryConf)}, {"hba_file", filepath.Join(dir, recoveryHBA)}, {"ident_file", filepath.Join(dir, recoveryID)},
		{"listen_addresses", "127.0.0.1"}, {"port", strconv.Itoa(port)}, {"unix_socket_directories", ""},
		{"hot_standby", "off"}, {"archive_mode", "off"}, {"logging_collector", "off"}, {"log_destination", "stderr"}, {"lc_messages", "C"},
		{"shared_preload_libraries", ""}, {"shared_buffers", "256MB"}, {"huge_pages", "off"}, {"max_connections", "20"},
		{"synchronous_standby_names", ""}, {"cluster_name", "dumpcat-recovery"},
	}
	opts = append(opts, settings...)
	args := []string{"-D", dir}
	for _, o := range opts {
		args = append(args, "-c", o[0]+"="+o[1])
	}
	ctx, cancel := context.WithCancel(env.Ctx)
	defer cancel()
	cmd := execx.Command(ctx, pg.Path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), cEnv...)
	if asUser != "" {
		if err := execx.AsUser(cmd, asUser); err != nil {
			return err
		}
	}
	out, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	rl := &recoveryLog{lg: lg, ready: make(chan struct{})}
	lg.Printf("Recovering with a temporary PostgreSQL %d on 127.0.0.1:%d%s: $ %s", pg.Major, port, map[bool]string{true: " as " + asUser, false: ""}[asUser != ""], execx.Quote(pg.Path, args))
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	followed := make(chan struct{})
	go func() {
		defer close(followed)
		rl.follow(out)
	}()
	exited := make(chan error, 1)
	go func() {
		<-followed
		exited <- cmd.Wait()
	}()
	select {
	case <-rl.ready:
	case err := <-exited:
		if env.Ctx.Err() != nil {
			return env.Ctx.Err()
		}
		return fmt.Errorf("the recovery server stopped (%v): %s", err, rl.tail())
	case <-env.Ctx.Done():
		if runtime.GOOS == "windows" {
			// its children are processes of their own: the postmaster stops them
			_ = execx.Command(context.Background(), ctl.Path, "stop", "-D", dir, "-m", "immediate", "-w").Run()
		}
		cancel()
		<-exited
		return env.Ctx.Err()
	}
	lg.Printf("Recovery finished in %s — stopping the temporary server", engine.HumanDuration(time.Since(t0)))
	if err := stopServer(env, cmd, ctl, dir, asUser); err != nil {
		lg.Printf("WARNING: %v", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			return fmt.Errorf("the recovery server did not stop cleanly (%v): %s", err, rl.tail())
		}
	case <-time.After(10 * time.Minute):
		cancel()
		<-exited
		return errors.New("the recovery server did not stop within 10 minutes")
	}
	return nil
}

// stopServer asks the temporary server for a fast shutdown.
func stopServer(env *engine.Env, cmd *exec.Cmd, ctl engine.PGTool, dir, asUser string) error {
	if runtime.GOOS != "windows" {
		return cmd.Process.Signal(syscall.SIGINT)
	}
	c := execx.Command(context.Background(), ctl.Path, "stop", "-D", dir, "-m", "fast", "-w", "-t", "600")
	c.Env = append(os.Environ(), cEnv...)
	w := env.Log.Writer("[pg_ctl] ")
	defer w.Close()
	c.Stdout, c.Stderr = w, w
	return c.Run()
}

// leaveReadyToRecover writes the recovery settings into postgresql.auto.conf:
// the next start of the directory recovers it.
func leaveReadyToRecover(dir string, settings [][2]string, asUser string, lg *tasklog.Logger) error {
	if len(settings) == 0 {
		return nil // a plain backup: the server replays its own WAL when started
	}
	var b strings.Builder
	b.WriteString("\n# Added by DumpCat: point-in-time recovery from the WAL in " + walDirName + "/ when the server starts.\n")
	b.WriteString("# Afterwards remove these settings (ALTER SYSTEM RESET restore_command; ...) and the " + walDirName + " folder.\n")
	for _, s := range settings {
		v := s[1]
		if s[0] == "restore_command" {
			v = restoreCommand(true)
		}
		b.WriteString(s[0] + " = '" + strings.ReplaceAll(v, "'", "''") + "'\n")
	}
	p := filepath.Join(dir, "postgresql.auto.conf")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if asUser != "" {
		return engine.RunSimple(context.Background(), lg, "chown", "--", asUser+":", p)
	}
	return nil
}

// detectService returns the systemd unit of the local PostgreSQL cluster.
func detectService(service string, major int) (string, error) {
	if s := strings.TrimSpace(service); s != "" {
		return s, nil
	}
	for _, s := range []string{fmt.Sprintf("postgresql@%d-main", major), fmt.Sprintf("postgresql-%d", major), "postgresql"} {
		if exec.Command("systemctl", "cat", s+".service").Run() == nil {
			return s, nil
		}
	}
	return "", errors.New("could not determine the PostgreSQL service name (enter it in the form, e.g. postgresql@16-main)")
}

// replaceCluster swaps the recovered directory in for the local cluster's:
// stop the service, keep the old directory, start it again; any error
// moves the old one back.
func replaceCluster(env *engine.Env, prepared, datadir, service, asUser string) error {
	ctx, lg := env.Ctx, env.Log
	stamp := time.Now().Format("20060102-150405")
	oldDir := datadir + ".before-restore-" + stamp
	lg.Printf("=== REPLACING THE DATA DIRECTORY %s (service %s) ===", datadir, service)
	if err := engine.RunSimple(ctx, lg, "systemctl", "stop", "--", service); err != nil {
		return fmt.Errorf("could not stop the service: %w", err)
	}
	moved := false
	if _, err := os.Stat(datadir); err == nil {
		lg.Printf("Renaming the old data directory → %s", oldDir)
		if err := os.Rename(datadir, oldDir); err != nil {
			_ = engine.RunSimple(context.Background(), lg, "systemctl", "start", "--", service)
			return fmt.Errorf("could not rename the old data directory: %w", err)
		}
		moved = true
	}
	rollback := func(cause error) error {
		lg.Printf("ERROR: %v — rolling back to the old data directory", cause)
		kept := prepared
		if err := os.Rename(datadir, prepared+".failed"); err == nil {
			kept = prepared + ".failed"
		}
		if moved {
			if err := os.Rename(oldDir, datadir); err != nil {
				lg.Printf("Rollback failed: %v. Run it yourself: mv %s %s", err, oldDir, datadir)
			}
		}
		_ = engine.RunSimple(context.Background(), lg, "systemctl", "start", "--", service)
		lg.Printf("The restored directory is kept at %s — delete it once you have looked into the error.", kept)
		return cause
	}
	if err := os.Rename(prepared, datadir); err != nil {
		return rollback(err)
	}
	if p, err := exec.LookPath("restorecon"); err == nil {
		_ = engine.RunSimple(ctx, lg, p, "-R", datadir)
	}
	if err := engine.RunSimple(ctx, lg, "systemctl", "start", "--", service); err != nil {
		lg.Printf("The service failed to start with the restored data.")
		return rollback(fmt.Errorf("could not start the service: %w", err))
	}
	lg.Printf("Service %s is running with the restored data.", service)
	if moved {
		lg.Printf("The old data directory is kept at %s — delete it once you have checked the restore.", oldDir)
	}
	return nil
}
