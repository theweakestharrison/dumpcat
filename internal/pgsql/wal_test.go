package pgsql

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// walWriter lays out WAL the way PostgreSQL does: pages with headers,
// MAXALIGNed records that continue on the next page (and segment) with
// XLP_FIRST_IS_CONTRECORD, a long header on the first page of a segment.
type walWriter struct {
	page, seg int64
	sysID     uint64
	tli       uint32
	buf       []byte // from LSN start
	start     LSN
	pos       int64 // write position, relative to start
	prev      LSN
	lsns      []LSN
}

func newWALWriter(segno uint64) *walWriter {
	w := &walWriter{page: 8192, seg: 64 << 10, sysID: 7698583384946407304, tli: 1}
	w.start = LSN(segno * uint64(w.seg))
	return w
}

func (w *walWriter) hdrLen(off int64) int64 {
	if (int64(w.start)+off)%w.seg == 0 {
		return longPageHeader
	}
	return shortPageHeader
}

// pageHeader writes the header of the page at off; rem is what remains of
// a record continuing on it.
func (w *walWriter) pageHeader(off int64, rem uint32) {
	w.grow(off + w.page)
	h := w.buf[off:]
	binary.LittleEndian.PutUint16(h[0:], 0xD113)
	info := uint16(0)
	if rem > 0 {
		info |= xlpFirstIsContrec
	}
	if w.hdrLen(off) == longPageHeader {
		info |= xlpLongHeader
		binary.LittleEndian.PutUint64(h[24:], w.sysID)
		binary.LittleEndian.PutUint32(h[32:], uint32(w.seg))
		binary.LittleEndian.PutUint32(h[36:], uint32(w.page))
	}
	binary.LittleEndian.PutUint16(h[2:], info)
	binary.LittleEndian.PutUint32(h[4:], w.tli)
	binary.LittleEndian.PutUint64(h[8:], uint64(w.start)+uint64(off))
	binary.LittleEndian.PutUint32(h[16:], rem)
}

func (w *walWriter) grow(n int64) {
	for int64(len(w.buf)) < n {
		w.buf = append(w.buf, make([]byte, w.page)...)
	}
}

// record appends a record (header + main data).
func (w *walWriter) record(rmid, info uint8, main []byte) LSN {
	var body []byte
	if len(main) < 256 {
		body = append([]byte{blockIDDataShort, byte(len(main))}, main...)
	} else {
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(main)))
		body = append(append([]byte{blockIDDataLong}, l[:]...), main...)
	}
	rec := make([]byte, recordHeader+len(body))
	copy(rec[recordHeader:], body)
	binary.LittleEndian.PutUint32(rec[0:], uint32(len(rec)))
	binary.LittleEndian.PutUint64(rec[8:], uint64(w.prev))
	rec[16], rec[17] = info, rmid
	crc := crc32.Update(0, crcTable, rec[recordHeader:])
	crc = crc32.Update(crc, crcTable, rec[:20])
	binary.LittleEndian.PutUint32(rec[20:], crc)

	pos := (w.pos + 7) &^ 7
	if pos%w.page == 0 {
		w.pageHeader(pos, 0)
		pos += w.hdrLen(pos)
	}
	lsn := w.start + LSN(pos)
	for left := rec; len(left) > 0; {
		page := pos - pos%w.page
		w.grow(page + w.page)
		n := copy(w.buf[pos:page+w.page], left)
		left = left[n:]
		pos += int64(n)
		if len(left) > 0 {
			w.pageHeader(pos, uint32(len(left)))
			pos += w.hdrLen(pos)
		}
	}
	w.pos, w.prev = pos, lsn
	w.lsns = append(w.lsns, lsn)
	return lsn
}

func pgTime(t time.Time) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(t.Sub(pgEpoch)/time.Microsecond))
	return b[:]
}

func (w *walWriter) commit(t time.Time) LSN { return w.record(rmXACT, xactCommit, pgTime(t)) }

func (w *walWriter) point(name string, t time.Time) LSN {
	main := append(pgTime(t), make([]byte, 64)...)
	copy(main[8:], name)
	return w.record(rmXLOG, xlogRestorePoint, main)
}

func (w *walWriter) filler(n int) LSN { return w.record(10, 0, make([]byte, n)) }

// fillTo writes records so that the next one starts exactly at off (on
// the same page as the last filler).
func (w *walWriter) fillTo(off int64) {
	for w.pos < off-300 {
		w.filler(min(int(off-w.pos-300), 4000))
	}
	w.filler(int(off - ((w.pos + 7) &^ 7) - recordHeader - 2))
}

