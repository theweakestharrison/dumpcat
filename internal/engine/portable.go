package engine

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dumpcat/internal/pipeline"
)

// PortableOpts rewrites a logical dump so it loads on an instance other than
// the one it was taken from.
type PortableOpts struct {
	StripDefiner      bool `json:"strip_definer"`      // drop DEFINER=user@host from views, routines, triggers and events
	ConvertCollations bool `json:"convert_collations"` // MySQL 8 *_0900_* / MariaDB *_uca1400_* / utf8mb3_* → collations every target knows
	SkipGTIDPurged    bool `json:"skip_gtid_purged"`   // drop SET @@GLOBAL.GTID_PURGED (and the SQL_LOG_BIN juggling around it)
}

func (o PortableOpts) Any() bool { return o.StripDefiner || o.ConvertCollations || o.SkipGTIDPurged }

// Labels lists the enabled rewrites, for the restore log.
func (o PortableOpts) Labels() []string {
	var out []string
	if o.StripDefiner {
		out = append(out, "strip DEFINER")
	}
	if o.ConvertCollations {
		out = append(out, "convert collations")
	}
	if o.SkipGTIDPurged {
		out = append(out, "skip GTID_PURGED")
	}
	return out
}

var (
	// DEFINER=`u`@`h`, DEFINER='u'@'h' or DEFINER=u@h ("SQL SECURITY DEFINER" has no '=')
	reDefiner = regexp.MustCompile("DEFINER\\s*=\\s*(?:`(?:[^`]|``)*`|'(?:[^'\\\\]|\\\\.)*'|[A-Za-z0-9_.$-]+)@(?:`(?:[^`]|``)*`|'(?:[^'\\\\]|\\\\.)*'|[A-Za-z0-9_.%:-]+) ?")
	// collations of one flavor only: MySQL 8 (utf8mb4_0900_ai_ci, utf8mb4_vi_0900_as_cs…)
	// and MariaDB 10.10+ (utf8mb4_uca1400_ai_ci, utf8mb4_uca1400_vietnamese_nopad_as_cs…)
	reNewCollation = regexp.MustCompile(`(?i)\b(utf8mb4|utf8mb3|utf8|ucs2|utf16|utf16le|utf32)_[a-z0-9_]*?(?:0900|uca1400)[a-z0-9_]*`)
	// MySQL 8.0.30+ names utf8 collations utf8mb3_*, unknown to MySQL 5.7
	reMb3Collation = regexp.MustCompile(`(?i)\butf8mb3_([a-z0-9_]+_(?:ci|cs|bin)|bin)\b`)
	// mydumper DDL files: table, database, view, trigger, routine and sequence schemas
	reMydumperSchema = regexp.MustCompile(`-schema(?:-create|-view|-triggers|-post|-sequence)?\.sql$`)
)

// PortableCollation maps a collation that exists in only one flavor/version
// to the closest one MySQL 5.7+, 8.x and MariaDB 10.x/11.x all know.
func PortableCollation(c string) string {
	return string(convertCollations([]byte(c)))
}

func convertCollations(line []byte) []byte {
	line = reNewCollation.ReplaceAllFunc(line, func(m []byte) []byte {
		// m aliases the line being scanned: build the replacement in a new slice
		cs, rest, _ := strings.Cut(string(m), "_")
		if strings.EqualFold(cs, "utf8mb3") {
			cs = "utf8"
		}
		rest = strings.ToLower(rest)
		if strings.HasSuffix(rest, "_bin") || strings.HasSuffix(rest, "_cs") {
			return []byte(cs + "_bin")
		}
		return []byte(cs + "_unicode_ci")
	})
	return reMb3Collation.ReplaceAll(line, []byte("utf8_$1"))
}

// PortableSQL rewrites a mysqldump stream line by line. Data lines
// (INSERT/REPLACE, one per statement in mysqldump output) are copied through
// without being buffered or modified, so huge extended inserts cost nothing
// and row data is never touched.
func PortableSQL(r io.Reader, o PortableOpts) io.Reader {
	if !o.Any() {
		return r
	}
	return &portableReader{br: bufio.NewReaderSize(r, 64*1024), o: o}
}

