// Package pgsql is the PostgreSQL driver: inspection over pgx, backups with
// pg_dump (directory format, parallel) and pg_dumpall --globals-only,
// restores with pg_restore, and the queries of restore drills.
package pgsql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dumpcat/internal/store"
)

// endpoint is where a server is reached: a TCP address (direct, or the
// local end of an agent or WireGuard tunnel) or a unix socket directory.
type endpoint struct {
	host     string // address to connect to, or the socket directory
	port     int
	socket   bool
	tlsName  string // the server's own name, checked against its certificate
	tunneled bool
}

func endpointOf(s *store.Server) (endpoint, error) {
	host, port, socket, err := s.Addr()
	if err != nil {
		return endpoint{}, err
	}
	e := endpoint{host: host, port: port, tlsName: s.Host, tunneled: s.Tunneled()}
	if socket != "" {
		// PostgreSQL names the directory; accept the socket file itself too
		e.host, e.socket, e.port = socket, true, s.Port
		if b := path.Base(socket); strings.HasPrefix(b, ".s.PGSQL.") { // a unix path on every OS
			e.host = path.Dir(socket)
			if p, err := strconv.Atoi(strings.TrimPrefix(b, ".s.PGSQL.")); err == nil {
				e.port = p
			}
		}
		if e.port == 0 {
			e.port = 5432
		}
	}
	if e.tlsName == "" || e.tlsName == "localhost" {
		e.tlsName = e.host
	}
	return e, nil
}

func tlsConfig(s *store.Server, serverName string) (*tls.Config, error) {
	tc := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if s.TLSMode != "verify" {
		tc.InsecureSkipVerify = true
		return tc, nil
	}
	if strings.TrimSpace(s.CACert) != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(s.CACert)) {
			return nil, errors.New("invalid CA certificate")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}

// Connect opens one connection to database (empty: the maintenance database
// postgres). Nothing is read from the environment or a password file.
func Connect(ctx context.Context, s *store.Server, database string) (*pgx.Conn, error) {
	cfg, err := connConfig(s, database)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return pgx.ConnectConfig(cctx, cfg)
}

// replicationConn opens a physical replication connection (the walsender
// protocol of pg_receivewal and pg_basebackup).
func replicationConn(ctx context.Context, s *store.Server) (*pgconn.PgConn, error) {
	cfg, err := connConfig(s, "postgres")
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["replication"] = "true"
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return pgconn.ConnectConfig(cctx, &cfg.Config)
}

func connConfig(s *store.Server, database string) (*pgx.ConnConfig, error) {
	e, err := endpointOf(s)
	if err != nil {
		return nil, err
	}
	if database == "" {
		database = "postgres"
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port = e.host, uint16(e.port)
	cfg.User, cfg.Password, cfg.Database = s.Username, s.Password, database
	cfg.ConnectTimeout = 15 * time.Second
	cfg.RuntimeParams = map[string]string{"application_name": "dumpcat"}
	cfg.Fallbacks = nil
	cfg.TLSConfig = nil
	cfg.ValidateConnect, cfg.AfterConnect = nil, nil
	if !e.socket {
		cfg.DialFunc = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 5 * time.Minute}).DialContext
		switch s.TLSMode {
		case "disabled":
		case "required", "verify":
			if cfg.TLSConfig, err = tlsConfig(s, e.tlsName); err != nil {
				return nil, err
			}
		default: // like libpq's sslmode=prefer: TLS when the server offers it
			tc, _ := tlsConfig(s, e.tlsName)
			cfg.TLSConfig = tc
			cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: e.host, Port: uint16(e.port)}}
		}
	}
	return cfg, nil
}

// adminConn connects to a database every role may reach for CREATE/DROP
// DATABASE: postgres, else template1.
func adminConn(ctx context.Context, s *store.Server) (*pgx.Conn, error) {
	c, err := Connect(ctx, s, "postgres")
	if err == nil {
		return c, nil
	}
	if c2, err2 := Connect(ctx, s, "template1"); err2 == nil {
		return c2, nil
	}
	return nil, err
}

// conninfoValue quotes a value for a libpq connection string.
func conninfoValue(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// ConnInfo builds the libpq connection string the client tools get with
// --dbname. Every value is quoted, so a database name can never add
// connection options of its own (pg_dump -d "a=b" would read it as one).
// The password is not in it: see WritePassFile. dir receives the CA file.
func ConnInfo(s *store.Server, database, dir string) (string, error) {
	e, err := endpointOf(s)
	if err != nil {
		return "", err
	}
	for _, v := range []string{s.Username, database, e.host} {
		if strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", fmt.Errorf("server %s: the user, host or database name contains a control character", s.Name)
		}
	}
	kv := [][2]string{}
	add := func(k, v string) { kv = append(kv, [2]string{k, v}) }
	if e.tunneled && s.TLSMode == "verify" && !e.socket && e.tlsName != e.host {
		// connect to the tunnel, check the certificate against the real name
		add("host", e.tlsName)
		add("hostaddr", e.host)
	} else {
		add("host", e.host)
	}
	add("port", strconv.Itoa(e.port))
	add("user", s.Username)
	add("dbname", database)
	add("connect_timeout", "15")
	add("application_name", "dumpcat")
	if !e.socket {
		switch s.TLSMode {
		case "disabled":
			add("sslmode", "disable")
		case "required":
			add("sslmode", "require")
		case "verify":
			add("sslmode", "verify-full")
			if strings.TrimSpace(s.CACert) != "" {
				ca := filepath.Join(dir, "pg-ca.pem")
				if err := os.WriteFile(ca, []byte(s.CACert), 0o600); err != nil {
					return "", err
				}
				add("sslrootcert", ca)
			} else {
				add("sslrootcert", "system") // libpq 16+: the operating system's CAs
			}
		default:
			add("sslmode", "prefer")
		}
	}
	parts := make([]string, len(kv))
	for i, p := range kv {
		parts[i] = p[0] + "=" + conninfoValue(p[1])
	}
	return strings.Join(parts, " "), nil
}

// passFileValue escapes a field of a .pgpass line.
func passFileValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(v)
}

// WritePassFile writes the server's password into a private (0600) password
// file and returns the environment the client tools need to use it: the
// password never appears on a command line or in the environment.
func WritePassFile(dir string, s *store.Server) ([]string, error) {
	// an empty PGPASSWORD makes libpq read the file below (a PGPASSWORD of the
	// service's own environment would win over it)
	env := []string{"PGPASSWORD="}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if s.Password == "" {
		// no password: never fall back to the service user's own ~/.pgpass
		return append(env, "PGPASSFILE="+filepath.Join(dir, "no-pgpass")), nil
	}
	if strings.ContainsAny(s.Password, "\r\n") || strings.ContainsAny(s.Username, "\r\n") {
		return nil, fmt.Errorf("server %s: the password contains a line break, which PostgreSQL's password file cannot hold", s.Name)
	}
	f, err := os.CreateTemp(dir, "pgpass-*")
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	line := "*:*:*:" + passFileValue(s.Username) + ":" + passFileValue(s.Password) + "\n"
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return append(env, "PGPASSFILE="+f.Name()), nil
}

// QuoteIdent quotes a PostgreSQL identifier.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteLiteral quotes a string constant (standard_conforming_strings, E” for backslashes).
func QuoteLiteral(s string) string {
	q := "'" + strings.ReplaceAll(s, "'", "''") + "'"
	if strings.Contains(s, `\`) {
		return "E" + strings.ReplaceAll(q, `\`, `\\`)
	}
	return q
}