// segments writes the WAL as segment files (zeros after the last record).
func (w *walWriter) segments(t *testing.T, dir string) []string {
	t.Helper()
	w.grow(((w.pos + w.seg - 1) / w.seg) * w.seg)
	var paths []string
	for off := int64(0); off < int64(len(w.buf)); off += w.seg {
		name := SegName(w.tli, uint64(int64(w.start)+off)/uint64(w.seg), uint64(w.seg))
		p := filepath.Join(dir, name)
		end := min(off+w.seg, int64(len(w.buf)))
		data := make([]byte, w.seg)
		copy(data, w.buf[off:end])
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

var t1 = time.Date(2026, 10, 20, 5, 9, 3, 684916000, time.UTC)

func TestLSNAndNames(t *testing.T) {
	l, err := ParseLSN("16/B374D848")
	if err != nil || l != LSN(0x16_B374D848) || l.String() != "16/B374D848" {
		t.Fatalf("%v %v", l, err)
	}
	for _, bad := range []string{"", "16", "16/", "/1", "G/1", "123456789/1"} {
		if _, err := ParseLSN(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if n := SegName(1, 0x10, 16<<20); n != "000000010000000000000010" {
		t.Fatal(n)
	}
	if n := SegName(2, 0x1FF, 16<<20); n != "0000000200000001000000FF" {
		t.Fatal(n)
	}
	tli, no, ok := ParseSegName("0000000200000001000000FF", 16<<20)
	if !ok || tli != 2 || no != 0x1FF {
		t.Fatal(tli, no, ok)
	}
	if _, _, ok := ParseSegName("000000020000000100000100", 16<<20); ok {
		t.Fatal("segment number beyond the log file accepted")
	}
	if tl, ok := HistoryTLI("00000002.history"); !ok || tl != 2 {
		t.Fatal("history")
	}
	if s := SlotName(7, "1b4e28ba-2fa1-11d2-883f-0016d3cca427"); s != "dumpcat_job7_1b4e28ba2fa1" {
		t.Fatal(s)
	}
}

func TestScanSegment(t *testing.T) {
	dir := t.TempDir()
	w := newWALWriter(3)
	w.filler(100)
	c1 := w.commit(t1)
	w.filler(9000) // spans two pages
	rp := w.point("before_drop", t1.Add(time.Second))
	w.fillTo(w.page*2 - 8) // the next header is split across a page boundary
	if c := w.commit(t1.Add(2 * time.Second)); int64(c-w.start) != w.page*2-8 {
		t.Fatalf("the commit starts at %s", c)
	}
	w.record(rmXACT, xactAbort, pgTime(t1.Add(3*time.Second)))
	paths := w.segments(t, dir)
	in, err := ScanSegment(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if in.SysID != w.sysID || in.SegSize != uint64(w.seg) || in.Start != w.start || in.TLI != 1 {
		t.Fatalf("header: %+v", in)
	}
	if in.Records != len(w.lsns) || in.End != w.start+LSN(w.pos) {
		t.Fatalf("records %d (want %d), end %s (want %s)", in.Records, len(w.lsns), in.End, w.start+LSN(w.pos))
	}
	if in.Timed != 4 || !in.First.Equal(t1) || !in.Last.Equal(t1.Add(3*time.Second)) {
		t.Fatalf("times: %d %s %s", in.Timed, in.First, in.Last)
	}
	if len(in.Points) != 1 || in.Points[0].Name != "before_drop" || in.Points[0].LSN != rp.String() {
		t.Fatalf("restore points: %+v", in.Points)
	}
	_ = c1

	// a damaged record ends the valid WAL
	b, _ := os.ReadFile(paths[0])
	off := int64(rp-w.start) + recordHeader + 4
	b[off] ^= 0xFF
	os.WriteFile(paths[0], b, 0o600)
	if in, err = ScanSegment(paths[0]); err != nil || in.End != rp-w.start+w.start && in.End >= rp {
		t.Fatalf("damaged: %+v %v", in, err)
	}
	if in.End >= rp || in.Timed != 1 {
		t.Fatalf("scan went past the damaged record: end %s, timed %d", in.End, in.Timed)
	}
}

func TestScanAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	w := newWALWriter(8)
	w.fillTo(w.seg - 3000)
	w.commit(t1)
	// a commit whose record starts in the first segment and ends in the second
	w.record(rmXACT, xactCommit, append(pgTime(t1.Add(time.Second)), make([]byte, 6000)...))
	w.commit(t1.Add(2 * time.Second))
	w.record(rmXLOG, xlogSwitch, nil)
	paths := w.segments(t, dir)
	if len(paths) != 2 {
		t.Fatalf("%d segments", len(paths))
	}
	a, err := ScanSegment(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := ScanSegment(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if a.Timed != 1 || b.Timed != 1 || !b.Switch || b.End != w.start+LSN(2*w.seg) {
		t.Fatalf("one by one: %+v / %+v", a, b)
	}
	var times []time.Time
	both, err := scanSegments(paths, w.seg, func(r *walRecord) error {
		if !r.Time.IsZero() {
			times = append(times, r.Time)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 3 || !times[1].Equal(t1.Add(time.Second)) || both.Records != len(w.lsns) || !both.Switch {
		t.Fatalf("in a row: %v %+v", times, both)
	}
	// a gap: the next file is not the next segment
	other := newWALWriter(20)
	other.commit(t1)
	op := other.segments(t, t.TempDir())
	gap, err := scanSegments([]string{paths[0], op[0]}, w.seg, nil)
	if err != nil || gap.End >= w.start+LSN(w.seg) {
		t.Fatalf("gap: %+v %v", gap, err)
	}
}

func TestWALTail(t *testing.T) {
	dir := t.TempDir()
	w := newWALWriter(5)
	w.commit(t1)
	w.filler(10000)
	p := filepath.Join(dir, SegName(1, 5, uint64(w.seg))+partialExt)
	write := func() {
		data := make([]byte, w.seg) // pg_receivewal pads the file with zeros
		copy(data, w.buf)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	empty := make([]byte, w.seg)
	os.WriteFile(p, empty, 0o600)
	tl := &walTail{Path: p}
	if err := tl.Update(); err != nil || tl.Info.Records != 0 {
		t.Fatalf("nothing received yet: %+v %v", tl.Info, err)
	}
	write()
	if err := tl.Update(); err != nil || tl.Info.Records != 2 || tl.Info.Timed != 1 {
		t.Fatalf("first part: %+v %v", tl.Info, err)
	}
	w.point("rp", t1.Add(time.Second))
	w.commit(t1.Add(2 * time.Second))
	write()
	if err := tl.Update(); err != nil || tl.Info.Records != 4 || tl.Info.Timed != 3 || tl.Info.End != w.start+LSN(w.pos) || len(tl.Info.Points) != 1 {
		t.Fatalf("more: %+v %v", tl.Info, err)
	}
}
