// Package agent reaches database servers that accept no inbound
// connections. `dumpcat agent` runs next to the database and connects out to
// the DumpCat server over HTTPS (WebSocket). The server then gives every
// agent target a local 127.0.0.1 port: each connection to that port asks the
// agent to dial the database and opens a second outbound WebSocket that
// carries the bytes. Everything that talks to MySQL (Go driver, mysqldump,
// mydumper, mysqlbinlog, mysql) works through it unchanged.
//
// The agent only dials the addresses on its own allow list, so a compromised
// DumpCat server cannot use it to reach the rest of the agent's network.
package agent

import (
	"errors"
	"fmt"
	"net"
	"path"
	"strings"
)

const (
	PathConnect = "/api/agent/connect" // control connection (one per agent)
	PathStream  = "/api/agent/stream"  // one per tunneled database connection
	KeyPrefix   = "dca_"
)

// Message is a control frame (JSON text frames on the control connection).
type Message struct {
	Type     string   `json:"type"` // hello, open, error, ping, pong
	ID       string   `json:"id,omitempty"`
	Net      string   `json:"net,omitempty"` // tcp or unix
	Addr     string   `json:"addr,omitempty"`
	Error    string   `json:"error,omitempty"`
	Version  string   `json:"version,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	OS       string   `json:"os,omitempty"`
	Allow    []string `json:"allow,omitempty"`
}

// Target is an address the agent dials on its side.
type Target struct {
	Net  string // tcp or unix
	Addr string // host:port or socket path
}

func (t Target) String() string {
	if t.Net == "unix" {
		return "unix:" + t.Addr
	}
	return t.Addr
}

// NormalizeTarget canonicalizes a target so allow-list checks compare like
// with like ("localhost:3306" == "127.0.0.1:3306").
func NormalizeTarget(t Target) (Target, error) {
	switch t.Net {
	case "unix":
		if t.Addr == "" || !strings.HasPrefix(t.Addr, "/") && !strings.Contains(t.Addr, `:\`) {
			return t, fmt.Errorf("socket path must be absolute: %q", t.Addr)
		}
		return Target{Net: "unix", Addr: path.Clean(strings.ReplaceAll(t.Addr, `\`, "/"))}, nil
	case "tcp", "":
		host, port, err := net.SplitHostPort(t.Addr)
		if err != nil {
			return t, fmt.Errorf("invalid address %q: %w", t.Addr, err)
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if host == "" || host == "localhost" {
			host = "127.0.0.1"
		}
		if port == "" || strings.Trim(port, "0123456789") != "" {
			return t, fmt.Errorf("invalid port in %q", t.Addr)
		}
		return Target{Net: "tcp", Addr: net.JoinHostPort(host, port)}, nil
	}
	return t, fmt.Errorf("unknown network %q", t.Net)
}

// ParseTarget reads "host:port", "unix:/path/to/socket" or a bare absolute
// socket path.
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "unix:"):
		return NormalizeTarget(Target{Net: "unix", Addr: strings.TrimPrefix(s, "unix:")})
	case strings.HasPrefix(s, "/"):
		return NormalizeTarget(Target{Net: "unix", Addr: s})
	}
	return NormalizeTarget(Target{Net: "tcp", Addr: s})
}

// DefaultAllow is what an agent may dial when it is not configured: the local
// MySQL/MariaDB port and the usual socket paths.
var DefaultAllow = []string{
	"127.0.0.1:3306", "[::1]:3306",
	"unix:/var/run/mysqld/mysqld.sock", "unix:/run/mysqld/mysqld.sock",
	"unix:/var/lib/mysql/mysql.sock", "unix:/tmp/mysql.sock",
}

// AllowList decides which targets an agent dials. "*" allows everything.
type AllowList struct {
	any     bool
	targets map[Target]bool
	list    []string
}

func ParseAllowList(items []string) (*AllowList, error) {
	a := &AllowList{targets: map[Target]bool{}}
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if it == "*" {
			a.any = true
			a.list = append(a.list, "*")
			continue
		}
		t, err := ParseTarget(it)
		if err != nil {
			return nil, err
		}
		if !a.targets[t] {
			a.targets[t] = true
			a.list = append(a.list, t.String())
		}
	}
	if !a.any && len(a.targets) == 0 {
		return nil, errors.New("the allow list is empty")
	}
	return a, nil
}

func (a *AllowList) Allows(t Target) bool {
	if a.any {
		return true
	}
	n, err := NormalizeTarget(t)
	return err == nil && a.targets[n]
}

func (a *AllowList) Strings() []string { return append([]string(nil), a.list...) }

var ErrOffline = errors.New("the agent is offline")
