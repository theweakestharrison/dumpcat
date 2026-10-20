package pgsql

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/store"
)

// SystemDatabases cannot be dumped or are templates.
var SystemDatabases = map[string]bool{"template0": true, "template1": true}

func queryString(ctx context.Context, c *pgx.Conn, q string, args ...any) string {
	var v *string
	if err := c.QueryRow(ctx, q, args...).Scan(&v); err != nil || v == nil {
		return ""
	}
	return *v
}

// Inspect reads the version, settings, role and databases of a server.
func (Driver) Inspect(ctx context.Context, s *store.Server, withSizes bool) (*dbconn.ServerInfo, error) {
	c, err := adminConn(ctx, s)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	info := &dbconn.ServerInfo{Engine: store.EnginePostgres, Flavor: "postgres"}
	info.Version = queryString(ctx, c, "SELECT current_setting('server_version')")
	info.VersionComment = queryString(ctx, c, "SELECT version()")
	if num, _ := strconv.Atoi(queryString(ctx, c, "SELECT current_setting('server_version_num')")); num > 0 {
		info.Major = num / 10000
		info.Minor = num % 10000
		if info.Major < 10 { // 9.6.24 = 90624
			info.Major, info.Minor = num/10000, (num/100)%100
		}
	}
	// readable by superusers and pg_read_all_settings only
	info.Datadir = queryString(ctx, c, "SELECT current_setting('data_directory', true)")
	info.WALLevel = queryString(ctx, c, "SELECT current_setting('wal_level')")
	info.IsReplica = queryString(ctx, c, "SELECT pg_is_in_recovery()::text") == "true"
	info.ReadOnly = queryString(ctx, c, "SELECT current_setting('default_transaction_read_only')") == "on"
	info.CurrentUser = queryString(ctx, c, "SELECT current_user::text")
	info.Hostname = queryString(ctx, c, "SELECT coalesce(nullif(current_setting('cluster_name'), ''), host(inet_server_addr()), 'local socket')")
	if up := queryString(ctx, c, "SELECT extract(epoch FROM now() - pg_postmaster_start_time())::bigint::text"); up != "" {
		info.Uptime, _ = strconv.ParseInt(up, 10, 64)
	}
	super, readAll := roleOf(ctx, c, info)
	if !super && !readAll {
		info.Warnings = append(info.Warnings, "The backup account is neither a superuser nor a member of pg_read_all_data (PostgreSQL 14+): pg_dump stops at the first table, sequence or schema it may not read. GRANT pg_read_all_data TO the account, or back up with the owner of the databases.")
	}
	if !super {
		info.Warnings = append(info.Warnings, "Roles are backed up without their passwords: pg_dumpall reads them from pg_authid, which only superusers may read.")
	}
	if info.IsReplica {
		info.Warnings = append(info.Warnings, "This is a standby: a long pg_dump can be canceled by recovery conflicts (raise max_standby_streaming_delay or turn on hot_standby_feedback).")
	}
	dbs, err := listDatabases(ctx, c, s, withSizes)
	if err != nil {
		return nil, err
	}
	info.Databases = dbs
	for _, d := range dbs {
		info.TotalBytes += d.SizeBytes
	}
	return info, nil
}

// roleOf lists the backup account's attributes and memberships that matter.
func roleOf(ctx context.Context, c *pgx.Conn, info *dbconn.ServerInfo) (super, readAll bool) {
	var createdb, replication, bypassrls bool
	if err := c.QueryRow(ctx, "SELECT rolsuper, rolcreatedb, rolreplication, rolbypassrls FROM pg_roles WHERE rolname = current_user").
		Scan(&super, &createdb, &replication, &bypassrls); err != nil {
		return false, false
	}
	for _, a := range []struct {
		on   bool
		name string
	}{{super, "SUPERUSER"}, {createdb, "CREATEDB"}, {replication, "REPLICATION"}, {bypassrls, "BYPASSRLS"}} {
		if a.on {
			info.Grants = append(info.Grants, a.name)
		}
	}
	if super {
		return true, true // a superuser is a member of every role
	}
	rows, err := c.Query(ctx, `SELECT r.rolname FROM pg_roles r WHERE r.rolname IN ('pg_read_all_data','pg_write_all_data','pg_monitor','pg_read_all_settings')
		AND pg_has_role(current_user, r.oid, 'MEMBER') ORDER BY 1`)
	if err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				info.Grants = append(info.Grants, "member of "+n)
				readAll = readAll || n == "pg_read_all_data"
			}
		}
		rows.Close()
	}
	return super, readAll
}

func listDatabases(ctx context.Context, c *pgx.Conn, s *store.Server, withSizes bool) ([]dbconn.DBInfo, error) {
	q := `SELECT datname, pg_encoding_to_char(encoding), datcollate, datallowconn, datistemplate,
		has_database_privilege(oid, 'CONNECT'),
		CASE WHEN $1 AND has_database_privilege(oid, 'CONNECT') THEN pg_database_size(oid) ELSE 0 END
		FROM pg_database ORDER BY datname`
	rows, err := c.Query(ctx, q, withSizes)
	if err != nil {
		return nil, err
	}
	var out []dbconn.DBInfo
	var reachable []int
	for rows.Next() {
		var d dbconn.DBInfo
		var allow, tmpl, connect bool
		if err := rows.Scan(&d.Name, &d.Charset, &d.Collation, &allow, &tmpl, &connect, &d.SizeBytes); err != nil {
			rows.Close()
			return nil, err
		}
		d.System = SystemDatabases[d.Name] || !allow || tmpl
		if !d.System && connect {
			reachable = append(reachable, len(out))
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !withSizes {
		return out, nil
	}
	// tables live in each database's own catalog
	for _, i := range reachable {
		if ctx.Err() != nil {
			break
		}
		func() {
			dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			dc, err := Connect(dctx, s, out[i].Name)
			if err != nil {
				return
			}
			defer dc.Close(context.Background())
			_ = dc.QueryRow(dctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE c.relkind IN ('r','p') AND NOT c.relispartition AND `+userSchemas).Scan(&out[i].Tables)
		}()
	}
	return out, nil
}

// userSchemas filters pg_namespace n down to the schemas pg_dump writes.
const userSchemas = `n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp\_%'`

// serverMajor reads the major version from the inspected version ("16.15").
func serverMajor(info *dbconn.ServerInfo, version string) int {
	if info != nil && info.Major > 0 {
		return info.Major
	}
	return major(version)
}
