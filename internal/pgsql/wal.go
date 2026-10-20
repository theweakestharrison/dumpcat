package pgsql

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A reader of PostgreSQL write-ahead log: segment names, LSNs, pages and
// records, enough to know what an archived segment holds (the cluster it
// belongs to, the range of its complete records, the times of commits,
// aborts and restore points, restore point names) without the server's
// pg_waldump. The page and record headers it reads have kept their layout
// since PostgreSQL 9.5; records are checked with their CRC-32C.

// LSN is a position in the WAL.
type LSN uint64

func (l LSN) String() string { return fmt.Sprintf("%X/%X", uint32(l>>32), uint32(l)) }

var reLSN = regexp.MustCompile(`^([0-9A-Fa-f]{1,8})/([0-9A-Fa-f]{1,8})$`)

// ParseLSN reads "16/B374D848".
func ParseLSN(s string) (LSN, error) {
	m := reLSN.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid LSN %q (expected the form 16/B374D848)", s)
	}
	hi, _ := strconv.ParseUint(m[1], 16, 32)
	lo, _ := strconv.ParseUint(m[2], 16, 32)
	return LSN(hi<<32 | lo), nil
}

const (
	walMagicMin         = 0xD000 // XLOG_PAGE_MAGIC of every supported version is 0xD0xx or 0xD1xx
	walMagicMax         = 0xD1FF
	xlpFirstIsContrec   = 0x0001
	xlpLongHeader       = 0x0002
	xlpOverwriteContrec = 0x0008
	shortPageHeader     = 24
	longPageHeader      = 40
	recordHeader        = 24
	maxRecord           = 1 << 30
	rmXLOG              = 0
	rmXACT              = 1
	xlogSwitch          = 0x40
	xlogRestorePoint    = 0x70
	xactOpMask          = 0x70
	xactCommit          = 0x00
	xactAbort           = 0x20
	xactCommitPrepared  = 0x30
	xactAbortPrepared   = 0x40
	blockIDDataShort    = 255
	blockIDDataLong     = 254
	blockIDOrigin       = 253
	blockIDToplevelXID  = 252
	DefaultWALSegSize   = 16 << 20
)

// pgEpoch is where PostgreSQL timestamps count from.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	reSegName     = regexp.MustCompile(`^([0-9A-F]{8})([0-9A-F]{8})([0-9A-F]{8})$`)
	reHistoryName = regexp.MustCompile(`^([0-9A-F]{8})\.history$`)
)

// SegName names segment segno of timeline tli.
func SegName(tli uint32, segno, segSize uint64) string {
	per := 0x100000000 / segSize
	return fmt.Sprintf("%08X%08X%08X", tli, segno/per, segno%per)
}

// ParseSegName reads a segment name: its timeline and its number (for
// segments of segSize bytes).
func ParseSegName(name string, segSize uint64) (tli uint32, segno uint64, ok bool) {
	m := reSegName.FindStringSubmatch(name)
	if m == nil || segSize == 0 {
		return 0, 0, false
	}
	t, _ := strconv.ParseUint(m[1], 16, 32)
	log, _ := strconv.ParseUint(m[2], 16, 32)
	seg, _ := strconv.ParseUint(m[3], 16, 32)
	per := 0x100000000 / segSize
	if seg >= per {
		return 0, 0, false
	}
	return uint32(t), log*per + seg, true
}

// IsSegName tells whether name is a WAL segment file name.
func IsSegName(name string) bool { return reSegName.MatchString(name) }

// HistoryTLI reads the timeline of a timeline history file name.
func HistoryTLI(name string) (uint32, bool) {
	m := reHistoryName.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	t, _ := strconv.ParseUint(m[1], 16, 32)
	return uint32(t), true
}

// HistoryFork reads a timeline history file (one line per ancestor:
// timeline, switch point, reason): where timeline from ended, ok false when
// the file does not list it. A hex-escaped bytea is decoded first.
func HistoryFork(content string, from uint32) (LSN, bool) {
	if h, ok := strings.CutPrefix(content, `\x`); ok {
		if b, err := hex.DecodeString(strings.TrimSpace(h)); err == nil {
			content = string(b)
		}
	}
	for _, ln := range strings.Split(content, "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if t, err := strconv.ParseUint(f[0], 10, 32); err != nil || uint32(t) != from {
			continue
		}
		if l, err := ParseLSN(f[1]); err == nil {
			return l, true
		}
	}
	return 0, false
}

// RestorePoint is a named point created by pg_create_restore_point().
type RestorePoint struct {
	Name string    `json:"name"`
	LSN  string    `json:"lsn"`
	Time time.Time `json:"time"`
}