// a non-data line longer than this is passed through unchanged
const maxRewriteLine = 16 << 20

type portableReader struct {
	br   *bufio.Reader
	o    PortableOpts
	out  []byte // pending output
	pass bool   // inside a line copied verbatim
	skip bool   // inside a dropped multi-line statement
	err  error
}

func (p *portableReader) Read(b []byte) (int, error) {
	for len(p.out) == 0 {
		if p.err != nil {
			return 0, p.err
		}
		p.fill()
	}
	n := copy(b, p.out)
	p.out = p.out[n:]
	return n, nil
}

func isDataLine(b []byte) bool {
	return bytes.HasPrefix(b, []byte("INSERT ")) || bytes.HasPrefix(b, []byte("REPLACE "))
}

// fill produces the next piece of output; it is only called once the
// previous piece has been consumed, so it may alias bufio's buffer.
func (p *portableReader) fill() {
	chunk, err := p.br.ReadSlice('\n')
	if p.pass {
		p.out = chunk
		p.pass = err == bufio.ErrBufferFull
		if err != nil && err != bufio.ErrBufferFull {
			p.err = err
		}
		return
	}
	if err == bufio.ErrBufferFull {
		if isDataLine(chunk) && !p.skip {
			p.out, p.pass = chunk, true
			return
		}
		// a long statement line: collect it whole (up to maxRewriteLine)
		line := append([]byte(nil), chunk...)
		for err == bufio.ErrBufferFull && len(line) < maxRewriteLine {
			chunk, err = p.br.ReadSlice('\n')
			line = append(line, chunk...)
		}
		if err == bufio.ErrBufferFull {
			p.out, p.pass = line, true
			return
		}
		chunk = line
	}
	if err != nil {
		p.err = err
	}
	if len(chunk) > 0 {
		p.out = p.rewrite(chunk)
	}
}

func (p *portableReader) rewrite(line []byte) []byte {
	if isDataLine(line) && !p.skip {
		return line
	}
	trimmed := bytes.TrimRight(line, " \t\r\n")
	if p.o.SkipGTIDPurged {
		if p.skip {
			p.skip = !bytes.HasSuffix(trimmed, []byte(";"))
			return nil
		}
		switch {
		case bytes.HasPrefix(line, []byte("SET @@GLOBAL.GTID_PURGED")):
			// the GTID set may span several lines
			p.skip = !bytes.HasSuffix(trimmed, []byte(";"))
			return []byte("-- SET @@GLOBAL.GTID_PURGED skipped by DumpCat\n")
		case bytes.HasPrefix(line, []byte("SET @MYSQLDUMP_TEMP_LOG_BIN")), bytes.HasPrefix(line, []byte("SET @@SESSION.SQL_LOG_BIN")):
			// needs SUPER; binary logging follows the restore's own setting instead
			return nil
		}
	}
	if p.o.StripDefiner && bytes.Contains(line, []byte("DEFINER")) {
		line = reDefiner.ReplaceAll(line, nil)
	}
	if p.o.ConvertCollations {
		line = convertCollations(line)
	}
	return line
}

// RewriteMydumperSchemas applies o to the schema files of an extracted
// mydumper directory (table, view, trigger, routine and database DDL). The
// rewritten files are written uncompressed; myloader picks the format of each
// file from its extension. It returns the number of files rewritten.
func RewriteMydumperSchemas(dir string, o PortableOpts) (int, error) {
	if !o.Any() {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		plain := pipeline.StripExt(name)
		if e.IsDir() || !reMydumperSchema.MatchString(plain) {
			continue
		}
		if err := rewriteFile(filepath.Join(dir, name), filepath.Join(dir, plain), o); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func rewriteFile(src, dst string, o PortableOpts) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	comp, enc := pipeline.DetectFromName(src)
	rd, cleanup, err := pipeline.NewReader(in, comp, enc, "")
	if err != nil {
		return err
	}
	defer cleanup()
	tmp := dst + ".dumpcat-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, PortableSQL(rd, o))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	in.Close()
	if src != dst {
		if err := os.Remove(src); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, dst)
}
