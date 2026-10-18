package binlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeLog builds a binlog file from events (header + body bytes).
type fakeLog struct {
	b    bytes.Buffer
	offs []int64
}

func newFakeLog() *fakeLog {
	f := &fakeLog{}
	f.b.WriteString(Magic)
	return f
}

func (f *fakeLog) add(ts uint32, typ byte, serverID uint32, body []byte) int64 {
	off := int64(f.b.Len())
	var h [HeaderLen]byte
	binary.LittleEndian.PutUint32(h[0:], ts)
	h[4] = typ
	binary.LittleEndian.PutUint32(h[5:], serverID)
	size := uint32(HeaderLen + len(body) + 4) // + checksum
	binary.LittleEndian.PutUint32(h[9:], size)
	binary.LittleEndian.PutUint32(h[13:], uint32(off)+size)
	f.b.Write(h[:])
	f.b.Write(body)
	f.b.Write([]byte{1, 2, 3, 4})
	f.offs = append(f.offs, off)
	return off
}

func mariaGTID(seq uint64, domain uint32) []byte {
	b := make([]byte, 13)
	binary.LittleEndian.PutUint64(b, seq)
	binary.LittleEndian.PutUint32(b[8:], domain)
	return b
}

func (f *fakeLog) trx(ts, commitTS uint32, seq uint64) int64 {
	off := f.add(ts, TypeMariaGTID, 1, mariaGTID(seq, 0))
	f.add(ts, TypeQuery, 1, make([]byte, 30))
	f.add(commitTS, TypeXID, 1, make([]byte, 8))
	return off
}

func sample() (*fakeLog, map[string]int64) {
	f := newFakeLog()
	at := map[string]int64{}
	f.add(100, TypeFormatDescription, 1, make([]byte, 80))
	f.add(100, TypeGTIDList, 1, make([]byte, 8))
	at["t1"] = f.trx(110, 110, 1)
	at["t2"] = f.trx(200, 205, 2) // long transaction: started 200, committed 205
	f.add(206, TypeBinlogCheckpoint, 1, make([]byte, 10))
	at["t3"] = f.trx(300, 300, 3)
	at["rotate"] = f.add(400, TypeRotate, 1, []byte("\x04\x00\x00\x00\x00\x00\x00\x00mysql-bin.000002"))
	at["end"] = int64(f.b.Len())
	return f, at
}

func TestScanAndInfo(t *testing.T) {
	f, at := sample()
	var gtids []string
	end, err := Scan(bytes.NewReader(f.b.Bytes()), 0, func(e *Event) error {
		if IsGTID(e.Type) {
			gtids = append(gtids, e.GTID)
		}
		return nil
	})
	if err != nil || end != at["end"] {
		t.Fatalf("scan: end=%d err=%v", end, err)
	}
	if len(gtids) != 3 || gtids[0] != "0-1-1" || gtids[2] != "0-1-3" {
		t.Fatalf("gtids: %v", gtids)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "mysql-bin.000001")
	os.WriteFile(p, f.b.Bytes(), 0o600)
	inf, err := ScanFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !inf.Closed || inf.Truncated || !inf.HasTrx || inf.FirstTS.Unix() != 100 || inf.LastTS.Unix() != 400 || inf.Size != at["end"] {
		t.Fatalf("info: %+v", inf)
	}
	// a partially written last event
	os.WriteFile(p, f.b.Bytes()[:at["rotate"]+7], 0o600)
	inf, err = ScanFile(p)
	if err != nil || !inf.Truncated || inf.Closed || inf.Size != at["rotate"] {
		t.Fatalf("truncated info: %+v %v", inf, err)
	}
	if _, err := ScanFile(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("missing file must fail")
	}
	os.WriteFile(p, []byte("not a binlog"), 0o600)
	if _, err := ScanFile(p); !errors.Is(err, ErrNotBinlog) {
		t.Fatalf("magic: %v", err)
	}
}

func TestTailIncremental(t *testing.T) {
	f, at := sample()
	p := filepath.Join(t.TempDir(), "mysql-bin.000001")
	data := f.b.Bytes()
	os.WriteFile(p, data[:at["t2"]+5], 0o600) // cut inside an event header
	tl := &Tail{Path: p}
	if err := tl.Update(); err != nil || tl.Info.Size != at["t2"] || tl.Info.LastTS.Unix() != 110 {
		t.Fatalf("first: %+v %v", tl.Info, err)
	}
	os.WriteFile(p, data, 0o600)
	if err := tl.Update(); err != nil || tl.Info.Size != at["end"] || tl.Info.LastTS.Unix() != 400 || tl.Info.FirstTS.Unix() != 100 {
		t.Fatalf("second: %+v %v", tl.Info, err)
	}
}

