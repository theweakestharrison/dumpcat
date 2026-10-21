// Package mssql is the Microsoft SQL Server driver: inspection, native
// BACKUP DATABASE / BACKUP LOG and RESTORE over TDS (go-mssqldb, no client
// tools), with the backup files passed through a folder that both SQL Server
// and DumpCat reach (see folder.go), restore drills and point-in-time
// recovery from scheduled log backups.
package mssql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/golang-sql/sqlexp"
	mssqldb "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	"dumpcat/internal/store"
)

// splitInstance cuts "host\INSTANCE" into the host and the instance name.
func splitInstance(h string) (host, instance string) {
	host, instance, _ = strings.Cut(strings.TrimSpace(h), `\`)
	return host, instance
}

// config builds the connection of a server. Nothing comes from a
// connection string: the values are set field by field, so no name or
// password can add options of its own.
func config(s *store.Server, database string) (msdsn.Config, error) {
	cfg, err := msdsn.Parse("sqlserver://localhost?encrypt=disable&app+name=DumpCat")
	if err != nil {
		return cfg, err
	}
	host, instance := splitInstance(s.Host)
	port := s.Port
	sv := *s
	sv.Host = host
	if instance != "" && port == 0 && !s.Tunneled() {
		// a named instance without a port: ask the SQL Server Browser (UDP 1434)
		if host == "" || host == "localhost" {
			host = "127.0.0.1"
		}
		cfg.Host, cfg.Instance, cfg.Port = host, instance, 0
	} else {
		h, p, _, err := sv.Addr()
		if err != nil {
			return cfg, err
		}
		cfg.Host, cfg.Port, cfg.Instance = h, uint64(p), ""
	}
	for _, v := range []string{s.Username, database} {
		if strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return cfg, fmt.Errorf("server %s: the user or database name contains a control character", s.Name)
		}
	}
	cfg.User, cfg.Password, cfg.Database = s.Username, s.Password, database
	cfg.DialTimeout = 15 * time.Second
	cfg.KeepAlive = 30 * time.Second
	cfg.DisableRetry = false // database/sql retries only what never reached the server (a pooled connection that died)
	// the certificate names the server, also when DumpCat reaches it through a tunnel
	name := host
	if name == "" || name == "localhost" {
		name = cfg.Host
	}
	switch s.TLSMode {
	case "disabled":
		cfg.Encryption, cfg.TLSConfig = msdsn.EncryptionDisabled, nil
	case "verify":
		tc, err := tlsConfig(s, name, true)
		if err != nil {
			return cfg, err
		}
		cfg.Encryption, cfg.TLSConfig, cfg.TrustServerCertificate = msdsn.EncryptionRequired, tc, false
		cfg.HostInCertificateProvided = true
	default: // "" and required: everything encrypted, the certificate not checked (SQL Server's own is self-signed)
		tc, _ := tlsConfig(s, name, false)
		cfg.Encryption, cfg.TLSConfig, cfg.TrustServerCertificate = msdsn.EncryptionRequired, tc, true
	}
	return cfg, nil
}

func tlsConfig(s *store.Server, serverName string, verify bool) (*tls.Config, error) {
	// one TLS record per TDS packet: SQL Server expects them whole
	tc := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12, DynamicRecordSizingDisabled: true}
	if !verify {
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

// Open returns a small pool of connections to database ("" = master) and
// checks that one connects.
func Open(ctx context.Context, s *store.Server, database string) (*sql.DB, error) {
	if database == "" {
		database = "master"
	}
	cfg, err := config(s, database)
	if err != nil {
		return nil, err
	}
	db, err := ping(ctx, cfg)
	if err != nil && s.TLSMode == "" && strings.Contains(err.Error(), "does not support encryption") {
		// like PostgreSQL's sslmode=prefer: a server without TLS still gets the login encrypted
		cfg.Encryption = msdsn.EncryptionOff
		db, err = ping(ctx, cfg)
	}
	return db, err
}

func ping(ctx context.Context, cfg msdsn.Config) (*sql.DB, error) {
	db := sql.OpenDB(mssqldb.NewConnectorConfig(cfg))
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(cctx); err != nil {
		db.Close()
		var e mssqldb.Error
		if errors.As(err, &e) {
			return nil, errors.New(errText(err)) // "Login failed for user 'x'.", without the driver's prefix
		}
		return nil, err
	}
	return db, nil
}

// QuoteName quotes an identifier: [name], with ] doubled.
func QuoteName(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

// QuoteString quotes a Unicode string constant: N'…', with ' doubled.
func QuoteString(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }

// exec runs a batch and hands every informational message (BACKUP's
// "10 percent processed.") to notice. The error holds every message of a
// failure ("Cannot open backup device … Operating system error 5", then
// "BACKUP DATABASE is terminating abnormally").
func exec(ctx context.Context, db *sql.DB, q string, notice func(string)) error {
	msg := &sqlexp.ReturnMessage{}
	rows, err := db.QueryContext(ctx, q, msg)
	if err != nil {
		return err
	}
	defer rows.Close()
	var failed error
	for active := true; active; {
		switch m := msg.Message(ctx).(type) {
		case sqlexp.MsgNotice:
			if notice != nil {
				notice(strings.TrimSpace(m.Message.String()))
			}
		case sqlexp.MsgNext:
			for rows.Next() {
			}
		case sqlexp.MsgNextResultSet:
			active = rows.NextResultSet()
		case sqlexp.MsgError:
			failed = m.Error
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed != nil {
		return failed
	}
	return rows.Err()
}

// errorNumbers lists the SQL Server error numbers of err (all of a batch).
func errorNumbers(err error) []int32 {
	var e mssqldb.Error
	if !errors.As(err, &e) {
		return nil
	}
	if len(e.All) == 0 {
		return []int32{e.Number}
	}
	out := make([]int32, len(e.All))
	for i, x := range e.All {
		out[i] = x.Number
	}
	return out
}

func hasError(err error, nums ...int32) bool {
	for _, n := range errorNumbers(err) {
		for _, want := range nums {
			if n == want {
				return true
			}
		}
	}
	return false
}

// errText is a SQL Server error without the driver's "mssql: " prefixes,
// the messages of one batch joined.
func errText(err error) string {
	var e mssqldb.Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	all := e.All
	if len(all) == 0 {
		all = []mssqldb.Error{e}
	}
	parts := make([]string, 0, len(all))
	for _, x := range all {
		m := strings.TrimSpace(x.Message)
		if m != "" && !strings.HasSuffix(m, "terminating abnormally.") || len(all) == 1 {
			parts = append(parts, m)
		}
	}
	return strings.Join(parts, " ")
}

// queryString runs a query returning one value, "" on any error or NULL.
func queryString(ctx context.Context, db *sql.DB, q string, args ...any) string {
	var v sql.NullString
	if err := db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
		return ""
	}
	return v.String
}

func queryInt(ctx context.Context, db *sql.DB, q string, args ...any) (int64, bool) {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil || !v.Valid {
		return 0, false
	}
	return v.Int64, true
}

// tcpAddr names where a server is reached, for messages.
func tcpAddr(cfg msdsn.Config) string {
	if cfg.Instance != "" {
		return cfg.Host + `\` + cfg.Instance
	}
	return net.JoinHostPort(cfg.Host, strconv.FormatUint(cfg.Port, 10))
}
