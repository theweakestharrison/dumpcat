package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"dumpcat/internal/config"
	"dumpcat/internal/update"
)

// updateCmd: "dumpcat update [-check] [-yes] [-force] [-rollback] [-url URL]".
// With the server running it drives the update through the local API (so running
// backups are waited for); with the server stopped it updates the files directly.
func updateCmd(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only check whether a new version is available")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	force := fs.Bool("force", false, "reinstall even if the release is not newer")
	rollback := fs.Bool("rollback", false, "go back to the version (and database) from before the last update")
	server := fs.String("url", "", "release server (default: Settings → Updates, DUMPCAT_UPDATE_URL)")
	cfgFile := fs.String("config", config.DefaultEnvFile(), "env file")
	dataDir := fs.String("data-dir", "", "data directory")
	_ = fs.Parse(args)
	cfgArgs := []string{"-config", *cfgFile}
	if *dataDir != "" {
		cfgArgs = append(cfgArgs, "-data-dir", *dataDir)
	}
	cfg := loadConfig("update", cfgArgs)
	log.SetFlags(0)

	c, err := localServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if c != nil {
		err = c.run(ctx, *check, *yes, *force, *rollback, *server)
	} else {
		err = offlineUpdate(ctx, cfg, *check, *yes, *force, *rollback, *server)
	}
	if err != nil {
		log.Fatal("error: ", err)
	}
}

func confirm(question string, yes bool) bool {
	if yes {
		return true
	}
	fmt.Print(question + " [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func printRelease(st update.Status) {
	fmt.Printf("Installed: DumpCat %s (%s)\n", st.Current, st.Platform)
	if st.Server == "" {
		fmt.Println("Release server: not configured (Settings → Updates, DUMPCAT_UPDATE_URL or -url)")
	} else {
		fmt.Printf("Release server: %s\n", st.Server)
	}
	if st.Signing.Required {
		fmt.Printf("Release signature: required (%d trusted key(s))\n", len(st.Signing.Keys))
	} else {
		fmt.Println("Release signature: not checked — no public key configured, integrity relies on HTTPS + SHA-256")
	}
	if st.CheckError != "" {
		fmt.Println("Check failed:", st.CheckError)
		return
	}
	if st.Latest == nil {
		return
	}
	if st.Available {
		fmt.Printf("New version available: %s", st.Latest.Version)
		if st.Latest.ReleasedAt != "" {
			fmt.Printf(" (released %s)", st.Latest.ReleasedAt)
		}
		fmt.Println()
		if st.Latest.SignedBy != "" {
			fmt.Printf("Signature OK (key %s)\n", st.Latest.SignedBy)
		}
		if n := strings.TrimSpace(st.Latest.Notes); n != "" {
			fmt.Println()
			fmt.Println(n)
			fmt.Println()
		}
	} else {
		fmt.Printf("Up to date (latest release: %s)\n", st.Latest.Version)
	}
}

// ---- running server ----

type localClient struct {
	base  string
	token string
	tok   func() string
	http  *http.Client
}

// localServer returns a client for the DumpCat server running on this machine,
// nil when it is not running.
func localServer(cfg *config.Config) (*localClient, error) {
	tokPath := cfg.Path("control.token")
	readTok := func() string {
		b, _ := os.ReadFile(tokPath)
		return strings.TrimSpace(string(b))
	}
	tok := readTok()
	if tok == "" {
		if _, err := os.Stat(tokPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot read %s (run as root / Administrator): %v", tokPath, err)
		}
		return nil, nil
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q", cfg.Listen)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	c := &localClient{base: scheme + "://" + net.JoinHostPort(host, port) + "/api", token: tok, tok: readTok,
		http: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
			// local connection to our own server, often with a self-signed certificate
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}
	var st update.Status
	if err := c.call("GET", "/update", nil, &st); err != nil {
		if errors.Is(err, errUnreachable) {
			return nil, fmt.Errorf("%s exists but DumpCat does not answer at %s — if it is not running, delete that file and retry", tokPath, c.base)
		}
		return nil, err
	}
	return c, nil
}

var errUnreachable = errors.New("server unreachable")

func (c *localClient) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnreachable, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *localClient) run(ctx context.Context, check, yes, force, rollback bool, server string) error {
	var st update.Status
	if rollback {
		if err := c.call("GET", "/update", nil, &st); err != nil {
			return err
		}
		if !st.Rollback.Available {
			return errors.New("rollback is not possible: " + st.Rollback.Reason)
		}
		fmt.Printf("Roll back DumpCat %s → %s and restore the database from before the update.\n", st.Current, st.Rollback.Version)
		fmt.Println("Changes made since the update (backup history, settings) will be lost.")
		if !confirm("Continue?", yes) {
			return errors.New("cancelled")
		}
		if err := c.call("POST", "/update/rollback", map[string]any{}, &st); err != nil {
			return err
		}
		return c.follow(ctx, st.Current, st.Rollback.Version)
	}
	if err := c.call("POST", "/update/check", map[string]any{"url": server}, &st); err != nil {
		return err
	}
	printRelease(st)
	if check || st.CheckError != "" {
		if st.CheckError != "" {
			return errors.New("update check failed")
		}
		return nil
	}
	if !st.Available && !force {
		return nil
	}
	if st.Latest == nil {
		return errors.New("no release information")
	}
	if !confirm(fmt.Sprintf("Install DumpCat %s now? Running backups are waited for, then DumpCat restarts.", st.Latest.Version), yes) {
		return errors.New("cancelled")
	}
	if err := c.call("POST", "/update/install", map[string]any{"url": server, "force": force}, &st); err != nil {
		return err
	}
	return c.follow(ctx, st.Current, st.Latest.Version)
}

