package engine

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"dumpcat/internal/pipeline"
	"dumpcat/internal/store"
)

// RowCounter reads a logical dump as it is written and counts what it holds:
// the tables it creates and the rows it inserts into each of them, in the SQL
// mysqldump, mariadb-dump and mydumper write. Restore drills compare these
// counts with the restored copy. It follows USE, ignores comments and skips
// DELIMITER blocks (the bodies of routines, triggers and events). Not safe
// for concurrent use.
type RowCounter struct {
	db     string
	tables store.TableStats

	mode, ret int
	quote     byte // the quote open inside a statement (0: none)
	esc       bool // the previous byte inside the quote was a backslash
	prev      byte // the previous byte outside quotes and comments
	depth     int  // parentheses
	head      []byte
	cdb, ctab string // the table an INSERT writes to
	rows      int64  // rows of that INSERT, not added yet
	tail      [6]byte
	tailN     int
	line      []byte // the beginning of a line inside a DELIMITER block
}

const (
	rcHead   = iota // the beginning of a statement, kept to classify it
	rcSkip          // a statement that inserts nothing
	rcCols          // INSERT with a column list (or PARTITION): waiting for VALUES
	rcValues        // the value lists of an INSERT
	rcLineComment
	rcBlockComment
	rcDelimBlock
)

const maxHead = 1024

// NewRowCounter counts a dump whose statements apply to database db until a
// USE statement switches to another one.
func NewRowCounter(db string) *RowCounter {
	return &RowCounter{db: db, tables: store.TableStats{}, head: make([]byte, 0, 128)}
}

// Tables returns the rows counted per database and table so far.
func (c *RowCounter) Tables() store.TableStats {
	c.flush()
	out := store.TableStats{}
	for db, ts := range c.tables {
		if db == "" {
			continue
		}
		m := make(map[string]int64, len(ts))
		for t, n := range ts {
			m[t] = n
		}
		out[db] = m
	}
	return out
}

func (c *RowCounter) Write(p []byte) (int, error) {
	for i := 0; i < len(p); i++ {
		if c.quote != 0 && !c.esc && c.mode != rcHead && c.mode < rcLineComment {
			// most of a dump is quoted text: jump to the next quote or backslash
			rest := p[i:]
			j := bytes.IndexByte(rest, c.quote)
			lim := rest
			if j >= 0 {
				lim = rest[:j]
			}
			if k := bytes.IndexByte(lim, '\\'); k >= 0 {
				j = k
			}
			if j < 0 {
				break
			}
			i += j
		}
		c.step(p[i])
	}
	return len(p), nil
}

func (c *RowCounter) step(b byte) {
	switch c.mode {
	case rcLineComment:
		if b == '\n' {
			c.mode = rcHead
		}
		return
	case rcBlockComment:
		if b == '/' && c.prev == '*' {
			c.mode, c.prev = c.ret, 0
		} else {
			c.prev = b
		}
		return
	case rcDelimBlock:
		c.delimLine(b)
		return
	}
	if c.quote != 0 {
		if c.mode == rcHead {
			c.push(b)
		}
		switch {
		case c.esc:
			c.esc = false
		case b == '\\' && c.quote != '`':
			c.esc = true
		case b == c.quote:
			c.quote = 0
		}
		return
	}
	if b == '*' && c.prev == '/' {
		if c.mode == rcHead && len(c.head) > 0 {
			c.head = c.head[:len(c.head)-1]
		}
		c.ret, c.mode, c.prev = c.mode, rcBlockComment, 0
		return
	}
	c.prev = b
	if b == '\'' || b == '"' || b == '`' {
		c.quote = b
		if c.mode == rcHead {
			c.push(b)
		}
		return
	}
	switch c.mode {
	case rcHead:
		c.headByte(b)
	case rcSkip:
		switch b {
		case '(':
			c.depth++
		case ')':
			if c.depth > 0 {
				c.depth--
			}
		case ';':
			if c.depth == 0 {
				c.end()
			}
		}
	case rcCols:
		switch b {
		case '(':
			if c.depth == 0 && c.afterValues() {
				c.mode, c.depth = rcValues, 1
				c.rows++
				return
			}
			c.depth++
			c.tailN = 0
		case ')':
			if c.depth > 0 {
				c.depth--
			}
		case ';':
			if c.depth == 0 {
				c.end()
			}
		case ' ', '\t', '\r', '\n':
		default:
			if c.depth == 0 {
				c.pushTail(b)
			}
		}
	case rcValues:
		switch b {
		case '(':
			if c.depth == 0 {
				c.rows++
			}
			c.depth++
		case ')':
			if c.depth > 0 {
				c.depth--
			}
		case ';':
			if c.depth == 0 {
				c.end()
			}
		}
	}
}

func (c *RowCounter) push(b byte) {
	if len(c.head) < maxHead {
		c.head = append(c.head, b)
	}
}

