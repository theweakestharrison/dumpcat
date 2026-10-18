// Package binlog reads MySQL/MariaDB binary log files (format v4) just far
// enough to find event boundaries, timestamps and GTIDs. The archiver uses
// it to record the time range of every archived file; point-in-time
// recovery uses it to turn a target time or GTID into an exact stop position.
package binlog

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	Magic     = "\xfebin"
	HeaderLen = 19
)

// Event types used here (MySQL and MariaDB share the low numbers).
const (
	TypeQuery             = 2
	TypeStop              = 3
	TypeRotate            = 4
	TypeFormatDescription = 15
	TypeXID               = 16
	TypeHeartbeat         = 27
	TypeMySQLGTID         = 33
	TypeAnonymousGTID     = 34
	TypePreviousGTIDs     = 35
	TypeHeartbeatV2       = 41
	TypeBinlogCheckpoint  = 161
	TypeMariaGTID         = 162
	TypeGTIDList          = 163
	TypeStartEncryption   = 164
)

var ErrNotBinlog = errors.New("not a binlog file (missing magic header)")

// Event is one binlog event header (plus the GTID for GTID events).
type Event struct {
	Offset    int64 // start position in the file
	Size      int64
	Type      byte
	Timestamp uint32
	ServerID  uint32
	GTID      string // MariaDB "domain-server-seq" or MySQL "uuid:gno"
}

func (e *Event) Time() time.Time { return time.Unix(int64(e.Timestamp), 0) }

// IsGTID reports whether t starts a transaction (GTID event).
func IsGTID(t byte) bool {
	return t == TypeMySQLGTID || t == TypeAnonymousGTID || t == TypeMariaGTID
}

// IsMeta reports events that describe the log itself and never change data.
func IsMeta(t byte) bool {
	switch t {
	case TypeStop, TypeRotate, TypeFormatDescription, TypeHeartbeat, TypePreviousGTIDs, TypeHeartbeatV2,
		TypeBinlogCheckpoint, TypeGTIDList, TypeStartEncryption:
		return true
	}
	return false
}