// follow prints the progress until the server restarts into target.
func (c *localClient) follow(ctx context.Context, from, target string) error {
	last := ""
	for {
		select {
		case <-ctx.Done():
			if err := c.call("POST", "/update/cancel", map[string]any{}, nil); err != nil {
				return fmt.Errorf("could not cancel: %v", err)
			}
			return errors.New("cancelled")
		case <-time.After(time.Second):
		}
		var st update.Status
		if err := c.call("GET", "/update", nil, &st); err != nil {
			if errors.Is(err, errUnreachable) {
				break // restarting
			}
			return err
		}
		s := st.State
		line := s.Phase
		switch s.Phase {
		case "downloading":
			if s.Total > 0 {
				line = fmt.Sprintf("Downloading %d%%", s.Done*100/s.Total)
			} else {
				line = fmt.Sprintf("Downloading %d MB", s.Done>>20)
			}
		case "waiting":
			line = "Waiting for: " + strings.Join(s.Waiting, ", ")
		case "failed":
			return errors.New(s.Error)
		default:
			if s.Message != "" {
				line = s.Message
			}
		}
		if line != last {
			fmt.Println(line)
			last = line
		}
		if s.Phase == "restarting" {
			break
		}
	}
	return c.waitRestart(from, target)
}

// waitRestart waits for the server to come back and prints the outcome.
func (c *localClient) waitRestart(from, target string) error {
	fmt.Println("Waiting for DumpCat to restart…")
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var auth struct {
			Version string `json:"version"`
		}
		req, _ := http.NewRequest("GET", c.base+"/auth/status", nil)
		resp, err := c.http.Do(req)
		if err != nil {
			continue
		}
		json.NewDecoder(resp.Body).Decode(&auth)
		resp.Body.Close()
		if auth.Version == from {
			// still the old process, or the new one was rolled back: look at the result
			c.token = c.tok()
			var st update.Status
			if c.call("GET", "/update", nil, &st) == nil && st.State.Phase != "restarting" && st.Last != nil && st.Last.ToVersion == target {
				return report(st.Last)
			}
			continue
		}
		if auth.Version != "" {
			c.token = c.tok()
			var st update.Status
			if c.call("GET", "/update", nil, &st) == nil && st.Last != nil {
				return report(st.Last)
			}
			fmt.Printf("DumpCat %s is running.\n", auth.Version)
			return nil
		}
	}
	return errors.New("DumpCat did not come back within 4 minutes — check its log")
}

func report(r *update.Result) error {
	switch r.Status {
	case "success":
		fmt.Printf("Updated: DumpCat %s → %s.\n", r.FromVersion, r.ToVersion)
		return nil
	case "rolled_back":
		if r.Kind == "rollback" {
			fmt.Printf("Rolled back: DumpCat %s → %s.\n", r.FromVersion, r.ToVersion)
			return nil
		}
		return errors.New(r.Message)
	}
	return errors.New(r.Message)
}

// ---- server not running ----

func offlineUpdate(ctx context.Context, cfg *config.Config, check, yes, force, rollback bool, server string) error {
	keys, err := trustedUpdateKeys()
	if err != nil {
		return err
	}
	st, err := openStoreErr(cfg)
	if err != nil {
		return err
	}
	o := &update.Offline{Version: version, DataDir: cfg.DataDir, DBPath: cfg.Path("dumpcat.db"), Exe: executablePath(),
		Snapshot: st.Snapshot, PublicKeys: keys}
	fmt.Println("DumpCat is not running: updating the installed files directly.")
	if rollback {
		st.Close()
		if !confirm("Roll back to the previous version and restore its database? Changes since the update will be lost.", yes) {
			return errors.New("cancelled")
		}
		v, err := o.Rollback("dumpcat update -rollback")
		if err != nil {
			return err
		}
		fmt.Printf("Rolled back to DumpCat %s. Start the service again.\n", v)
		return nil
	}
	defer st.Close()
	if server == "" {
		server, _ = updateServer(st)
	}
	rel, err := o.Check(ctx, server)
	status := update.Status{Current: version, Platform: update.Platform(), Server: server, Signing: update.Signing(keys)}
	if err != nil {
		status.CheckError = err.Error()
	} else {
		status.Latest = rel
		status.Available = update.Compare(rel.Version, version) > 0
	}
	printRelease(status)
	if err != nil {
		return errors.New("update check failed")
	}
	if check || (!status.Available && !force) {
		return nil
	}
	if !confirm(fmt.Sprintf("Install DumpCat %s?", rel.Version), yes) {
		return errors.New("cancelled")
	}
	last := -1
	err = o.Install(ctx, rel, "dumpcat update", func(done, total int64) {
		if total > 0 {
			if p := int(done * 100 / total); p/10 != last/10 {
				last = p
				fmt.Printf("Downloading %d%%\n", p)
			}
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("DumpCat %s installed (the database was backed up first). Start the service to finish the update;\n", rel.Version)
	fmt.Println("if the new version does not start, the previous one is restored automatically.")
	return nil
}
