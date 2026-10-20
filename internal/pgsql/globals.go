package pgsql

import (
	"context"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// restoreGlobals applies the roles of _globals.sql one statement at a time
// over a plain connection: no psql, so a backup file can never run psql
// meta-commands (\! …). Like CREATE USER IF NOT EXISTS for MySQL, a role the
// target already has is left alone (no new password or attributes), and so
// are the restore account itself and the pg_* roles; tablespaces are not
// created (their directories belong to the source server). Failed
// statements are logged and skipped.
func restoreGlobals(env *engine.Env, src engine.RestoreSource, spec *engine.RestoreSpec, files map[string]store.BackupFile) error {
	ctx := env.Ctx
	f, ok := files[GlobalsFile]
	if !ok {
		env.Log.Printf("The backup contains no roles — skipping")
		return nil
	}
	env.Log.Printf("Restoring roles (statements that fail are logged and skipped)")
	rd, done, err := src.Open(ctx, f)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(rd, 256<<20))
	if derr := done(err == nil); err == nil && derr != nil {
		return derr
	}
	if err != nil {
		return err
	}
	c, err := adminConn(ctx, spec.Target)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	existing := map[string]bool{}
	rows, err := c.Query(ctx, "SELECT rolname FROM pg_roles")
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			existing[n] = true
		}
	}
	rows.Close()
	me := queryString(ctx, c, "SELECT current_user::text")
	created := map[string]bool{}
	var kept []string
	failed, tablespaces := 0, 0
	for _, st := range SplitSQL(string(data)) {
		kind, role := classify(st)
		switch kind {
		case "create role":
			if existing[role] || role == me || strings.HasPrefix(role, "pg_") {
				kept = append(kept, role)
				continue
			}
		case "alter role":
			if !created[role] {
				continue
			}
		case "tablespace":
			tablespaces++
			continue
		}
		if _, err := c.Exec(ctx, st, pgx.QueryExecModeSimpleProtocol); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed++
			env.Log.Printf("  ✗ %s: %v", shorten(st, 120), err)
			continue
		}
		if kind == "create role" {
			created[role] = true
		}
	}
	env.Log.Printf("Roles: %d created, %d already on the target (left as they are)", len(created), len(kept))
	if tablespaces > 0 {
		env.Log.Printf("NOTE: %d tablespace(s) in the backup were not created (their directories belong to the source server)", tablespaces)
	}
	if failed > 0 {
		env.Log.Printf("%d role statement(s) failed (see above)", failed)
	}
	return nil
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// classify tells what a statement of pg_dumpall --globals-only does and
// which role it is about.
func classify(st string) (kind, role string) {
	u := strings.ToUpper(st)
	switch {
	case strings.HasPrefix(u, "CREATE ROLE "):
		return "create role", parseIdent(st[len("CREATE ROLE "):])
	case strings.HasPrefix(u, "ALTER ROLE "):
		return "alter role", parseIdent(st[len("ALTER ROLE "):])
	case strings.HasPrefix(u, "CREATE TABLESPACE "), strings.HasPrefix(u, "ALTER TABLESPACE "),
		strings.HasPrefix(u, "COMMENT ON TABLESPACE "), strings.HasPrefix(u, "GRANT ") && strings.Contains(u, " ON TABLESPACE "):
		return "tablespace", ""
	}
	return "", ""
}

// parseIdent reads the identifier at the start of s: "Quoted ""name""" or
// a plain word.
func parseIdent(s string) string {
	s = strings.TrimLeft(s, " \t\r\n")
	if strings.HasPrefix(s, `"`) {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '"' {
				if i+1 < len(s) && s[i+1] == '"' {
					b.WriteByte('"')
					i++
					continue
				}
				return b.String()
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	end := strings.IndexAny(s, " \t\r\n;")
	if end < 0 {
		end = len(s)
	}
	return s[:end]
}

// SplitSQL splits a script into statements (without the ';'). It knows
// quoted strings, quoted identifiers, dollar quoting and comments, and drops
// psql meta-command lines (\connect, \restrict …) between statements.
func SplitSQL(script string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	n := len(script)
	for i := 0; i < n; {
		c := script[i]
		switch {
		case c == '\\' && strings.TrimSpace(cur.String()) == "" && atLineStart(script, i):
			// a meta-command takes the rest of the line
			for i < n && script[i] != '\n' {
				i++
			}
		case c == '-' && i+1 < n && script[i+1] == '-':
			for i < n && script[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && script[i+1] == '*':
			depth := 0
			for i < n {
				if i+1 < n && script[i] == '/' && script[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < n && script[i] == '*' && script[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
			cur.WriteByte(' ')
		case c == '\'' || c == '"':
			escapes := c == '\'' && i > 0 && (script[i-1] == 'E' || script[i-1] == 'e') && (i < 2 || !isIdentByte(script[i-2]))
			j := i + 1
			for j < n {
				if escapes && script[j] == '\\' {
					j += 2
					continue
				}
				if script[j] == c {
					if j+1 < n && script[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= n {
				j = n - 1
			}
			cur.WriteString(script[i : j+1])
			i = j + 1
		case c == '$':
			if tag, ok := dollarTag(script[i:]); ok {
				end := strings.Index(script[i+len(tag):], tag)
				if end < 0 {
					cur.WriteString(script[i:])
					i = n
				} else {
					stop := i + len(tag) + end + len(tag)
					cur.WriteString(script[i:stop])
					i = stop
				}
				continue
			}
			cur.WriteByte(c)
			i++
		case c == ';':
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return out
}

func atLineStart(s string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch s[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b >= 0x80
}

// dollarTag reads "$tag$" or "$$" at the start of s.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		if s[j] == '$' {
			return s[:j+1], true
		}
		if !isIdentByte(s[j]) || (j == 1 && s[j] >= '0' && s[j] <= '9') {
			return "", false
		}
	}
	return "", false
}