// Scan reads events from r, which must be positioned at offset start of a
// binlog file (0 = beginning, the magic header is then checked). fn is called
// for every complete event; returning io.EOF from fn stops the scan without
// error. Scan returns the offset just after the last complete event and
// io.ErrUnexpectedEOF when the file ends in the middle of an event.
func Scan(r io.Reader, start int64, fn func(e *Event) error) (int64, error) {
	br := bufio.NewReaderSize(r, 256*1024)
	off := start
	if start == 0 {
		var m [4]byte
		if _, err := io.ReadFull(br, m[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if string(m[:]) != Magic {
			return 0, ErrNotBinlog
		}
		off = 4
	}
	var h [HeaderLen]byte
	body := make([]byte, 64)
	for {
		n, err := io.ReadFull(br, h[:])
		if err != nil {
			if n == 0 && errors.Is(err, io.EOF) {
				return off, nil
			}
			return off, io.ErrUnexpectedEOF
		}
		e := Event{
			Offset:    off,
			Timestamp: binary.LittleEndian.Uint32(h[0:4]),
			Type:      h[4],
			ServerID:  binary.LittleEndian.Uint32(h[5:9]),
			Size:      int64(binary.LittleEndian.Uint32(h[9:13])),
		}
		if e.Size < HeaderLen {
			return off, fmt.Errorf("corrupt event at position %d (size %d)", off, e.Size)
		}
		rest := e.Size - HeaderLen
		if IsGTID(e.Type) && rest > 0 {
			b := body[:min(rest, int64(len(body)))]
			if _, err := io.ReadFull(br, b); err != nil {
				return off, io.ErrUnexpectedEOF
			}
			e.GTID = parseGTID(e.Type, e.ServerID, b)
			rest -= int64(len(b))
		}
		if rest > 0 {
			if k, err := br.Discard(int(rest)); int64(k) != rest || err != nil {
				return off, io.ErrUnexpectedEOF
			}
		}
		if err := fn(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return off + e.Size, nil
			}
			return off, err
		}
		off += e.Size
	}
}

func parseGTID(t byte, serverID uint32, b []byte) string {
	switch t {
	case TypeMariaGTID:
		if len(b) < 12 {
			return ""
		}
		seq := binary.LittleEndian.Uint64(b[0:8])
		domain := binary.LittleEndian.Uint32(b[8:12])
		return fmt.Sprintf("%d-%d-%d", domain, serverID, seq)
	case TypeMySQLGTID:
		if len(b) < 25 {
			return ""
		}
		u := hex.EncodeToString(b[1:17])
		gno := int64(binary.LittleEndian.Uint64(b[17:25]))
		return fmt.Sprintf("%s-%s-%s-%s-%s:%d", u[0:8], u[8:12], u[12:16], u[16:20], u[20:32], gno)
	}
	return ""
}

// Info summarizes a binlog file. Event times are not monotonic (replayed
// statements keep their original SET TIMESTAMP), so FirstTS/LastTS are the
// earliest/latest event times, not the first/last events.
type Info struct {
	Size      int64 // bytes up to the end of the last complete event
	FirstTS   time.Time
	LastTS    time.Time
	Events    int
	HasTrx    bool // contains at least one transaction / data event
	Closed    bool // ends with a rotate or stop event
	Truncated bool // the file ends in the middle of an event
}

// ScanFile summarizes a binlog file on disk.
func ScanFile(path string) (*Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	inf := &Info{}
	var last byte
	end, err := Scan(f, 0, func(e *Event) error {
		inf.observe(e)
		last = e.Type
		return nil
	})
	inf.Size = end
	if errors.Is(err, io.ErrUnexpectedEOF) {
		inf.Truncated = true
		err = nil
	}
	if err != nil {
		return nil, err
	}
	inf.Closed = last == TypeRotate || last == TypeStop
	return inf, nil
}

func (inf *Info) observe(e *Event) {
	t := e.Time()
	if inf.Events == 0 || t.Before(inf.FirstTS) {
		inf.FirstTS = t
	}
	if inf.Events == 0 || t.After(inf.LastTS) {
		inf.LastTS = t
	}
	inf.Events++
	if !IsMeta(e.Type) {
		inf.HasTrx = true
	}
}

// Tail follows a growing binlog file incrementally (used for the file that
// is still being streamed).
type Tail struct {
	Path string
	Info Info
	off  int64
}

// Update scans the bytes appended since the last call.
func (t *Tail) Update() error {
	f, err := os.Open(t.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if t.off > 0 {
		if _, err := f.Seek(t.off, io.SeekStart); err != nil {
			return err
		}
	}
	end, err := Scan(f, t.off, func(e *Event) error {
		t.Info.observe(e)
		return nil
	})
	t.off = end
	t.Info.Size = end
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil // the rest is still being written
	}
	return err
}

// ---- file names ----

var nameRe = regexp.MustCompile(`^(.+)\.(\d{6,})$`)

// SplitName splits "mysql-bin.000012" into ("mysql-bin", 12).
func SplitName(name string) (string, int64, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return m[1], n, true
}

// IsName reports whether name looks like a binlog file (not an index).
func IsName(name string) bool {
	_, _, ok := SplitName(name)
	return ok
}

// ---- stop point resolution ----

// Stopper finds where replay must stop for a target time or GTID. Feed it
// every event of the files in order (calling StartFile before each file).
// A transaction is replayed when all of its events are older than the target
// time: the commit is the last event of a transaction, and binlog order is
// commit order, so the first transaction touching the target time ends the
// replay — never in the middle of a transaction.
type Stopper struct {
	Time time.Time // zero = no time limit
	GTID string    // stop before this transaction

	from     int64 // ignore events before this offset (base position)
	trxStart int64
	trxMax   uint32
	inTrx    bool
}

// StartFile resets per-file state; from is the replay start offset in it.
func (s *Stopper) StartFile(from int64) {
	s.from = from
	s.inTrx = false
}

func (s *Stopper) late(ts uint32) bool {
	return !s.Time.IsZero() && !time.Unix(int64(ts), 0).Before(s.Time)
}

// Feed returns (offset, true) when replay must stop before offset.
func (s *Stopper) Feed(e *Event) (int64, bool) {
	if e.Offset < s.from {
		return 0, false
	}
	if IsGTID(e.Type) {
		if s.inTrx && s.late(s.trxMax) {
			return s.trxStart, true
		}
		if s.GTID != "" && strings.EqualFold(e.GTID, s.GTID) {
			return e.Offset, true
		}
		s.inTrx, s.trxStart, s.trxMax = true, e.Offset, e.Timestamp
		return 0, false
	}
	if IsMeta(e.Type) {
		return 0, false
	}
	if s.inTrx {
		if e.Timestamp > s.trxMax {
			s.trxMax = e.Timestamp
		}
		return 0, false
	}
	// servers without GTID events: decide per event
	if s.late(e.Timestamp) {
		return e.Offset, true
	}
	return 0, false
}

// EndFile closes the last transaction of a file (transactions never span files).
func (s *Stopper) EndFile() (int64, bool) {
	if s.inTrx && s.late(s.trxMax) {
		s.inTrx = false
		return s.trxStart, true
	}
	s.inTrx = false
	return 0, false
}

var (
	mariaGTIDRe = regexp.MustCompile(`^(\d+)-(\d+)-(\d+)$`)
	mysqlGTIDRe = regexp.MustCompile(`^([0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}):(\d+)$`)
)

// NormalizeGTID validates a user supplied GTID and returns it in the form
// produced by the parser.
func NormalizeGTID(g string) (string, error) {
	g = strings.ToLower(strings.TrimSpace(g))
	if mariaGTIDRe.MatchString(g) {
		return g, nil
	}
	if m := mysqlGTIDRe.FindStringSubmatch(g); m != nil {
		u := strings.ReplaceAll(m[1], "-", "")
		return fmt.Sprintf("%s-%s-%s-%s-%s:%s", u[0:8], u[8:12], u[12:16], u[16:20], u[20:32], m[2]), nil
	}
	return "", fmt.Errorf("invalid GTID: %q (MariaDB: domain-server-seq, MySQL: uuid:number)", g)
}

// GTIDIncluded reports whether the GTID set recorded with a backup (MariaDB
// "0-1-100,1-2-5" or MySQL "uuid:1-100:105,uuid2:1-3") already contains
// gtid. ok is false when the set does not tell.
func GTIDIncluded(set, gtid string) (included, ok bool) {
	set = strings.ToLower(strings.Join(strings.Fields(set), ""))
	if set == "" {
		return false, false
	}
	if m := mariaGTIDRe.FindStringSubmatch(gtid); m != nil {
		domain, _ := strconv.ParseUint(m[1], 10, 64)
		seq, _ := strconv.ParseUint(m[3], 10, 64)
		for _, p := range strings.Split(set, ",") {
			f := strings.Split(p, "-")
			if len(f) != 3 {
				continue
			}
			d, err1 := strconv.ParseUint(f[0], 10, 64)
			s, err2 := strconv.ParseUint(f[2], 10, 64)
			if err1 != nil || err2 != nil || d != domain {
				continue
			}
			return seq <= s, true
		}
		return false, true // domain unknown to the backup: not yet executed
	}
	uuid, gnoStr, found := strings.Cut(gtid, ":")
	if !found {
		return false, false
	}
	gno, err := strconv.ParseInt(gnoStr, 10, 64)
	if err != nil {
		return false, false
	}
	for _, p := range strings.Split(set, ",") {
		parts := strings.Split(p, ":")
		if len(parts) < 2 || strings.ReplaceAll(parts[0], "-", "") != strings.ReplaceAll(uuid, "-", "") {
			continue
		}
		for _, iv := range parts[1:] {
			lo, hi, isRange := strings.Cut(iv, "-")
			a, err := strconv.ParseInt(lo, 10, 64)
			if err != nil {
				continue
			}
			b := a
			if isRange {
				if b, err = strconv.ParseInt(hi, 10, 64); err != nil {
					continue
				}
			}
			if gno >= a && gno <= b {
				return true, true
			}
		}
		return false, true
	}
	return false, true
}