// walRecord is what the scanner reports of one record.
type walRecord struct {
	LSN, End LSN
	TLI      uint32
	Rmgr     uint8
	Info     uint8
	Time     time.Time // commit, abort or restore point (zero otherwise)
	Commit   bool      // a commit or abort (what recovery_target_time looks at)
	Point    string    // restore point name
	Switch   bool
}

// SegInfo summarizes the WAL of a segment file (or of several in a row).
type SegInfo struct {
	SysID       uint64
	SegSize     uint64
	TLI         uint32 // timeline of the last page read
	Start       LSN    // first byte of the scanned WAL
	End         LSN    // end of the last complete, valid record (Start: none)
	Records     int
	Timed       int       // commits, aborts and restore points
	First, Last time.Time // their first and last time (record order)
	Points      []RestorePoint
	Switch      bool // ended by XLOG_SWITCH: the rest of the segment is unused
}

func (in *SegInfo) observe(r *walRecord) {
	in.Records++
	in.End = r.End
	in.TLI = r.TLI
	if !r.Time.IsZero() {
		if in.Timed == 0 {
			in.First = r.Time
		}
		in.Last = r.Time
		in.Timed++
	}
	if r.Point != "" {
		in.Points = append(in.Points, RestorePoint{Name: r.Point, LSN: r.LSN.String(), Time: r.Time})
	}
	if r.Switch {
		in.Switch = true
	}
}

// errWALEnd: no further valid record (zeros, a recycled page, a torn or
// missing record).
var errWALEnd = errors.New("end of valid WAL")