func (c *RowCounter) headByte(b byte) {
	switch b {
	case ' ', '\t', '\r', '\n':
		if len(c.head) == 0 {
			return
		}
		if len(c.head) == 2 && c.head[0] == '-' && c.head[1] == '-' {
			c.head = c.head[:0]
			if b != '\n' {
				c.mode = rcLineComment
			}
			return
		}
		if b == '\n' && c.isDelimiter() {
			d := strings.TrimSpace(string(c.head[len("DELIMITER"):]))
			c.head = c.head[:0]
			if d != "" && d != ";" {
				c.mode, c.line = rcDelimBlock, c.line[:0]
			}
			return
		}
		c.push(b)
	case '#':
		if len(c.head) == 0 {
			c.mode = rcLineComment
			return
		}
		c.push(b)
	case ';':
		if c.isDelimiter() {
			c.push(b)
			return
		}
		c.end()
	case '(':
		c.open()
	default:
		c.push(b)
		if len(c.head) >= maxHead {
			c.mode = rcSkip // too long to be a statement we count
		}
	}
}

func (c *RowCounter) isDelimiter() bool {
	const kw = "DELIMITER"
	return len(c.head) > len(kw) && bytes.EqualFold(c.head[:len(kw)], []byte(kw)) && (c.head[len(kw)] == ' ' || c.head[len(kw)] == '\t')
}

// delimLine skips a DELIMITER block up to the line "DELIMITER ;".
func (c *RowCounter) delimLine(b byte) {
	if b != '\n' {
		if len(c.line) < 32 {
			c.line = append(c.line, b)
		}
		return
	}
	ln := strings.TrimSpace(string(c.line))
	c.line = c.line[:0]
	if len(ln) > len("DELIMITER") && strings.EqualFold(ln[:len("DELIMITER")], "DELIMITER") && strings.TrimSpace(ln[len("DELIMITER"):]) == ";" {
		c.mode, c.prev = rcHead, 0
	}
}

func (c *RowCounter) pushTail(b byte) {
	if b >= 'a' && b <= 'z' {
		b -= 'a' - 'A'
	}
	if b < 'A' || b > 'Z' {
		c.tailN = 0
		return
	}
	if c.tailN < len(c.tail) {
		c.tail[c.tailN] = b
		c.tailN++
		return
	}
	copy(c.tail[:], c.tail[1:])
	c.tail[len(c.tail)-1] = b
}

func (c *RowCounter) afterValues() bool {
	t := string(c.tail[:c.tailN])
	return strings.HasSuffix(t, "VALUES") || strings.HasSuffix(t, "VALUE")
}

// open handles the first parenthesis of a statement.
func (c *RowCounter) open() {
	h := parseHead(c.head)
	c.head = c.head[:0]
	c.depth = 1
	switch h.kind {
	case headInsert:
		c.cdb, c.ctab = h.db, h.table
		if c.cdb == "" {
			c.cdb = c.db
		}
		if h.values {
			c.mode = rcValues
			c.rows++
		} else {
			c.mode, c.tailN = rcCols, 0
		}
	case headCreate:
		c.table(h.db, h.table)
		c.mode = rcSkip
	default:
		c.mode = rcSkip
	}
}

// end handles the end of a statement.
func (c *RowCounter) end() {
	if c.mode == rcHead {
		switch h := parseHead(c.head); h.kind {
		case headUse:
			c.db = h.db
		case headCreate:
			c.table(h.db, h.table)
		}
	}
	c.flush()
	c.mode, c.depth, c.head = rcHead, 0, c.head[:0]
}

func (c *RowCounter) flush() {
	if c.ctab != "" {
		c.table(c.cdb, c.ctab)
		c.tables[c.cdb][c.ctab] += c.rows
	}
	c.cdb, c.ctab, c.rows = "", "", 0
}

func (c *RowCounter) table(db, t string) {
	if db == "" {
		db = c.db
	}
	if t == "" {
		return
	}
	m := c.tables[db]
	if m == nil {
		m = map[string]int64{}
		c.tables[db] = m
	}
	if _, ok := m[t]; !ok {
		m[t] = 0
	}
}

const (
	headOther = iota
	headInsert
	headCreate
	headUse
)

type headInfo struct {
	kind      int
	db, table string
	values    bool // ends with VALUES: the value lists start here
}

type sqlTok struct {
	s      string
	quoted bool
}

// sqlTokens splits the beginning of a statement into words, `quoted`
// identifiers and single punctuation characters.
func sqlTokens(h []byte) []sqlTok {
	var out []sqlTok
	for i := 0; i < len(h); {
		b := h[i]
		switch {
		case b == ' ' || b == '\t' || b == '\r' || b == '\n':
			i++
		case b == '`':
			var sb strings.Builder
			j := i + 1
			for j < len(h) {
				if h[j] == '`' {
					if j+1 < len(h) && h[j+1] == '`' {
						sb.WriteByte('`')
						j += 2
						continue
					}
					break
				}
				sb.WriteByte(h[j])
				j++
			}
			out = append(out, sqlTok{sb.String(), true})
			i = j + 1
		case isWordByte(b):
			j := i
			for j < len(h) && isWordByte(h[j]) {
				j++
			}
			out = append(out, sqlTok{string(h[i:j]), false})
			i = j
		default:
			out = append(out, sqlTok{string(b), false})
			i++
		}
	}
	return out
}

