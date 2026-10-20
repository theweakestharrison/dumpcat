package pgsql

import (
	"strings"
	"testing"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

const seg16 = 16 << 20

func baseRun(end string, consistent time.Time) *store.Run {
	fin := consistent
	return &store.Run{ID: 1, Method: store.MethodPgBase, Status: store.StatusSuccess, StartedAt: consistent.Add(-time.Minute), FinishedAt: &fin,
		Meta: map[string]any{"system_id": "7698583384946407304", "start_lsn": "0/9000028", "end_lsn": end, "timeline": float64(1),
			"wal_segment_size": float64(seg16), "consistent_at": float64(consistent.Unix())}}
}

// walRow is an archived segment as the catalog returns it (meta decoded from JSON).
func walRow(id int64, tli uint32, segno uint64, first, last time.Time, extra map[string]any) store.LogFile {
	sid := int64(1)
	name := SegName(tli, segno, seg16)
	m := map[string]any{"tli": float64(tli), "segno": float64(segno), "system_id": "7698583384946407304", "seg_size": float64(seg16),
		"start": LSN(segno * seg16).String(), "end": LSN((segno+1)*seg16 - 100).String()}
	for k, v := range extra {
		m[k] = v
	}
	if m["partial"] == true {
		name += partialExt
	}
	return store.LogFile{ID: id, FileName: name, FirstTS: first, LastTS: last, SizeBytes: seg16, Meta: m,
		Locations: []store.LogLocation{{StorageID: &sid, Status: store.LocOK}}}
}

func names(p *engine.LogPlan) string {
	var out []string
	for _, f := range p.Files {
		out = append(out, f.FileName)
	}
	if p.Partial != "" {
		out = append(out, "+"+p.Partial)
	}
	return strings.Join(out, " ")
}

func TestWALPlan(t *testing.T) {
	at := func(m int) time.Time { return t1.Add(-48*time.Hour + time.Duration(m)*time.Minute) }
	run := baseRun("0/90000F0", at(0))
	d := Driver{}
	plan := func(files []store.LogFile, cur engine.LogCurrent, tg engine.PITRTarget) (*engine.LogPlan, error) {
		t.Helper()
		if err := engine.NormalizeTarget(d, &tg); err != nil {
			t.Fatal(err)
		}
		b, err := d.Base(run, &tg)
		if err != nil {
			return nil, err
		}
		return d.Plan(&engine.PlanInput{Base: b, Files: files, Current: cur, Target: tg, Loc: time.UTC})
	}
	files := []store.LogFile{
		walRow(1, 1, 8, at(-5), at(-1), nil), // before the backup: not needed
		walRow(2, 1, 9, at(-1), at(2), nil),
		walRow(3, 1, 10, at(3), at(10), map[string]any{"points": []any{map[string]any{"name": "rp1", "lsn": "0/A000200", "time": at(5).Format(time.RFC3339Nano)}}}),
		walRow(4, 1, 11, at(11), at(20), nil),
		walRow(5, 1, 12, at(21), at(30), nil),
		{ID: 6, FileName: "00000002.history", Meta: map[string]any{"history": true, "tli": float64(2), "pinned": true}, Locations: files0Loc()},
		walRow(7, 2, 12, at(25), at(31), nil), // the source was promoted inside segment 12
		walRow(8, 2, 13, time.Time{}, time.Time{}, nil),
		walRow(9, 1, 30, at(90), at(91), map[string]any{"system_id": "1"}), // another cluster
	}
	now := at(40)
	cur := engine.LogCurrent{Name: SegName(2, 14, seg16), Events: true, First: at(35), Last: at(39),
		Meta:   map[string]any{"system_id": "7698583384946407304", "end": "0/E000400", "partial": true},
		Status: engine.LogStatus{State: "streaming", Current: SegName(2, 14, seg16), CheckedAt: &now}}

	p, err := plan(files, cur, engine.PITRTarget{Type: "latest"})
	want := "00000002.history 000000010000000000000009 00000001000000000000000A 00000001000000000000000B 00000001000000000000000C 00000002000000000000000C 00000002000000000000000D +00000002000000000000000E"
	if err != nil || names(p) != want || !p.Coverage.Equal(now) {
		t.Fatalf("latest: %v\n%s\nwant %s (coverage %s)", err, names(p), want, p.Coverage)
	}
	// a time: the segments up to the first one starting well after it, plus that one
	tm := at(4)
	if p, err = plan(files, cur, engine.PITRTarget{Type: "time", Time: &tm}); err != nil || names(p) != "00000002.history 000000010000000000000009 00000001000000000000000A 00000001000000000000000B" {
		t.Fatalf("time: %v %s", err, names(p))
	}
	// before the backup is consistent
	early := at(-3)
	if _, err = plan(files, cur, engine.PITRTarget{Type: "time", Time: &early}); err == nil {
		t.Fatal("a time before the backup")
	}
	// later than the archive reaches (the stream is not level)
	cur.Status.LagBytes = 4096
	late := at(39).Add(30 * time.Second)
	if _, err = plan(files, cur, engine.PITRTarget{Type: "time", Time: &late}); err == nil || !strings.Contains(err.Error(), "only archived up to") {
		t.Fatalf("beyond the coverage: %v", err)
	}
	cur.Status.LagBytes = 0
	// a restore point and an LSN
	if p, err = plan(files, cur, engine.PITRTarget{Type: "name", Name: "rp1"}); err != nil || names(p) != "00000002.history 000000010000000000000009 00000001000000000000000A 00000001000000000000000B" {
		t.Fatalf("name: %v %s", err, names(p))
	}
	if _, err = plan(files, cur, engine.PITRTarget{Type: "name", Name: "nope"}); err == nil {
		t.Fatal("unknown restore point")
	}
	if p, err = plan(files, cur, engine.PITRTarget{Type: "lsn", LSN: "0/9100000"}); err != nil || names(p) != "00000002.history 000000010000000000000009 00000001000000000000000A" {
		t.Fatalf("lsn: %v %s", err, names(p))
	}
	if _, err = plan(files, cur, engine.PITRTarget{Type: "lsn", LSN: "0/8000000"}); err == nil {
		t.Fatal("an LSN before the end of the backup")
	}
	if _, err = plan(files, cur, engine.PITRTarget{Type: "lsn", LSN: "0/F000000"}); err == nil {
		t.Fatal("an LSN not received yet")
	}
	// a gap ends the chain (and is reported); without the stream the
	// segment holding the end of the backup must be archived
	gap := []store.LogFile{files[1], files[3]}
	if p, err = plan(gap, engine.LogCurrent{}, engine.PITRTarget{Type: "latest"}); err != nil || names(p) != "000000010000000000000009" || len(p.Warnings) != 1 {
		t.Fatalf("gap: %v %s %v", err, names(p), p.Warnings)
	}
	if _, err = plan(files[3:5], engine.LogCurrent{}, engine.PITRTarget{Type: "latest"}); err == nil || !strings.Contains(err.Error(), "000000010000000000000009") {
		t.Fatalf("start missing: %v", err)
	}
	// a pg_dump backup is no base
	dump := *run
	dump.Method = store.MethodPgDump
	if _, err := d.Base(&dump, &engine.PITRTarget{Type: "latest"}); err == nil {
		t.Fatal("pg_dump base")
	}
}

func files0Loc() []store.LogLocation {
	sid := int64(1)
	return []store.LogLocation{{StorageID: &sid, Status: store.LocOK}}
}

func TestWALTargets(t *testing.T) {
	d := Driver{}
	for _, tg := range []engine.PITRTarget{{Type: "lsn", LSN: "0/3000150"}, {Type: "name", Name: "before drop"}, {Type: "latest"}} {
		if err := engine.NormalizeTarget(d, &tg); err != nil {
			t.Errorf("%+v: %v", tg, err)
		}
	}
	for _, tg := range []engine.PITRTarget{{Type: "lsn", LSN: "3000150"}, {Type: "name"}, {Type: "name", Name: strings.Repeat("x", 64)},
		{Type: "name", Name: "a\nb"}, {Type: "gtid", GTID: "0-1-1"}, {Type: "position", File: "x.000001", Pos: 4}} {
		if err := engine.NormalizeTarget(d, &tg); err == nil {
			t.Errorf("%+v accepted", tg)
		}
	}
	if c := restoreCommand(false); !strings.Contains(c, walDirName) || !strings.Contains(c, "%f") || !strings.Contains(c, "%p") {
		t.Fatal(c)
	}
}