// walScan reads records from consecutive segments laid out one after the
// other (offset 0 is the first byte of the first segment). It can resume
// where it stopped once more data was written (the segment being received).
type walScan struct {
	ra       io.ReaderAt
	size     int64 // bytes available
	pageSize int64
	segSize  int64
	base     LSN // LSN of offset 0
	sysID    uint64

	page    []byte
	pageAt  int64 // offset of the loaded page (-1: none)
	pageTLI uint32
	flags   uint16
	remLen  uint32
	hdrLen  int64

	pos     int64 // where the next record starts (0: not past the first page header yet)
	prev    LSN   // LSN of the previous record (0: unknown)
	started bool
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// open reads the long header of the first page: page and segment size,
// system identifier, the LSN of offset 0.
func (s *walScan) open() error {
	var h [longPageHeader]byte
	if n, err := s.ra.ReadAt(h[:], 0); n < len(h) {
		if err == nil || errors.Is(err, io.EOF) {
			err = errWALEnd
		}
		return err
	}
	magic, info := le16(h[0:]), le16(h[2:])
	if magic < walMagicMin || magic > walMagicMax || info&xlpLongHeader == 0 {
		return errWALEnd // not written yet (a new segment is zero-filled)
	}
	s.base = LSN(le64(h[8:]))
	s.sysID = le64(h[24:])
	s.segSize = int64(le32(h[32:]))
	s.pageSize = int64(le32(h[36:]))
	if s.pageSize < 1024 || s.pageSize > 65536 || s.pageSize&(s.pageSize-1) != 0 || s.segSize < s.pageSize || s.segSize&(s.segSize-1) != 0 {
		return fmt.Errorf("not a WAL segment (page size %d, segment size %d)", s.pageSize, s.segSize)
	}
	if int64(s.base)%s.segSize != 0 {
		return fmt.Errorf("not a WAL segment (first page at %s)", s.base)
	}
	s.pageAt = -1
	return nil
}

// loadPage reads and checks the page starting at off.
func (s *walScan) loadPage(off int64) error {
	if s.pageAt == off {
		return nil
	}
	if off+s.pageSize > s.size {
		return errWALEnd
	}
	if s.page == nil {
		s.page = make([]byte, s.pageSize)
	}
	if n, err := s.ra.ReadAt(s.page, off); n < len(s.page) {
		if err == nil || errors.Is(err, io.EOF) {
			err = errWALEnd
		}
		s.pageAt = -1
		return err
	}
	p := s.page
	magic, info := le16(p[0:]), le16(p[2:])
	if magic < walMagicMin || magic > walMagicMax || LSN(le64(p[8:])) != s.base+LSN(off) {
		s.pageAt = -1
		return errWALEnd // zeros or a recycled page of older WAL
	}
	s.hdrLen = shortPageHeader
	if off%s.segSize == 0 {
		if info&xlpLongHeader == 0 || le64(p[24:]) != s.sysID {
			s.pageAt = -1
			return errWALEnd
		}
		s.hdrLen = longPageHeader
	}
	s.pageAt, s.flags, s.remLen, s.pageTLI = off, info, le32(p[16:]), le32(p[4:])
	return nil
}

func align8(n int64) int64 { return (n + 7) &^ 7 }

// dataStart moves pos past the header of its page when it is at a page start.
func (s *walScan) dataStart(pos int64) (int64, error) {
	page := pos - pos%s.pageSize
	if err := s.loadPage(page); err != nil {
		return 0, err
	}
	if pos-page < s.hdrLen {
		return page + s.hdrLen, nil
	}
	return pos, nil
}

// errOverwritten: the record being read was never completed; a later page
// starts over at at (XLP_FIRST_IS_OVERWRITE_CONTRECORD).
type errOverwritten struct{ at int64 }

func (e errOverwritten) Error() string { return "aborted continuation record" }

// readRecord reads a record of total bytes starting at pos (past the page
// header); a record continues on the next page only when that page says so.
func (s *walScan) readRecord(dst []byte, pos int64) (int64, error) {
	left := uint32(len(dst))
	for len(dst) > 0 {
		page := pos - pos%s.pageSize
		if err := s.loadPage(page); err != nil {
			return 0, err
		}
		if pos-page < s.hdrLen {
			// a continuation page: its header tells how much of the record remains
			if s.flags&xlpFirstIsContrec == 0 {
				if s.flags&xlpOverwriteContrec != 0 {
					return 0, errOverwritten{page + s.hdrLen}
				}
				return 0, errWALEnd
			}
			if s.remLen != left {
				return 0, errWALEnd
			}
			pos = page + s.hdrLen
		}
		n := copy(dst, s.page[pos-page:])
		dst = dst[n:]
		pos += int64(n)
		left -= uint32(n)
	}
	return pos, nil
}

// next returns the next valid record (errWALEnd when there is none yet).
func (s *walScan) next() (*walRecord, error) {
	if !s.started {
		pos, err := s.dataStart(0)
		if err != nil {
			return nil, err
		}
		if s.flags&xlpFirstIsContrec != 0 {
			// the end of a record of the previous segment: skip it
			left := int64(s.remLen)
			for left > 0 {
				page := pos - pos%s.pageSize
				if err := s.loadPage(page); err != nil {
					return nil, err
				}
				if pos-page < s.hdrLen {
					pos = page + s.hdrLen
				}
				n := min(left, page+s.pageSize-pos)
				left -= n
				pos += n
			}
		}
		s.pos, s.started = pos, true
	}
	for {
		pos, err := s.dataStart(align8(s.pos))
		if err != nil {
			return nil, err
		}
		page := pos - pos%s.pageSize
		if pos == page+s.hdrLen && s.flags&xlpFirstIsContrec != 0 {
			return nil, errWALEnd // the page continues a record that ended before it
		}
		total := le32(s.page[pos-page:]) // at least 8 bytes of a record are on its first page
		if total < recordHeader || total > maxRecord {
			return nil, errWALEnd
		}
		rec := make([]byte, total)
		end, err := s.readRecord(rec, pos)
		var ow errOverwritten
		if errors.As(err, &ow) {
			s.pos, s.prev = ow.at, 0
			continue
		}
		if err != nil {
			return nil, err
		}
		crc := crc32.Update(0, crcTable, rec[recordHeader:])
		crc = crc32.Update(crc, crcTable, rec[:20])
		if crc != le32(rec[20:]) {
			return nil, errWALEnd
		}
		lsn := s.base + LSN(pos)
		if prev := LSN(le64(rec[8:])); s.prev != 0 && prev != s.prev {
			return nil, errWALEnd
		}
		r := &walRecord{LSN: lsn, End: s.base + LSN(end), TLI: s.pageTLI, Rmgr: rec[17], Info: rec[16]}
		parseRecord(r, rec)
		s.prev, s.pos = lsn, end
		if r.Switch {
			// the rest of the segment is unused: the next record starts the next one
			s.pos = pos - pos%s.segSize + s.segSize
			r.End = s.base + LSN(s.pos)
		}
		return r, nil
	}
}

// parseRecord reads the time of commits, aborts and restore points, the
// name of restore points and switches (records without block references).
func parseRecord(r *walRecord, rec []byte) {
	op := r.Info & 0xF0
	interesting := (r.Rmgr == rmXLOG && (op == xlogRestorePoint || op == xlogSwitch)) || r.Rmgr == rmXACT
	if !interesting {
		return
	}
	if r.Rmgr == rmXLOG && op == xlogSwitch {
		r.Switch = true
		return
	}
	if r.Rmgr == rmXACT {
		switch r.Info & xactOpMask {
		case xactCommit, xactAbort, xactCommitPrepared, xactAbortPrepared:
		default:
			return
		}
	}
	main := mainData(rec)
	if len(main) < 8 {
		return
	}
	r.Time = pgEpoch.Add(time.Duration(int64(le64(main))) * time.Microsecond)
	if r.Rmgr == rmXACT {
		r.Commit = true
		return
	}
	name := main[8:]
	if i := strings.IndexByte(string(name), 0); i >= 0 {
		name = name[:i]
	}
	r.Point = string(name)
}

// mainData returns the main data of a record without block references
// (nil for others): it is the last part of the record.
func mainData(rec []byte) []byte {
	p := recordHeader
	for p < len(rec) {
		switch id := rec[p]; id {
		case blockIDDataShort:
			if p+2 > len(rec) {
				return nil
			}
			n := int(rec[p+1])
			return tail(rec, n)
		case blockIDDataLong:
			if p+5 > len(rec) {
				return nil
			}
			return tail(rec, int(le32(rec[p+1:])))
		case blockIDOrigin:
			p += 3
		case blockIDToplevelXID:
			p += 5
		default:
			return nil // a block reference: not a record read here
		}
	}
	return nil
}

func tail(rec []byte, n int) []byte {
	if n <= 0 || n > len(rec)-recordHeader {
		return nil
	}
	return rec[len(rec)-n:]
}

// scanAll reads every valid record from the current position on.
func (s *walScan) scanAll(info *SegInfo, fn func(*walRecord) error) error {
	for {
		r, err := s.next()
		if errors.Is(err, errWALEnd) {
			return nil
		}
		if err != nil {
			return err
		}
		info.observe(r)
		if fn != nil {
			if err := fn(r); err != nil {
				return err
			}
		}
	}
}

// ScanSegment summarizes one WAL segment file (complete or being written).
func ScanSegment(path string) (*SegInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	s := &walScan{ra: f, size: st.Size()}
	info := &SegInfo{}
	if err := s.open(); err != nil {
		if errors.Is(err, errWALEnd) {
			return info, nil
		}
		return nil, err
	}
	info.SysID, info.SegSize, info.Start, info.End = s.sysID, uint64(s.segSize), s.base, s.base
	if err := s.scanAll(info, nil); err != nil {
		return nil, err
	}
	return info, nil
}

// walTail follows the segment being received: every Update reads what was
// written since the last one.
type walTail struct {
	Path string
	Info SegInfo
	s    *walScan
}

func (t *walTail) Update() error {
	f, err := os.Open(t.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if t.s == nil {
		s := &walScan{ra: f, size: st.Size()}
		if err := s.open(); err != nil {
			if errors.Is(err, errWALEnd) {
				return nil
			}
			return err
		}
		t.s = s
		t.Info = SegInfo{SysID: s.sysID, SegSize: uint64(s.segSize), Start: s.base, End: s.base}
	}
	t.s.ra, t.s.size, t.s.pageAt = f, st.Size(), -1 // pages may have been written since
	if t.s.pos >= t.s.segSize {
		return nil // switched: complete
	}
	return t.s.scanAll(&t.Info, nil)
}

// segFiles lays consecutive segment files out one after the other.
type segFiles struct {
	files   []*os.File
	segSize int64
	size    int64
}

func (sf *segFiles) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		i := int((off + int64(n)) / sf.segSize)
		if i >= len(sf.files) {
			return n, io.EOF
		}
		in := (off + int64(n)) % sf.segSize
		m, err := sf.files[i].ReadAt(p[n:min(len(p), n+int(sf.segSize-in))], in)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// scanSegments reads the records of consecutive segment files in a row
// (records that span two segments included), calling fn for each.
func scanSegments(paths []string, segSize int64, fn func(*walRecord) error) (*SegInfo, error) {
	sf := &segFiles{segSize: segSize}
	defer func() {
		for _, f := range sf.files {
			f.Close()
		}
	}()
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		sf.files = append(sf.files, f)
	}
	sf.size = int64(len(paths)) * segSize
	s := &walScan{ra: sf, size: sf.size}
	info := &SegInfo{}
	if err := s.open(); err != nil {
		if errors.Is(err, errWALEnd) {
			return info, nil
		}
		return nil, err
	}
	if s.segSize != segSize {
		return nil, fmt.Errorf("segment size %d, expected %d", s.segSize, segSize)
	}
	info.SysID, info.SegSize, info.Start, info.End = s.sysID, uint64(s.segSize), s.base, s.base
	return info, s.scanAll(info, fn)
}
