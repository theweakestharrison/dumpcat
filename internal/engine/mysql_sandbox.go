package engine

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// ErrNoEstimates: the server keeps no row estimates a drill could use.
var ErrNoEstimates = errors.New("row estimates are not available for this server")

// TempName cuts db so that db+suffix fits MySQL's 64 characters.
func (MySQL) TempName(db, suffix string) string {
	b := []rune(db)
	if keep := 64 - len([]rune(suffix)); len(b) > keep {
		b = b[:keep]
	}
	return string(b) + suffix
}

type mysqlCheck struct {
	db   *sql.DB
	lctn int
}

func (MySQL) OpenCheck(ctx context.Context, s *store.Server) (CheckConn, error) {
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return nil, err
	}
	c := &mysqlCheck{db: db}
	_ = db.QueryRowContext(ctx, "SELECT @@lower_case_table_names").Scan(&c.lctn)
	return c, nil
}

func (c *mysqlCheck) Close() error { return c.db.Close() }

func (c *mysqlCheck) Fold(s string) string {
	if c.lctn != 0 {
		return strings.ToLower(s)
	}
	return s
}

// Tables lists the base tables of a database.
func (c *mysqlCheck) Tables(ctx context.Context, schema string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_TYPE IN ('BASE TABLE','SYSTEM VERSIONED')`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (c *mysqlCheck) Objects(ctx context.Context, schema string) (int, error) {
	var n int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?`, schema).Scan(&n)
	return n, err
}

func (c *mysqlCheck) Count(ctx context.Context, schema, table string) (int64, error) {
	var n int64
	err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dbconn.QuoteIdent(schema)+"."+dbconn.QuoteIdent(table)).Scan(&n)
	return n, err
}

// Estimate: InnoDB's TABLE_ROWS is too rough to compare with a backup.
func (c *mysqlCheck) Estimate(ctx context.Context, schema string) (map[string]int64, error) {
	return nil, ErrNoEstimates
}

// Assert runs one assertion in a read-only session of its own, inside the
// temporary copy of its database.
func (MySQL) Assert(ctx context.Context, s *store.Server, database, q string) (sql.NullString, error) {
	var v sql.NullString
	db, err := dbconn.Open(ctx, s, database)
	if err != nil {
		return v, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return v, err
	}
	defer conn.Close()
	_, _ = conn.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY")
	_, _ = conn.ExecContext(ctx, "SET SESSION max_statement_time=600")    // MariaDB
	_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time=600000") // MySQL
	return firstValue(ctx, conn.QueryContext, q)
}

// firstValue returns the first column of the first row of a query.
func firstValue(ctx context.Context, query func(context.Context, string, ...any) (*sql.Rows, error), q string) (sql.NullString, error) {
	var v sql.NullString
	rows, err := query(ctx, q)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || len(cols) == 0 {
		return v, errors.New("the query returned no column")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return v, err
		}
		return v, sql.ErrNoRows
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return v, err
	}
	return vals[0], nil
}

// DropDatabases drops the copies of a drill in one session (sql_log_bin=0
// with noLog).
func (MySQL) DropDatabases(ctx context.Context, s *store.Server, names []string, noLog bool, lg *tasklog.Logger) (left, errs []string, err error) {
	db, err := dbconn.Open(ctx, s, "")
	if err != nil {
		return names, nil, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return names, nil, err
	}
	defer conn.Close()
	if noLog {
		if _, err := conn.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
			lg.Printf("WARNING: could not disable the binary log for the cleanup: %v", err)
		}
	}
	for _, name := range names {
		lg.Printf("DROP DATABASE IF EXISTS %s", dbconn.QuoteIdent(name))
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbconn.QuoteIdent(name)); err != nil {
			left = append(left, name)
			errs = append(errs, name+": "+err.Error())
		}
	}
	return left, errs, nil
}

// assertTimeout bounds one drill assertion.
const AssertTimeout = 10 * time.Minute
