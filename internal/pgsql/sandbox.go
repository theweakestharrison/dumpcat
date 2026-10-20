package pgsql

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// TempName cuts db so that db+suffix fits PostgreSQL's 63 bytes.
func (Driver) TempName(db, suffix string) string {
	keep := max(63-len(suffix), 0)
	if len(db) > keep {
		cut := keep
		for cut > 0 && !utf8.RuneStart(db[cut]) { // on a character boundary
			cut--
		}
		db = db[:cut]
	}
	return db + suffix
}

// pgCheck reads restored copies: one connection per database.
type pgCheck struct {
	s     *store.Server
	conns map[string]*pgx.Conn
	names map[string]map[string][2]string // db → "schema.table" → schema, table
}

func (Driver) OpenCheck(ctx context.Context, s *store.Server) (engine.CheckConn, error) {
	c, err := adminConn(ctx, s)
	if err != nil {
		return nil, err
	}
	c.Close(context.Background())
	return &pgCheck{s: s, conns: map[string]*pgx.Conn{}, names: map[string]map[string][2]string{}}, nil
}

func (c *pgCheck) conn(ctx context.Context, db string) (*pgx.Conn, error) {
	if cn, ok := c.conns[db]; ok {
		return cn, nil
	}
	cn, err := Connect(ctx, c.s, db)
	if err != nil {
		return nil, err
	}
	c.conns[db] = cn
	return cn, nil
}

func (c *pgCheck) Close() error {
	for _, cn := range c.conns {
		cn.Close(context.Background())
	}
	c.conns = map[string]*pgx.Conn{}
	return nil
}

func (c *pgCheck) Fold(s string) string { return s }

// dataTables: the tables pg_dump writes rows of (partitions included, their
// parents and extension members not).
const dataTables = `SELECT n.nspname, c.relname, c.reltuples::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind = 'r' AND ` + userSchemas + `
	AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')`

func (c *pgCheck) tables(ctx context.Context, db string) (map[string]int64, error) {
	cn, err := c.conn(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := cn.Query(ctx, dataTables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string][2]string{}
	est := map[string]int64{}
	for rows.Next() {
		var nsp, rel string
		var n int64
		if err := rows.Scan(&nsp, &rel, &n); err != nil {
			return nil, err
		}
		key := nsp + "." + rel
		names[key] = [2]string{nsp, rel}
		est[key] = max(n, 0)
	}
	c.names[db] = names
	return est, rows.Err()
}

func (c *pgCheck) Tables(ctx context.Context, db string) ([]string, error) {
	est, err := c.tables(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(est))
	for k := range est {
		out = append(out, k)
	}
	return out, nil
}

func (c *pgCheck) Objects(ctx context.Context, db string) (int, error) {
	cn, err := c.conn(ctx, db)
	if err != nil {
		return 0, err
	}
	var n int
	err = cn.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r','p','v','m','f') AND NOT c.relispartition AND `+userSchemas).Scan(&n)
	return n, err
}

// Count counts the table's own rows (FROM ONLY: not those of its children,
// as pg_dump copies each table separately).
func (c *pgCheck) Count(ctx context.Context, db, table string) (int64, error) {
	cn, err := c.conn(ctx, db)
	if err != nil {
		return 0, err
	}
	p, ok := c.names[db][table]
	if !ok {
		nsp, rel, _ := strings.Cut(table, ".")
		p = [2]string{nsp, rel}
	}
	var n int64
	err = cn.QueryRow(ctx, "SELECT count(*) FROM ONLY "+QuoteIdent(p[0])+"."+QuoteIdent(p[1])).Scan(&n)
	return n, err
}

// Estimate runs ANALYZE and returns pg_class.reltuples: exact for tables
// that fit the statistics sample (30,000 rows by default), close otherwise.
func (c *pgCheck) Estimate(ctx context.Context, db string) (map[string]int64, error) {
	cn, err := c.conn(ctx, db)
	if err != nil {
		return nil, err
	}
	if _, err := cn.Exec(ctx, "ANALYZE"); err != nil {
		return nil, err
	}
	return c.tables(ctx, db)
}

// Assert runs one read-only query inside database and returns the first
// column of the first row as text (booleans as true/false).
func (Driver) Assert(ctx context.Context, s *store.Server, database, q string) (sql.NullString, error) {
	var v sql.NullString
	c, err := Connect(ctx, s, database)
	if err != nil {
		return v, err
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY"); err != nil {
		return v, err
	}
	_, _ = c.Exec(ctx, "SET statement_timeout = '600s'")
	// the simple protocol returns every value as text
	rows, err := c.Query(ctx, q, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	if len(fields) == 0 {
		return v, errors.New("the query returned no column")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return v, err
		}
		return v, sql.ErrNoRows
	}
	raw := rows.RawValues()
	if len(raw) == 0 || raw[0] == nil {
		return v, rows.Err()
	}
	v.Valid, v.String = true, string(raw[0])
	if fields[0].DataTypeOID == pgtype.BoolOID {
		v.String = strconv.FormatBool(v.String == "t")
	}
	return v, nil
}

// DropDatabases drops drill copies, closing the sessions still connected
// to them (WITH (FORCE), PostgreSQL 13+): they are DumpCat's own databases.
func (Driver) DropDatabases(ctx context.Context, s *store.Server, names []string, noLog bool, lg *tasklog.Logger) (left, errs []string, err error) {
	c, err := adminConn(ctx, s)
	if err != nil {
		return names, nil, err
	}
	defer c.Close(context.Background())
	num, _ := strconv.Atoi(queryString(ctx, c, "SELECT current_setting('server_version_num')"))
	for _, name := range names {
		q := "DROP DATABASE IF EXISTS " + QuoteIdent(name)
		if num >= 130000 {
			q += " WITH (FORCE)"
		}
		lg.Printf("%s", q)
		if _, err := c.Exec(ctx, q); err != nil {
			left = append(left, name)
			errs = append(errs, name+": "+err.Error())
		}
	}
	return left, errs, nil
}
