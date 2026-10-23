// DumpCat — MySQL/MariaDB backup & restore manager with a web UI.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"dumpcat/internal/agent"
	"dumpcat/internal/api"
	"dumpcat/internal/auth"
	"dumpcat/internal/config"
	"dumpcat/internal/engine"
	"dumpcat/internal/execx"
	"dumpcat/internal/monitor"
	"dumpcat/internal/mssql"
	"dumpcat/internal/notify"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/runner"
	"dumpcat/internal/scheduler"
	"dumpcat/internal/secret"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
	"dumpcat/internal/update"
	"dumpcat/internal/wg"
	"dumpcat/web"
)

var version = "1.8.0"

// defaultUpdateURL is the release server baked in at build time
// (-ldflags "-X main.defaultUpdateURL=https://..."); Settings and DUMPCAT_UPDATE_URL override it.
var defaultUpdateURL = ""

func usage() {
	fmt.Fprintf(os.Stderr, `DumpCat %s — MySQL, MariaDB & PostgreSQL backup/restore manager

Usage:
  dumpcat serve   [flags]                 run the web server (default)
  dumpcat agent   -server URL -key KEY    run on a database host that accepts no inbound connections
                                          ("dumpcat agent -h" for the options)
  dumpcat user add -u NAME [-r admin|operator|viewer] [-p PASS]
  dumpcat user passwd -u NAME              reset a password
  dumpcat user reset-2fa -u NAME           disable a user's 2FA
  dumpcat user list
  dumpcat setup-token                      print the one-time token that creating the first admin requires
  dumpcat mcp [-check]                     serve DumpCat's tools to an AI assistant over stdio (MCP);
                                          URL and token from DUMPCAT_MCP_* ("dumpcat mcp -h")
  dumpcat decode -in FILE [-out FILE|-]    decrypt + decompress a backup file (.zst/.gz/.age)
  dumpcat update [-check] [-yes] [-rollback] check for / install a new version, or roll back
  dumpcat release keygen|sign|verify|pubkey  sign releases for the updater ("dumpcat release help")
  dumpcat version

Configure with DUMPCAT_* environment variables or the file %s
`, version, config.DefaultEnvFile())
}

