package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dumpcat/internal/agent"
	"dumpcat/internal/config"
)

func defaultAgentEnvFile() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(config.WindowsRoot(), "agent.env")
	}
	return "/etc/dumpcat/agent.env"
}

// agentCmd runs `dumpcat agent` on a database host that accepts no inbound
// connections: it connects out to the DumpCat server and forwards the
// database connections the server asks for.
func agentCmd(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	envFile := fs.String("config", defaultAgentEnvFile(), "env file with the DUMPCAT_AGENT_* settings")
	server := fs.String("server", "", "DumpCat URL, e.g. https://backup.example.com (DUMPCAT_AGENT_SERVER)")
	key := fs.String("key", "", "agent key from the Agents page (DUMPCAT_AGENT_KEY; prefer the env file: a key on the command line is visible to other users)")
	allow := fs.String("allow", "", "database addresses this agent may connect to, comma separated: host:port or unix:/path (DUMPCAT_AGENT_ALLOW; default "+strings.Join(agent.DefaultAllow, ",")+")")
	insecure := fs.Bool("insecure", false, "accept the server's certificate without verifying it, e.g. self-signed (DUMPCAT_AGENT_INSECURE)")
	logFile := fs.String("log", "", "also write the log to this file (DUMPCAT_AGENT_LOG)")
	allowPhysical := fs.Bool("allow-physical", false, "let DumpCat run physical backups (mariabackup/xtrabackup) on this host and stream them back; the agent must be able to read the datadir (DUMPCAT_AGENT_ALLOW_PHYSICAL)")
	datadir := fs.String("datadir", "", "datadir the Agents page reports as readable or not (DUMPCAT_AGENT_DATADIR; default /var/lib/mysql or the usual location)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: dumpcat agent [flags]

Runs on a database host that accepts no inbound connections. The agent connects
out to the DumpCat server over HTTPS and opens the database connections the
server asks for — only to the addresses on its allow list. With -allow-physical
it also runs physical backups (mariabackup/xtrabackup) here for DumpCat.

Flags (or DUMPCAT_AGENT_* variables in %s):
`, defaultAgentEnvFile())
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if err := config.LoadEnvFile(*envFile); err != nil {
		// the systemd unit runs as an unprivileged user and gets the
		// settings from EnvironmentFile= instead
		if !os.IsPermission(err) || os.Getenv("DUMPCAT_AGENT_KEY") == "" {
			log.Fatalf("cannot read %s: %v", *envFile, err)
		}
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	pick := func(flagName, cur, env string) string {
		if set[flagName] {
			return cur
		}
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
		return cur
	}
	*server = pick("server", *server, "DUMPCAT_AGENT_SERVER")
	*key = pick("key", *key, "DUMPCAT_AGENT_KEY")
	*allow = pick("allow", *allow, "DUMPCAT_AGENT_ALLOW")
	*logFile = pick("log", *logFile, "DUMPCAT_AGENT_LOG")
	*datadir = pick("datadir", *datadir, "DUMPCAT_AGENT_DATADIR")
	envBool := func(flagName string, cur *bool, env string) {
		if !set[flagName] {
			switch strings.ToLower(os.Getenv(env)) {
			case "1", "true", "yes", "on":
				*cur = true
			}
		}
	}
	envBool("insecure", insecure, "DUMPCAT_AGENT_INSECURE")
	envBool("allow-physical", allowPhysical, "DUMPCAT_AGENT_ALLOW_PHYSICAL")
	if *server == "" || *key == "" {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "\nerror: the server URL and the agent key are required")
		os.Exit(2)
	}
	items := agent.DefaultAllow
	if *allow != "" {
		items = strings.Split(*allow, ",")
	}
	al, err := agent.ParseAllowList(items)
	if err != nil {
		log.Fatalf("allow list: %v", err)
	}
	service := inService()
	if *logFile == "" && service {
		*logFile = filepath.Join(config.WindowsRoot(), "agent.log") // a service has no console
	}
	if *logFile != "" {
		lf, err := openRotatingFile(*logFile, 10<<20, 3)
		if err != nil {
			log.Fatal(err)
		}
		if service {
			log.SetOutput(lf)
		} else {
			log.SetOutput(io.MultiWriter(os.Stderr, lf))
		}
	}
	if *insecure {
		log.Printf("warning: the server certificate is not verified (-insecure)")
	}
	cl := &agent.Client{Server: *server, Key: *key, Allow: al, Version: version, Insecure: *insecure, Logf: log.Printf,
		AllowPhysical: *allowPhysical, Datadir: *datadir}
	run := func(stop <-chan struct{}) error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-stop
			cancel()
		}()
		log.Printf("DumpCat agent %s — connecting to %s", version, *server)
		return cl.Run(ctx)
	}
	if service {
		err = runService(run)
	} else {
		err = run(signalStop())
	}
	if err != nil {
		log.Fatal(err)
	}
}