func isWordByte(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func parseHead(h []byte) headInfo {
	toks := sqlTokens(h)
	kw := func(i int, words ...string) bool {
		if i >= len(toks) || toks[i].quoted {
			return false
		}
		for _, w := range words {
			if strings.EqualFold(toks[i].s, w) {
				return true
			}
		}
		return false
	}
	// name reads [db.]table at i
	name := func(i int) (db, t string, next int) {
		if i >= len(toks) {
			return "", "", i
		}
		if i+2 < len(toks) && toks[i+1].s == "." && !toks[i+1].quoted {
			return toks[i].s, toks[i+2].s, i + 3
		}
		return "", toks[i].s, i + 1
	}
	switch {
	case kw(0, "INSERT", "REPLACE"):
		i := 1
		for kw(i, "LOW_PRIORITY", "DELAYED", "HIGH_PRIORITY", "IGNORE") {
			i++
		}
		if kw(i, "INTO") {
			i++
		}
		db, t, next := name(i)
		return headInfo{kind: headInsert, db: db, table: t, values: next == len(toks)-1 && kw(next, "VALUES", "VALUE")}
	case kw(0, "CREATE"):
		i := 1
		if kw(i, "OR") && kw(i+1, "REPLACE") {
			i += 2
		}
		if !kw(i, "TABLE") {
			return headInfo{}
		}
		i++
		if kw(i, "IF") && kw(i+1, "NOT") && kw(i+2, "EXISTS") {
			i += 3
		}
		db, t, _ := name(i)
		return headInfo{kind: headCreate, db: db, table: t}
	case kw(0, "USE"):
		if len(toks) > 1 {
			return headInfo{kind: headUse, db: toks[1].s}
		}
	}
	return headInfo{}
}

var (
	reMdChunk = regexp.MustCompile(`(\.\d+)+$`)
	reMdSkip  = regexp.MustCompile(`-schema-(create|triggers|post|sequence)$|(^|-)metadata$`)
)

// MydumperTableStats counts the tables and rows of a mydumper dump directory.
// It returns nil when the dump does not hold plain INSERT statements (e.g.
// --load-data or --csv) or a file cannot be attributed to a database.
func MydumperTableStats(dir string, dbs []string) (store.TableStats, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	// the longest database name first: "shop.archive" before "shop"
	byLen := append([]string(nil), dbs...)
	sort.Slice(byLen, func(i, j int) bool { return len(byLen[i]) > len(byLen[j]) })
	dbOf := func(prefix string) string {
		for _, d := range byLen {
			if strings.HasPrefix(prefix, d+".") {
				return d
			}
		}
		return ""
	}
	views := map[string]bool{}
	var schemas, data []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := pipeline.StripExt(e.Name())
		switch {
		case strings.HasSuffix(base, ".dat") || strings.HasSuffix(base, ".csv") || strings.HasSuffix(base, ".tsv"):
			return nil, nil
		case strings.HasSuffix(base, "-schema-view.sql"):
			views[strings.TrimSuffix(base, "-schema-view.sql")] = true
		case strings.HasSuffix(base, "-schema.sql"):
			schemas = append(schemas, e.Name())
		case strings.HasSuffix(base, ".sql") && !reMdSkip.MatchString(strings.TrimSuffix(base, ".sql")):
			data = append(data, e.Name())
		}
	}
	out := store.TableStats{}
	count := func(name, db string) error {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		defer f.Close()
		comp, _ := pipeline.DetectFromName(name)
		rd, cleanup, err := pipeline.NewReader(f, comp, false, "")
		if err != nil {
			return err
		}
		defer cleanup()
		rc := NewRowCounter(db)
		if _, err := io.Copy(rc, rd); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for d, ts := range rc.Tables() {
			if out[d] == nil {
				out[d] = map[string]int64{}
			}
			for t, n := range ts {
				out[d][t] += n
			}
		}
		return nil
	}
	for _, name := range schemas {
		prefix := strings.TrimSuffix(pipeline.StripExt(name), "-schema.sql")
		if views[prefix] {
			continue // the placeholder table of a view
		}
		db := dbOf(prefix)
		if db == "" {
			return nil, nil
		}
		if err := count(name, db); err != nil {
			return nil, err
		}
	}
	for _, name := range data {
		db := dbOf(reMdChunk.ReplaceAllString(strings.TrimSuffix(pipeline.StripExt(name), ".sql"), ""))
		if db == "" {
			return nil, nil
		}
		if err := count(name, db); err != nil {
			return nil, err
		}
	}
	return out, nil
}