func main() {
	log.SetFlags(log.LstdFlags)
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		cfg := loadConfig("serve", args)
		service := inService()
		if cfg.LogFile == "" && service {
			cfg.LogFile = cfg.Path("dumpcat.log") // a service has no console
		}
		if cfg.LogFile != "" {
			lf, err := openRotatingFile(cfg.LogFile, 20<<20, 5)
			if err != nil {
				log.Fatal(err)
			}
			if service {
				log.SetOutput(lf)
			} else {
				log.SetOutput(io.MultiWriter(os.Stderr, lf))
			}
		}
		var err error
		if service {
			err = runService(func(stop <-chan struct{}) error { return serveMain(cfg, stop, true) })
		} else {
			err = serveMain(cfg, signalStop(), false)
		}
		if err != nil {
			log.Fatal(err)
		}
	case "agent":
		agentCmd(args)
	case "mcp":
		mcpCmd(args)
	case "user":
		userCmd(args)
	case "setup-token":
		if err := setupTokenCmd(args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "update":
		updateCmd(args)
	case "update-restart": // internal: restarts the Windows service after an update
		updateRestartCmd(args)
	case "release":
		if err := releaseCmd(args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "decode":
		if err := decodeCmd(args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println("DumpCat", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func loadConfig(name string, args []string) *config.Config {
	cfg := config.Default()
	// env file location may be overridden before parsing other flags
	for i, a := range args {
		if a == "-config" || a == "--config" {
			if i+1 < len(args) {
				cfg.EnvFile = args[i+1]
			}
		} else if strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") {
			cfg.EnvFile = a[strings.Index(a, "=")+1:]
		}
	}
	if err := config.LoadEnvFile(cfg.EnvFile); err != nil {
		log.Printf("cannot read %s: %v", cfg.EnvFile, err)
	}
	cfg.ApplyEnv()
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.String("config", cfg.EnvFile, "env file (KEY=VALUE)")
	cfg.BindFlags(fs)
	_ = fs.Parse(args)
	if err := cfg.Normalize(); err != nil {
		log.Fatal(err)
	}
	return cfg
}

func openStore(cfg *config.Config) *store.Store {
	st, err := openStoreErr(cfg)
	if err != nil {
		log.Fatal(err)
	}
	return st
}

func openStoreErr(cfg *config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	box, err := secret.Load(cfg.Path("master.key"), cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.Path("dumpcat.db"), box)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	if err := st.CheckMasterKey(); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// executablePath is the absolute path of the running binary.
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return exe
}

// updateServer returns the release server and where it is configured.
// updateEvent is the notification about an update or a rollback.
func updateEvent(r update.Result, baseURL string) notify.Event {
	ev := notify.Event{Kind: "update", Status: "failed", Time: r.FinishedAt}
	switch {
	case r.Kind == "update" && r.Status == "success":
		ev.Status, ev.Title = "success", "DumpCat updated to "+r.ToVersion
	case r.Kind == "update" && r.Status == "rolled_back":
		ev.Title = "DumpCat " + r.ToVersion + " did not start, back on " + r.FromVersion
	case r.Kind == "update" && r.By == update.AutoBy:
		ev.Title = "Automatic update to " + r.ToVersion + " failed"
	case r.Kind == "update":
		ev.Title = "Update to " + r.ToVersion + " failed"
	case r.Status == "rolled_back":
		ev.Status, ev.Title = "success", "DumpCat rolled back to "+r.ToVersion
	default:
		ev.Title = "Rollback to " + r.ToVersion + " failed"
	}
	ev.Add("From", r.FromVersion)
	ev.Add("To", r.ToVersion)
	if r.By != "" {
		ev.Add("Started by", r.By)
	}
	ev.Add("Time", r.FinishedAt.Format("2006-01-02 15:04:05"))
	if ev.Status == "success" {
		if r.Message != "" {
			ev.Add("Details", r.Message)
		}
	} else {
		ev.Error = r.Message
	}
	if b := strings.TrimRight(baseURL, "/"); b != "" {
		ev.URL = b + "/#/system"
	}
	return ev
}

func updateServer(st *store.Store) (string, string) {
	if u := strings.TrimSpace(st.GetSettings().UpdateURL); u != "" {
		return u, "settings"
	}
	if u := strings.TrimSpace(os.Getenv("DUMPCAT_UPDATE_URL")); u != "" {
		return u, "env"
	}
	if defaultUpdateURL != "" {
		return defaultUpdateURL, "build"
	}
	return "", ""
}

// restartRequest is returned by serve when an update or rollback must be applied.
type restartRequest struct{ p *update.Pending }

func (r *restartRequest) Error() string { return "restart for " + r.p.Kind }

// serveMain runs the server, applies updates when asked and puts the previous
// version back automatically when a new one cannot start.
func serveMain(cfg *config.Config, stop <-chan struct{}, service bool) error {
	exe := executablePath()
	boot := update.StartBoot(cfg.DataDir, version, exe)
	if boot.GaveUp() {
		return rollBack(boot, fmt.Sprintf("it failed to start %d times", boot.Attempts()-1), exe, service)
	}
	if boot.Active() {
		log.Printf("update: starting DumpCat %s (attempt %d)", version, boot.Attempts())
	}
	err := serve(cfg, stop, boot, exe)
	var rr *restartRequest
	switch {
	case errors.As(err, &rr):
		if aerr := update.Apply(rr.p); aerr != nil {
			log.Printf("update failed: %v", aerr)
			os.Remove(rr.p.NewExe)
			update.ClearPending(cfg.DataDir)
			update.SaveResult(cfg.DataDir, &update.Result{Kind: rr.p.Kind, Status: "failed", FromVersion: rr.p.FromVersion,
				ToVersion: rr.p.ToVersion, Message: aerr.Error(), By: rr.p.By})
		}
		return relaunch(exe, service)
	case err != nil && boot.Active():
		return rollBack(boot, err.Error(), exe, service)
	}
	return err
}

func rollBack(boot *update.Boot, reason, exe string, service bool) error {
	log.Printf("update: the new version did not start (%s) — restoring the previous version", reason)
	again, err := boot.Fail(reason)
	if err != nil {
		return fmt.Errorf("automatic rollback failed: %w", err)
	}
	if again {
		return relaunch(exe, service)
	}
	return errors.New(reason)
}

// listen binds the address; right after an update the old process may still be
// releasing the port, so it retries for a while.
func listen(addr string, retry bool) (net.Listener, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil || !retry || time.Now().After(deadline) {
			return ln, err
		}
		time.Sleep(time.Second)
	}
}

// signalStop returns a channel that is closed on Ctrl+C / SIGTERM.
func signalStop() <-chan struct{} {
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		log.Printf("received %v, shutting down...", s)
		close(stop)
	}()
	return stop
}

// serve runs the web server, scheduler and workers until stop is closed (or an
// update asks for a restart).
func serve(cfg *config.Config, stop <-chan struct{}, boot *update.Boot, exe string) error {
	updateKeys, err := trustedUpdateKeys()
	if err != nil {
		return err
	}
	for _, d := range []string{"logs", "staging", "rclone", "restore"} {
		if err := os.MkdirAll(cfg.Path(d), 0o750); err != nil {
			return err
		}
	}
	st, err := openStoreErr(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	tools := engine.NewTools(func() map[string]string { return st.GetSettings().ToolPaths })
	sm := storage.NewManager(st, cfg.Path("rclone"),
		func() string {
			if t := tools.Get("rclone"); t.Found {
				return t.Path
			}
			return ""
		},
		func() []string { return execx.SplitArgs(st.GetSettings().RcloneFlags) })
	notifier := notify.New(st)
	run := runner.New(st, sm, tools, notifier, cfg.DataDir, version)
	run.BaseURL = func() string {
		if b := strings.TrimRight(st.GetSettings().BaseURL, "/"); b != "" {
			return b
		}
		return cfg.BaseURL
	}
	run.RecoverInterrupted()
	run.CleanupStaleWork()
	go run.RecoverFastMode()
	sched := scheduler.New(st, run)
	sched.Reload()
	run.SyncArchivers()

	// database hosts behind a firewall connect out to us; their servers are
	// reached through local tunnel ports
	agents := agent.NewManager()
	agents.Auth = api.AgentAuth(st)
	agents.OnHello = func(id int64, in agent.Info) {
		st.AgentSeen(id, in.Since, in.RemoteAddr, in.Version, in.Hostname, in.OS)
	}
	agents.Logf = log.Printf
	run.Agents = agents
	store.TunnelEndpoint = func(id int64, network, addr string) (string, int, error) {
		host, port, err := agents.Endpoint(id, agent.Target{Net: network, Addr: addr})
		if errors.Is(err, agent.ErrOffline) {
			name := fmt.Sprintf("#%d", id)
			if a, e := st.GetAgent(id); e == nil {
				name = a.Name
			}
			err = fmt.Errorf("agent %s is offline — start it on the database host (see the Agents page)", name)
		}
		return host, port, err
	}
	// SQL Server's backup folder on the agent's host (--allow-mssql-dir)
	mssql.AgentFolder = func(id int64, dir string) (mssql.Files, error) {
		name := fmt.Sprintf("#%d", id)
		if a, e := st.GetAgent(id); e == nil {
			name = a.Name
		}
		f, err := agents.Folder(id, name, dir)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", name, err)
		}
		return f, nil
	}
	defer agents.Close()

	// built-in WireGuard (off until an admin turns it on)
	wgm := wg.NewManager()
	wgm.Logf = log.Printf
	wgm.OnEndpoint = st.SetWGPeerEndpoint
	store.WGEndpoint = func(id int64, addr string) (string, int, error) {
		host, port, err := wgm.Endpoint(id, addr)
		if errors.Is(err, wg.ErrDisabled) {
			err = errors.New("WireGuard is turned off (WireGuard page)")
		}
		return host, port, err
	}
	if cfg, err := api.WireGuardConfig(st); err != nil {
		log.Printf("WireGuard: %v", err)
	} else if cfg != nil {
		if err := wgm.Apply(cfg); err != nil {
			log.Printf("WireGuard did not start: %v", err)
		}
	}
	defer wgm.Close()

	srv := &api.Server{Cfg: cfg, Store: st, Runner: run, Sched: sched, Storage: sm, OAuth: storage.NewOAuthManager(),
		Tools: tools, Notifier: notifier, Limiter: auth.NewLimiter(), IPLimit: ipLimiter(), Version: version, WebFS: web.FS(),
		Agents: agents, WG: wgm, Started: time.Now()}
	// freshness and binlog lag alerts, the weekly digest and /metrics
	srv.Monitor = &monitor.Checker{Store: st, Runner: run, Send: notifier.Send, Digest: notifier.SendDigestAll,
		BaseURL: run.BaseURL, Disk: sysinfo.DiskUsage}
	if d := os.Getenv("DUMPCAT_WEB_DIR"); d != "" {
		srv.WebFS = os.DirFS(d) // development: edit UI without rebuilding
	}
	restartCh := make(chan *update.Pending, 1)
	upd := &update.Manager{Version: version, DataDir: cfg.DataDir, DBPath: cfg.Path("dumpcat.db"), Exe: exe,
		Server: func() (string, string) { return updateServer(st) }, Drain: run, Snapshot: st.Snapshot, Logf: log.Printf,
		PublicKeys: updateKeys,
		Restart: func(p *update.Pending) {
			select {
			case restartCh <- p:
			default:
			}
		}}
	// automatic updates: the policy is read from the settings at every look
	upd.Policy = func() update.AutoPolicy {
		set := st.GetSettings()
		return update.AutoPolicy{Check: set.UpdateAutoCheck, Install: set.UpdateAutoInstall,
			FromHour: set.UpdateFromHour, ToHour: set.UpdateToHour, Loc: run.Location()}
	}
	upd.Notify = func(r update.Result) {
		r.Message += " — DumpCat tries again in the next install window"
		notifier.Send(updateEvent(r, run.BaseURL()))
	}
	upd.Audit = func(action, details string) { st.Audit(nil, update.AutoBy, action, "system", details, "") }
	srv.Updater = upd
	// lets "dumpcat update" on this machine drive the update through the API
	srv.ControlToken = auth.RandomToken(32)
	if err := os.WriteFile(cfg.Path("control.token"), []byte(srv.ControlToken), 0o600); err != nil {
		log.Printf("cannot write %s: %v", cfg.Path("control.token"), err)
	}
	defer os.Remove(cfg.Path("control.token"))
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go upd.Auto(bgCtx)
	go srv.Monitor.Run(bgCtx)

	// while no account exists, creating the first admin requires a one-time token
	setupTok, err := srv.PrepareSetup(cfg.Path(api.SetupTokenFile))
	if err != nil {
		log.Printf("setup token: %v", err)
	}
	if setupTok != "" {
		again := "sudo dumpcat setup-token"
		if runtime.GOOS == "windows" {
			again = `& "` + exe + `" setup-token   (PowerShell as Administrator)`
		}
		log.Printf("============================================================")
		log.Printf(" No account yet. Open the web UI and create the first admin")
		log.Printf(" account with this one-time setup token:")
		log.Printf("")
		log.Printf("     %s", setupTok)
		log.Printf("")
		log.Printf(" Show it again with: %s", again)
		log.Printf("============================================================")
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	certFile, keyFile := cfg.TLSCert, cfg.TLSKey
	if certFile == "" || keyFile == "" {
		certFile, keyFile = "", ""
		if cfg.TLSAuto {
			if certFile, keyFile, err = ensureSelfSigned(cfg.Path("tls")); err != nil {
				return err
			}
		}
	}
	ln, err := listen(cfg.Listen, boot.Active())
	if err != nil {
		return err
	}
	scheme, note := "http", ""
	if certFile != "" {
		scheme = "https"
		if cfg.TLSAuto && cfg.TLSCert == "" {
			note = " (self-signed certificate)"
		}
	}
	log.Printf("DumpCat %s listening on %s://%s%s", version, scheme, displayAddr(cfg.Listen), note)
	errCh := make(chan error, 1)
	go func() {
		var err error
		if certFile != "" {
			err = httpSrv.ServeTLS(ln, certFile, keyFile)
		} else {
			err = httpSrv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	// the new version is up: finish the update and run what it postponed
	if boot.Active() {
		deferred := boot.Healthy()
		log.Printf("update: DumpCat %s started successfully", version)
		for _, d := range deferred {
			if _, err := run.StartBackup(d.JobID, d.Type, "schedule", "scheduler"); err != nil {
				log.Printf("postponed backup of job %d: %v", d.JobID, err)
			}
		}
	}
	// tell the notification channels how the last update or rollback ended (once)
	if r := update.TakeResultToAnnounce(cfg.DataDir, time.Now()); r != nil {
		notifier.Send(updateEvent(*r, run.BaseURL()))
	}
	go func() {
		time.Sleep(2 * time.Minute)
		run.Housekeeping()
	}()

	var restart *update.Pending
	select {
	case <-stop:
	case restart = <-restartCh:
	case err := <-errCh:
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	sched.Stop()
	run.Shutdown(30 * time.Second)
	if restart != nil {
		for _, d := range run.TakeDeferred() {
			restart.Deferred = append(restart.Deferred, update.Deferred{JobID: d.JobID, Type: d.Type})
		}
		if err := update.SavePending(cfg.DataDir, restart); err != nil {
			log.Printf("update: %v", err)
		}
		return &restartRequest{p: restart}
	}
	return nil
}

func ipLimiter() *auth.Limiter {
	l := auth.NewLimiter()
	l.MaxFails = 20
	return l
}

func displayAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "0.0.0.0" + listen
	}
	return listen
}

// ---- user CLI ----

func readPassword(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			log.Fatal(err)
		}
		return string(b)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func askNewPassword() string {
	p1 := readPassword("New password: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		p2 := readPassword("Repeat: ")
		if p1 != p2 {
			log.Fatal("passwords do not match")
		}
	}
	return p1
}

func userCmd(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("user "+sub, flag.ExitOnError)
	username := fs.String("u", "", "username")
	role := fs.String("r", store.RoleAdmin, "role: admin|operator|viewer")
	pass := fs.String("p", "", "password (not recommended — prompted when empty)")
	cfgFile := fs.String("config", config.DefaultEnvFile(), "env file")
	dataDir := fs.String("data-dir", "", "data directory")
	_ = fs.Parse(rest)
	cfgArgs := []string{"-config", *cfgFile}
	if *dataDir != "" {
		cfgArgs = append(cfgArgs, "-data-dir", *dataDir)
	}
	cfg := loadConfig("user", cfgArgs)
	st := openStore(cfg)
	defer st.Close()
	switch sub {
	case "list":
		users, err := st.ListUsers()
		if err != nil {
			log.Fatal(err)
		}
		for _, u := range users {
			flags := ""
			if u.TOTPEnabled {
				flags += " 2fa"
			}
			if u.Disabled {
				flags += " disabled"
			}
			fmt.Printf("%-24s %-9s%s\n", u.Username, u.Role, flags)
		}
	case "add":
		if *username == "" {
			log.Fatal("-u NAME is required")
		}
		if !store.ValidRole(*role) {
			log.Fatal("invalid role")
		}
		p := *pass
		if p == "" {
			p = askNewPassword()
		}
		hash, err := auth.HashPassword(p)
		if err != nil {
			log.Fatal(err)
		}
		u := &store.User{Username: *username, Role: *role, PasswordHash: hash}
		if err := st.CreateUser(u); err != nil {
			log.Fatal(err)
		}
		os.Remove(cfg.Path(api.SetupTokenFile)) // an account exists: the web setup is over
		st.Audit(nil, "cli", "user_create", "user:"+u.Username, "role="+u.Role, "cli")
		fmt.Println("Created user", u.Username, "("+u.Role+")")
	case "passwd", "reset-2fa", "enable", "disable":
		if *username == "" {
			log.Fatal("-u NAME is required")
		}
		u, err := st.GetUserByName(*username)
		if err != nil {
			log.Fatal("user not found: ", *username)
		}
		switch sub {
		case "passwd":
			p := *pass
			if p == "" {
				p = askNewPassword()
			}
			hash, err := auth.HashPassword(p)
			if err != nil {
				log.Fatal(err)
			}
			u.PasswordHash = hash
			u.Disabled = false
			st.DeleteUserSessions(u.ID, "")
			st.DeleteUserTokens(u.ID)
		case "reset-2fa":
			u.TOTPEnabled, u.TOTPSecret = false, ""
		case "enable":
			u.Disabled = false
		case "disable":
			u.Disabled = true
			st.DeleteUserSessions(u.ID, "")
			st.DeleteUserTokens(u.ID)
		}
		if err := st.UpdateUser(u); err != nil {
			log.Fatal(err)
		}
		st.Audit(nil, "cli", "user_"+sub, "user:"+u.Username, "", "cli")
		fmt.Println("OK")
	default:
		usage()
		os.Exit(2)
	}
}

// setupTokenCmd prints the one-time token that the web UI asks for before it
// creates the first admin. The server writes it to the data directory when it
// starts while no account exists.
func setupTokenCmd(args []string) error {
	fs := flag.NewFlagSet("setup-token", flag.ExitOnError)
	cfgFile := fs.String("config", config.DefaultEnvFile(), "env file")
	dataDir := fs.String("data-dir", "", "data directory")
	_ = fs.Parse(args)
	cfgArgs := []string{"-config", *cfgFile}
	if *dataDir != "" {
		cfgArgs = append(cfgArgs, "-data-dir", *dataDir)
	}
	cfg := loadConfig("setup-token", cfgArgs)
	st, err := openStoreErr(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.CountUsers()
	if err != nil {
		return err
	}
	if n > 0 {
		return errors.New("an account already exists, so setup is complete and needs no token (forgotten password: dumpcat user passwd -u NAME)")
	}
	b, err := os.ReadFile(cfg.Path(api.SetupTokenFile))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no setup token yet: start DumpCat, it creates one when it starts while no account exists")
	}
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return nil
}

// ---- decode CLI (disaster recovery without the database) ----

func decodeCmd(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	in := fs.String("in", "", "file backup (.sql.zst, .sql.zst.age, .xbstream.zst, .tar.age ...)")
	out := fs.String("out", "", "output file (default: input name without compression/encryption suffix; '-' = stdout)")
	passFile := fs.String("passphrase-file", "", "read the passphrase from a file (or set DUMPCAT_PASSPHRASE)")
	_ = fs.Parse(args)
	if *in == "" {
		return errors.New("-in FILE is required")
	}
	comp, enc := pipeline.DetectFromName(*in)
	pass := os.Getenv("DUMPCAT_PASSPHRASE")
	if enc && pass == "" {
		if *passFile != "" {
			b, err := os.ReadFile(*passFile)
			if err != nil {
				return err
			}
			pass = strings.TrimRight(string(b), "\r\n")
		} else {
			pass = readPassword("Passphrase: ")
		}
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	rd, cleanup, err := pipeline.NewReader(bufio.NewReaderSize(f, 1<<20), comp, enc, pass)
	if err != nil {
		return err
	}
	defer cleanup()
	dst := *out
	if dst == "" {
		dst = pipeline.StripExt(*in)
		if dst == *in {
			return errors.New("file is neither compressed nor encrypted — nothing to decode")
		}
	}
	var w io.Writer
	if dst == "-" {
		w = os.Stdout
	} else {
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("%s already exists", dst)
		}
		of, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer of.Close()
		bw := bufio.NewWriterSize(of, 1<<20)
		defer bw.Flush()
		w = bw
	}
	n, err := io.Copy(w, rd)
	if err != nil {
		return err
	}
	if dst != "-" {
		fmt.Fprintf(os.Stderr, "Wrote %s (%d bytes)\n", filepath.Base(dst), n)
	}
	return nil
}
