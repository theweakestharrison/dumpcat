// Package dbconn talks to MySQL/MariaDB servers through the Go driver for
// connection tests, inspection and small administrative statements.
package dbconn

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"dumpcat/internal/store"
)

var SystemDatabases = map[string]bool{
	"information_schema": true,
	"performance_schema": true,
	"sys":                true,
	"mysql":              true,
}

func IsSystemDB(name string) bool {
	return SystemDatabases[strings.ToLower(name)] || strings.HasPrefix(name, "#")
}

// Open connects to a server. database may be empty.
func Open(ctx context.Context, s *store.Server, database string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = s.Username
	cfg.Passwd = s.Password
	host, port, socket, err := s.Addr() // direct, or the local end of an agent tunnel
	if err != nil {
		return nil, err
	}
	if socket != "" {
		cfg.Net = "unix"
		cfg.Addr = socket
	} else {
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	}
	cfg.DBName = database
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 0
	cfg.ParseTime = false
	cfg.AllowNativePasswords = true
	cfg.AllowCleartextPasswords = s.TLSMode == "required" || s.TLSMode == "verify"
	cfg.MultiStatements = false
	cfg.InterpolateParams = true
	cfg.Collation = "utf8mb4_general_ci" // charset utf8mb4 (supported by MySQL 5.5+ and all MariaDB)
	switch s.TLSMode {
	case "required":
		cfg.TLSConfig = "skip-verify"
	case "verify":
		tc := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
		if strings.TrimSpace(s.CACert) != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(s.CACert)) {
				return nil, errors.New("invalid CA certificate")
			}
			tc.RootCAs = pool
		}
		sum := sha256.Sum256([]byte(s.Host + "\x00" + s.CACert))
		name := "dumpcat-" + hex.EncodeToString(sum[:8])
		if err := mysql.RegisterTLSConfig(name, tc); err != nil {
			return nil, err
		}
		cfg.TLSConfig = name
	case "disabled":
		cfg.TLSConfig = "false"
	default:
		cfg.TLSConfig = "preferred"
	}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type DBInfo struct {
	Name      string `json:"name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
	SizeBytes int64  `json:"size_bytes"`
	Tables    int    `json:"tables"`
	NonInnoDB int    `json:"non_innodb"`
	System    bool   `json:"system"`
}

type ServerInfo struct {
	Engine         string   `json:"engine"` // driver that inspected the server: mysql, postgres
	Version        string   `json:"version"`
	VersionComment string   `json:"version_comment"`
	Flavor         string   `json:"flavor"`
	Major          int      `json:"major"`
	Minor          int      `json:"minor"`
	Datadir        string   `json:"datadir"`
	WALLevel       string   `json:"wal_level,omitempty"` // PostgreSQL
	LogBin         bool     `json:"log_bin"`
	BinlogFormat   string   `json:"binlog_format"`
	GTIDMode       string   `json:"gtid_mode"`
	ServerID       string   `json:"server_id"`
	ReadOnly       bool     `json:"read_only"`
	IsReplica      bool     `json:"is_replica"`
	CurrentUser    string   `json:"current_user"`
	Grants         []string `json:"grants"`
	Warnings       []string `json:"warnings"`
	Databases      []DBInfo `json:"databases"`
	TotalBytes     int64    `json:"total_bytes"`
	Hostname       string   `json:"hostname"`
	Uptime         int64    `json:"uptime"`
}

func queryString(ctx context.Context, db *sql.DB, q string) string {
	var v sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return ""
	}
	return v.String
}

func DetectFlavor(version, comment string) string {
	l := strings.ToLower(version + " " + comment)
	switch {
	case strings.Contains(l, "mariadb"):
		return "mariadb"
	case strings.Contains(l, "percona"):
		return "percona"
	default:
		return "mysql"
	}
}

func parseVersion(v string) (int, int) {
	v = strings.TrimPrefix(v, "5.5.5-") // MariaDB replication prefix
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0
	}
	maj, _ := strconv.Atoi(parts[0])
	minStr := parts[1]
	for i, c := range minStr {
		if c < '0' || c > '9' {
			minStr = minStr[:i]
			break
		}
	}
	mn, _ := strconv.Atoi(minStr)
	return maj, mn
}

// Inspect gathers version, configuration and database sizes.
func Inspect(ctx context.Context, s *store.Server, withSizes bool) (*ServerInfo, error) {
	db, err := Open(ctx, s, "")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	info := &ServerInfo{}
	info.Version = queryString(ctx, db, "SELECT VERSION()")
	info.VersionComment = queryString(ctx, db, "SELECT @@version_comment")
	info.Flavor = DetectFlavor(info.Version, info.VersionComment)
	info.Major, info.Minor = parseVersion(info.Version)
	info.Datadir = queryString(ctx, db, "SELECT @@datadir")
	info.LogBin = queryString(ctx, db, "SELECT @@log_bin") == "1"
	info.BinlogFormat = queryString(ctx, db, "SELECT @@binlog_format")
	if info.Flavor == "mariadb" {
		if queryString(ctx, db, "SELECT @@gtid_strict_mode") != "" {
			info.GTIDMode = "MariaDB GTID"
		}
	} else {
		info.GTIDMode = queryString(ctx, db, "SELECT @@gtid_mode")
	}
	info.ServerID = queryString(ctx, db, "SELECT @@server_id")
	info.ReadOnly = queryString(ctx, db, "SELECT @@read_only") == "1"
	info.CurrentUser = queryString(ctx, db, "SELECT CURRENT_USER()")
	info.Hostname = queryString(ctx, db, "SELECT @@hostname")
	if up := queryString(ctx, db, "SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='UPTIME'"); up != "" {
		info.Uptime, _ = strconv.ParseInt(up, 10, 64)
	}
	if info.Uptime == 0 {
		var name, val string
		if err := db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'Uptime'").Scan(&name, &val); err == nil {
			info.Uptime, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	info.IsReplica = isReplica(ctx, db, info)
	if rows, err := db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER()"); err == nil {
		for rows.Next() {
			var g string
			if rows.Scan(&g) == nil {
				info.Grants = append(info.Grants, g)
			}
		}
		rows.Close()
	}
	info.Warnings = privilegeWarnings(info.Grants)
	dbs, err := ListDatabases(ctx, db, withSizes)
	if err != nil {
		return nil, err
	}
	info.Databases = dbs
	for _, d := range dbs {
		info.TotalBytes += d.SizeBytes
		if d.NonInnoDB > 0 && !d.System {
			info.Warnings = append(info.Warnings, fmt.Sprintf("Database `%s` has %d non-InnoDB tables (MyISAM/Aria...): lock-free logical backups cannot guarantee consistency for these tables.", d.Name, d.NonInnoDB))
		}
	}
	return info, nil
}

func isReplica(ctx context.Context, db *sql.DB, info *ServerInfo) bool {
	queries := []string{"SHOW REPLICA STATUS", "SHOW SLAVE STATUS"}
	if info.Flavor == "mariadb" {
		queries = []string{"SHOW ALL SLAVES STATUS", "SHOW SLAVE STATUS"}
	}
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		has := rows.Next()
		rows.Close()
		return has
	}
	return false
}

func privilegeWarnings(grants []string) []string {
	all := strings.ToUpper(strings.Join(grants, "\n"))
	if strings.Contains(all, "ALL PRIVILEGES ON *.*") {
		return nil
	}
	var w []string
	need := map[string]string{
		"SELECT":      "SELECT (read data)",
		"SHOW VIEW":   "SHOW VIEW (dump view)",
		"TRIGGER":     "TRIGGER (dump trigger)",
		"EVENT":       "EVENT (dump event)",
		"PROCESS":     "PROCESS (xtrabackup/mariabackup)",
		"RELOAD":      "RELOAD (FLUSH / backup lock)",
		"LOCK TABLES": "LOCK TABLES",
	}
	for k, desc := range need {
		if !strings.Contains(all, k) {
			w = append(w, "The backup user may be missing the "+desc+" privilege.")
		}
	}
	return w
}

// ListDatabases lists schemas with charset, size and engine statistics.
func ListDatabases(ctx context.Context, db *sql.DB, withSizes bool) ([]DBInfo, error) {
	rows, err := db.QueryContext(ctx, "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME")
	if err != nil {
		return nil, err
	}
	var out []DBInfo
	idx := map[string]int{}
	for rows.Next() {
		var d DBInfo
		if err := rows.Scan(&d.Name, &d.Charset, &d.Collation); err != nil {
			rows.Close()
			return nil, err
		}
		d.System = IsSystemDB(d.Name)
		idx[d.Name] = len(out)
		out = append(out, d)
	}
	rows.Close()
	if !withSizes {
		return out, nil
	}
	rows, err = db.QueryContext(ctx, `SELECT TABLE_SCHEMA, COUNT(*), COALESCE(SUM(DATA_LENGTH+INDEX_LENGTH),0),
		SUM(CASE WHEN TABLE_TYPE='BASE TABLE' AND ENGINE IS NOT NULL AND ENGINE NOT IN ('InnoDB','ROCKSDB','TokuDB') THEN 1 ELSE 0 END)
		FROM information_schema.TABLES GROUP BY TABLE_SCHEMA`)
	if err != nil {
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		var size sql.NullFloat64
		var non sql.NullInt64
		if rows.Scan(&name, &n, &size, &non) != nil {
			continue
		}
		if i, ok := idx[name]; ok {
			out[i].Tables = n
			out[i].SizeBytes = int64(size.Float64)
			out[i].NonInnoDB = int(non.Int64)
		}
	}
	return out, nil
}

// QuoteIdent quotes a MySQL identifier.
func QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func QuoteString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
	return "'" + r.Replace(s) + "'"
}

// DumpUsers produces CREATE USER / GRANT statements for non-system accounts.
func DumpUsers(ctx context.Context, s *store.Server) (string, int, error) {
	db, err := Open(ctx, s, "")
	if err != nil {
		return "", 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, "SET SESSION print_identified_with_as_hex = ON")
	rows, err := conn.QueryContext(ctx, "SELECT User, Host FROM mysql.user")
	if err != nil {
		return "", 0, err
	}
	type acct struct{ u, h string }
	var accts []acct
	for rows.Next() {
		var a acct
		if rows.Scan(&a.u, &a.h) == nil {
			accts = append(accts, a)
		}
	}
	rows.Close()
	skip := func(u string) bool {
		l := strings.ToLower(u)
		return l == "" || l == "root" || l == "mariadb.sys" || l == "debian-sys-maint" || l == "public" || strings.HasPrefix(l, "mysql.")
	}
	var sb strings.Builder
	sb.WriteString("-- Users & grants exported by DumpCat at " + time.Now().Format(time.RFC3339) + "\n")
	sb.WriteString("-- Excludes root and system accounts. Apply with: mysql --force < this_file\n\n")
	count := 0
	for _, a := range accts {
		if skip(a.u) {
			continue
		}
		acc := QuoteString(a.u) + "@" + QuoteString(a.h)
		var create string
		if err := conn.QueryRowContext(ctx, "SHOW CREATE USER "+acc).Scan(&create); err == nil {
			if strings.HasPrefix(strings.ToUpper(create), "CREATE USER ") && !strings.Contains(strings.ToUpper(create[:min(len(create), 40)]), "IF NOT EXISTS") {
				create = "CREATE USER IF NOT EXISTS " + create[len("CREATE USER "):]
			}
			sb.WriteString(create + ";\n")
		} else {
			sb.WriteString("CREATE USER IF NOT EXISTS " + acc + ";\n")
		}
		grows, err := conn.QueryContext(ctx, "SHOW GRANTS FOR "+acc)
		if err == nil {
			for grows.Next() {
				var g string
				if grows.Scan(&g) == nil {
					sb.WriteString(g + ";\n")
				}
			}
			grows.Close()
		}
		sb.WriteString("\n")
		count++
	}
	sb.WriteString("FLUSH PRIVILEGES;\n")
	return sb.String(), count, nil
}

// GetGlobal / SetGlobal are used by the "fast restore" mode.
func GetGlobal(ctx context.Context, db *sql.DB, name string) (string, error) {
	var v sql.NullString
	err := db.QueryRowContext(ctx, "SELECT @@GLOBAL."+name).Scan(&v)
	return v.String, err
}

func SetGlobal(ctx context.Context, db *sql.DB, name, value string) error {
	_, err := db.ExecContext(ctx, "SET GLOBAL "+name+" = "+value)
	return err
}
