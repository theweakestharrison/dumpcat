package mssql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// TempName cuts db so that db+suffix fits SQL Server's 128 characters.
func (Driver) TempName(db, suffix string) string {
	keep := max(128-utf8.RuneCountInString(suffix), 0)
	if utf8.RuneCountInString(db) > keep {
		db = string([]rune(db)[:keep])
	}
	return db + suffix
}

// msCheck reads restored copies over one pool of the sandbox server.
type msCheck struct {
	db    *sql.DB
	names map[string]map[string][2]string // database → "schema.table" → schema, table
}

func (Driver) OpenCheck(ctx context.Context, s *store.Server) (engine.CheckConn, error) {
	db, err := Open(ctx, s, "")
	if err != nil {
		return nil, err
	}
	return &msCheck{db: db, names: map[string]map[string][2]string{}}, nil
}

func (c *msCheck) Close() error { return c.db.Close() }

// Fold: the default collations compare names without case.
func (c *msCheck) Fold(s string) string { return strings.ToLower(s) }

func (c *msCheck) tables(ctx context.Context, db string) (map[string]int64, error) {
	q := "SELECT s.name, t.name, ISNULL(SUM(p.rows), 0) FROM " + QuoteName(db) + ".sys.tables t JOIN " + QuoteName(db) + ".sys.schemas s ON s.schema_id = t.schema_id LEFT JOIN " +
		QuoteName(db) + ".sys.partitions p ON p.object_id = t.object_id AND p.index_id IN (0, 1) WHERE t.is_ms_shipped = 0 GROUP BY s.name, t.name"
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string][2]string{}
	est := map[string]int64{}
	for rows.Next() {
		var s, t string
		var n int64
		if err := rows.Scan(&s, &t, &n); err != nil {
			return nil, err
		}
		key := s + "." + t
		names[strings.ToLower(key)] = [2]string{s, t}
		est[key] = n
	}
	c.names[strings.ToLower(db)] = names
	return est, rows.Err()
}

func (c *msCheck) Tables(ctx context.Context, db string) ([]string, error) {
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

func (c *msCheck) Objects(ctx context.Context, db string) (int, error) {
	n, ok := queryInt(ctx, c.db, "SELECT COUNT(*) FROM "+QuoteName(db)+".sys.objects WHERE type IN ('U', 'V') AND is_ms_shipped = 0")
	if !ok {
		return 0, errors.New("could not count the tables and views")
	}
	return int(n), nil
}

func (c *msCheck) Count(ctx context.Context, db, table string) (int64, error) {
	p, ok := c.names[strings.ToLower(db)][strings.ToLower(table)]
	if !ok {
		s, t, _ := strings.Cut(table, ".")
		p = [2]string{s, t}
	}
	var n int64
	err := c.db.QueryRowContext(ctx, "SELECT COUNT_BIG(*) FROM "+QuoteName(db)+"."+QuoteName(p[0])+"."+QuoteName(p[1])).Scan(&n)
	return n, err
}

// Estimate returns the server's own row counts (sys.partitions).
func (c *msCheck) Estimate(ctx context.Context, db string) (map[string]int64, error) {
	return c.tables(ctx, db)
}

// Assert runs one query inside database in a transaction that is rolled
// back, and returns the first column of the first row as text.
func (Driver) Assert(ctx context.Context, s *store.Server, database, q string) (sql.NullString, error) {
	var v sql.NullString
	db, err := Open(ctx, s, database)
	if err != nil {
		return v, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return v, err
	}
	defer tx.Rollback() // an assertion never changes the copy
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return v, err
	}
	if len(cols) == 0 {
		return v, errors.New("the query returned no column")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return v, err
		}
		return v, sql.ErrNoRows
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return v, err
	}
	switch x := vals[0].(type) {
	case nil:
	case []byte:
		v.Valid, v.String = true, string(x)
	case bool:
		v.Valid, v.String = true, fmt.Sprint(x)
	case time.Time:
		v.Valid, v.String = true, x.Format("2006-01-02 15:04:05")
	default:
		v.Valid, v.String = true, fmt.Sprint(x)
	}
	return v, nil
}

// DropDatabases drops drill copies, closing the sessions still connected
// to them: they are DumpCat's own databases.
func (Driver) DropDatabases(ctx context.Context, s *store.Server, names []string, noLog bool, lg *tasklog.Logger) (left, errs []string, err error) {
	db, err := Open(ctx, s, "")
	if err != nil {
		return names, nil, err
	}
	defer db.Close()
	for _, name := range names {
		if queryString(ctx, db, "SELECT name FROM sys.databases WHERE name = @p1", name) == "" {
			continue
		}
		q := "ALTER DATABASE " + QuoteName(name) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE"
		_ = exec(ctx, db, q, nil) // fails for a copy still restoring: DROP works on it anyway
		q = "DROP DATABASE " + QuoteName(name)
		lg.Printf("%s", q)
		if err := exec(ctx, db, q, nil); err != nil {
			left = append(left, name)
			errs = append(errs, name+": "+errText(err))
		}
	}
	return left, errs, nil
}

// CheckCopies runs DBCC CHECKDB on each restored copy: the consistency
// check DBAs run on restored backups, here on the sandbox instead of the
// production server.
func (Driver) CheckCopies(ctx context.Context, s *store.Server, copies []store.DrillDB, lg *tasklog.Logger) []store.DrillCheck {
	db, err := Open(ctx, s, "")
	if err != nil {
		return []store.DrillCheck{{Kind: "integrity", Name: "DBCC CHECKDB", Status: store.CheckFailed, Message: err.Error()}}
	}
	defer db.Close()
	var out []store.DrillCheck
	for _, c := range copies {
		t0 := time.Now()
		q := "DBCC CHECKDB (" + QuoteName(c.Temp) + ") WITH NO_INFOMSGS, ALL_ERRORMSGS"
		lg.Printf("%s", q)
		var msgs []string
		err := exec(ctx, db, q, func(m string) {
			if len(msgs) < 20 {
				msgs = append(msgs, m)
			}
		})
		chk := store.DrillCheck{Kind: "integrity", Database: c.Name, Name: "DBCC CHECKDB", Status: store.CheckOK, Actual: engine.HumanDuration(time.Since(t0)),
			Message: "no allocation or consistency errors"}
		switch {
		case ctx.Err() != nil:
			return out
		case err != nil:
			chk.Status, chk.Message = store.CheckFailed, errText(err)
		case len(msgs) > 0:
			chk.Status, chk.Message = store.CheckFailed, strings.Join(msgs, " ")
		}
		lg.Printf("%s: DBCC CHECKDB %s", c.Name, map[bool]string{true: "✓", false: "✗ " + chk.Message}[chk.Status == store.CheckOK])
		out = append(out, chk)
	}
	return out
}