func resolve(t *testing.T, data []byte, s *Stopper, from int64) (int64, bool) {
	t.Helper()
	var stop int64
	found := false
	s.StartFile(from)
	_, err := Scan(bytes.NewReader(data), 0, func(e *Event) error {
		if off, ok := s.Feed(e); ok {
			stop, found = off, true
			return io.EOF
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		stop, found = s.EndFile()
	}
	return stop, found
}

func TestStopperTime(t *testing.T) {
	f, at := sample()
	data := f.b.Bytes()
	cases := []struct {
		ts   int64
		want int64
		stop bool
	}{
		{105, at["t1"], true}, // before everything
		{111, at["t2"], true}, // after t1 committed
		{203, at["t2"], true}, // t2 started before but committed at 205: excluded whole
		{206, at["t3"], true}, // t2 committed, checkpoint ignored
		{300, at["t3"], true}, // equal second is not "before"
		{301, 0, false},       // everything applies
		{9999, 0, false},
	}
	for _, c := range cases {
		s := &Stopper{Time: time.Unix(c.ts, 0)}
		got, ok := resolve(t, data, s, 0)
		if ok != c.stop || (ok && got != c.want) {
			t.Errorf("target %d: got %d,%v want %d,%v", c.ts, got, ok, c.want, c.stop)
		}
	}
	// events before the replay start are ignored
	s := &Stopper{Time: time.Unix(105, 0)}
	if got, ok := resolve(t, data, s, at["t2"]); !ok || got != at["t2"] {
		t.Errorf("from t2: %d %v", got, ok)
	}
	// last transaction of a file closed by EndFile
	g := newFakeLog()
	g.add(100, TypeFormatDescription, 1, make([]byte, 80))
	last := g.trx(150, 160, 9)
	s = &Stopper{Time: time.Unix(155, 0)}
	if got, ok := resolve(t, g.b.Bytes(), s, 0); !ok || got != last {
		t.Errorf("end of file: %d %v", got, ok)
	}
}

func TestStopperGTID(t *testing.T) {
	f, at := sample()
	s := &Stopper{GTID: "0-1-2"}
	if got, ok := resolve(t, f.b.Bytes(), s, 0); !ok || got != at["t2"] {
		t.Fatalf("gtid: %d %v", got, ok)
	}
	s = &Stopper{GTID: "0-1-9"}
	if _, ok := resolve(t, f.b.Bytes(), s, 0); ok {
		t.Fatal("unknown gtid must not stop")
	}
}

func TestMySQLGTID(t *testing.T) {
	body := make([]byte, 60) // longer than the parse window, like MySQL 8
	body[0] = 1
	copy(body[1:17], []byte{0x3e, 0x11, 0xfa, 0x47, 0x71, 0xca, 0x11, 0xe1, 0x9e, 0x33, 0xc8, 0x0a, 0xa9, 0x42, 0x95, 0x63})
	binary.LittleEndian.PutUint64(body[17:], 42)
	f := newFakeLog()
	f.add(1, TypeFormatDescription, 1, make([]byte, 80))
	f.add(2, TypeMySQLGTID, 1, append(body, make([]byte, 40)...))
	var got string
	if _, err := Scan(bytes.NewReader(f.b.Bytes()), 0, func(e *Event) error {
		if e.Type == TypeMySQLGTID {
			got = e.GTID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != "3e11fa47-71ca-11e1-9e33-c80aa9429563:42" {
		t.Fatalf("mysql gtid: %q", got)
	}
}

func TestNamesAndGTIDs(t *testing.T) {
	if b, n, ok := SplitName("mysql-bin.000012"); !ok || b != "mysql-bin" || n != 12 {
		t.Fatal("split")
	}
	for _, bad := range []string{"mysql-bin.index", "mysql-bin.000001.idx", "x.12", ""} {
		if IsName(bad) {
			t.Errorf("%q is not a binlog name", bad)
		}
	}
	if g, err := NormalizeGTID(" 3E11FA4771CA11E19E33C80AA9429563:7 "); err != nil || g != "3e11fa47-71ca-11e1-9e33-c80aa9429563:7" {
		t.Fatalf("normalize mysql: %q %v", g, err)
	}
	if g, err := NormalizeGTID("0-11-5"); err != nil || g != "0-11-5" {
		t.Fatalf("normalize maria: %q %v", g, err)
	}
	if _, err := NormalizeGTID("abc"); err == nil {
		t.Fatal("invalid gtid accepted")
	}
	cases := []struct {
		set, gtid       string
		included, known bool
	}{
		{"0-11-100", "0-11-100", true, true},
		{"0-11-100,1-11-5", "0-11-101", false, true},
		{"1-11-5", "0-11-1", false, true},
		{"3e11fa47-71ca-11e1-9e33-c80aa9429563:1-50:60", "3e11fa47-71ca-11e1-9e33-c80aa9429563:55", false, true},
		{"3e11fa47-71ca-11e1-9e33-c80aa9429563:1-50:60", "3e11fa47-71ca-11e1-9e33-c80aa9429563:60", true, true},
		{"3e11fa47-71ca-11e1-9e33-c80aa9429563:1-50,\n4e11fa47-71ca-11e1-9e33-c80aa9429563:1-3", "4e11fa47-71ca-11e1-9e33-c80aa9429563:3", true, true},
		{"", "0-1-1", false, false},
	}
	for _, c := range cases {
		inc, known := GTIDIncluded(c.set, c.gtid)
		if inc != c.included || known != c.known {
			t.Errorf("GTIDIncluded(%q, %q) = %v,%v", c.set, c.gtid, inc, known)
		}
	}
}
