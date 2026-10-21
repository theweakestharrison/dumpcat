package mssql

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

func TestCompareLSN(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"37000000012800001", "37000000012800001", 0},
		{"37000000012800001", "37000000012800002", -1},
		{"9000000001200001", "37000000012800001", -1}, // fewer digits
		{"0037000000012800001", "37000000012800001", 0},
		{"", "1", -1},
		{"100000000000000000000", "99999999999999999999", 1}, // past int64
	} {
		if got := CompareLSN(c.a, c.b); got != c.want {
			t.Errorf("CompareLSN(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]string{
		"37000000012800001":         "37000000012800001",
		" 00037000000012800001 ":    "37000000012800001",
		"00000025:00000080:0001":    "37000000012800001", // 0x25 = 37, 0x80 = 128
		"0x0000002F:000000B0:0001":  "47000000017600001",
		"1a:c8:2":                   "26000000020000002",
		"FFFFFFFF:FFFFFFFF:FFFF":    "4294967295429496729565535",
		"00000025:00000080:0001 ":   "37000000012800001",
		"0000000000000000000000001": "1",
	} {
		got, err := ParseLSN(in)
		if err != nil || got != want {
			t.Errorf("ParseLSN(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1/2", "12:34", "0x12:34:56:78", "1.5", "-1"} {
		if _, err := ParseLSN(bad); err == nil {
			t.Errorf("ParseLSN(%q) accepted", bad)
		}
	}
}

func TestQuote(t *testing.T) {
	if got := QuoteName("a]b"); got != "[a]]b]" {
		t.Errorf("QuoteName = %s", got)
	}
	if got := QuoteString(`it's C:\x`); got != `N'it''s C:\x'` {
		t.Errorf("QuoteString = %s", got)
	}
}

func TestCheckWithOptions(t *testing.T) {
	for _, ok := range []string{"", "BUFFERCOUNT = 64, MAXTRANSFERSIZE = 4194304", "blocksize=65536", "DESCRIPTION = N'nightly, full'", "CONTINUE_AFTER_ERROR"} {
		if err := CheckWithOptions(ok); err != nil {
			t.Errorf("CheckWithOptions(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"NORECOVERY", "buffercount = 64, norecovery", "COPY_ONLY", "BUFFERCOUNT = 1; DROP DATABASE x", "MAXTRANSFERSIZE = 1 --", "STATS = 1",
		"BUFFERCOUNT = (SELECT 1)", "DESCRIPTION = N'a' + N'b'", "ENCRYPTION (ALGORITHM = AES_256)"} {
		if err := CheckWithOptions(bad); err == nil {
			t.Errorf("CheckWithOptions(%q) accepted", bad)
		}
	}
}

func TestFolderPaths(t *testing.T) {
	f := folder{Server: `D:\Backup\DumpCat\`, Files: localFiles(`/mnt/sql`)}
	if got := f.server("x.bak"); got != `D:\Backup\DumpCat\x.bak` {
		t.Errorf("server path = %s", got)
	}
	if got := joinServer(`\\nas\sql`, "x.bak"); got != `\\nas\sql\x.bak` {
		t.Errorf("UNC path = %s", got)
	}
	if got := joinServer("/var/opt/mssql/dumpcat/", "x.bak"); got != "/var/opt/mssql/dumpcat/x.bak" {
		t.Errorf("linux path = %s", got)
	}
	for p, win := range map[string]bool{`C:\x`: true, `\\nas\s`: true, "/var/opt": false, "c:/x": true} {
		if windowsPath(p) != win {
			t.Errorf("windowsPath(%q) != %v", p, win)
		}
	}
	name := fileName("my shop/db", ".bak")
	if !strings.HasPrefix(name, "dumpcat-") || !strings.HasSuffix(name, ".bak") || strings.ContainsAny(name, `/\ `) || name == fileName("my shop/db", ".bak") {
		t.Errorf("fileName = %s", name)
	}
	if f.same() || f.throughAgent() || !strings.Contains(f.describe("reads"), "reads it as /mnt/sql") {
		t.Errorf("a mapped folder: same %v, agent %v, %s", f.same(), f.throughAgent(), f.describe("reads"))
	}
	for _, c := range [][2]string{{`D:\Backup\`, `d:\backup`}, {"/var/opt/mssql/x/", "/var/opt/mssql/x"}, {`\\NAS\sql`, `\\nas\SQL\`}} {
		if !sameDir(c[0], c[1]) {
			t.Errorf("sameDir(%q, %q)", c[0], c[1])
		}
	}
	if sameDir("/var/opt/mssql/X", "/var/opt/mssql/x") {
		t.Error("Linux paths are case-sensitive")
	}
}

// agentFiles stands in for the agent's folder.
type agentFiles struct{ dir string }

func (a agentFiles) Open(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("no")
}
func (a agentFiles) Create(context.Context, string, io.Reader) (int64, error) { return 0, nil }
func (a agentFiles) Remove(context.Context, string) error                     { return nil }
func (a agentFiles) Free(context.Context) (int64, error)                      { return -1, nil }
func (a agentFiles) Dir() string                                              { return a.dir }
func (a agentFiles) String() string                                           { return "agent sql01 (" + a.dir + ")" }

func TestResolveFolder(t *testing.T) {
	native := "/var/opt/mssql/dumpcat"
	other := `D:\Backup`
	if runtime.GOOS == "windows" {
		native, other = `D:\Backup`, "/var/opt/mssql/dumpcat"
	}
	s := &store.Server{}
	if _, err := resolveFolder(s, ""); err == nil {
		t.Error("no folder at all accepted")
	}
	f, err := resolveFolder(s, native)
	if err != nil || f.Server != native || f.Files != localFiles(native) || !f.same() || f.describe("reads") != native {
		t.Errorf("default folder: %+v %v", f, err)
	}
	if _, err := resolveFolder(&store.Server{BackupDir: other}, ""); err == nil || !strings.Contains(err.Error(), "seen from DumpCat") {
		t.Errorf("a path of another kind of host without a local path: %v", err)
	}
	local := filepath.Join(t.TempDir(), "x")
	f, err = resolveFolder(&store.Server{BackupDir: other, BackupDirLocal: local}, "")
	if err != nil || f.Server != other || f.Files != localFiles(local) {
		t.Errorf("mapped folder: %+v %v", f, err)
	}
	// behind an agent: the agent's folder, unless the folder is shared
	agent := int64(3)
	defer func(old func(int64, string) (Files, error)) { AgentFolder = old }(AgentFolder)
	AgentFolder = nil
	if _, err := resolveFolder(&store.Server{BackupDir: native, AgentID: &agent}, ""); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("an agent without agent connections: %v", err)
	}
	var asked string
	AgentFolder = func(id int64, dir string) (Files, error) {
		asked = dir
		if id != agent {
			return nil, errors.New("unknown agent")
		}
		return agentFiles{"/var/opt/mssql/backup"}, nil
	}
	f, err = resolveFolder(&store.Server{BackupDir: `D:\Backup`, AgentID: &agent}, "")
	if err != nil || asked != `D:\Backup` || !f.throughAgent() || f.same() || !strings.Contains(f.describe("reads"), "through agent sql01") {
		t.Errorf("through the agent: %+v %v", f, err)
	}
	if f, err = resolveFolder(&store.Server{BackupDir: other, BackupDirLocal: local, AgentID: &agent}, ""); err != nil || f.throughAgent() {
		t.Errorf("a shared folder of an agent's server: %+v %v", f, err)
	}
	other4 := int64(4)
	if _, err := resolveFolder(&store.Server{BackupDir: native, AgentID: &other4}, ""); err == nil || !strings.Contains(err.Error(), "Backup folder seen from DumpCat") {
		t.Errorf("an agent without the folder: %v", err)
	}
	wg := int64(5)
	if _, err := resolveFolder(&store.Server{BackupDir: native, WGPeerID: &wg}, ""); err == nil || !strings.Contains(err.Error(), "WireGuard") {
		t.Errorf("WireGuard without a local path: %v", err)
	}
	if _, err := resolveFolder(&store.Server{BackupDir: native, BackupDirLocal: "relative/dir"}, ""); err == nil {
		t.Error("relative local path accepted")
	}
}

func TestCheckName(t *testing.T) {
	d := Driver{}
	for _, ok := range []string{"shop", "shop restore", "a]b", strings.Repeat("x", 128), "Bán hàng"} {
		if err := d.CheckName(ok); err != nil {
			t.Errorf("CheckName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", " shop", "shop ", "MSDB", "master", "tempdb", "a\nb", strings.Repeat("x", 129)} {
		if err := d.CheckName(bad); err == nil {
			t.Errorf("CheckName(%q) accepted", bad)
		}
	}
	if got := d.TempName(strings.Repeat("é", 130), "__drill_7"); len([]rune(got)) != 128 || !strings.HasSuffix(got, "__drill_7") {
		t.Errorf("TempName = %d runes", len([]rune(got)))
	}
}

func TestPlaceFiles(t *testing.T) {
	list := []FileListEntry{
		{Logical: "shop", Type: "D", FileID: 1}, {Logical: "shop_log", Type: "L", FileID: 2},
		{Logical: "Archive 2024", Type: "D", FileID: 3}, {Logical: "fs", Type: "S", FileID: 65537},
	}
	got := placeFiles(list, "shop_copy", `D:\Data`, `L:\Logs`, nil, map[string]bool{strings.ToLower(`D:\Data\shop_copy.mdf`): true})
	want := map[string]string{
		"shop":         `D:\Data\shop_copy_2.mdf`, // the name is taken by another database
		"shop_log":     `L:\Logs\shop_copy_log.ldf`,
		"Archive 2024": `D:\Data\shop_copy_Archive@202024.ndf`,
		"fs":           `D:\Data\shop_copy_fs`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s → %s, want %s", k, got[k], v)
		}
	}
	// replacing a database keeps its own files
	got = placeFiles(list[:2], "shop", "/var/opt/mssql/data", "/var/opt/mssql/data", map[string]string{"shop": "/data/shop.mdf"}, map[string]bool{})
	if got["shop"] != "/data/shop.mdf" || got["shop_log"] != "/var/opt/mssql/data/shop_log.ldf" {
		t.Errorf("replace: %v", got)
	}
}

func TestStopClause(t *testing.T) {
	at := time.Date(2026, 10, 21, 8, 30, 15, 250e6, time.UTC)
	if got := stopClause(engine.PITRTarget{Type: "time", Time: &at}, 7*time.Hour); got != "STOPAT = N'2026-10-21T15:30:15.250'" {
		t.Errorf("time: %s", got)
	}
	if got := stopClause(engine.PITRTarget{Type: "mark", Name: "before 'x'"}, 0); got != "STOPATMARK = N'before ''x'''" {
		t.Errorf("mark: %s", got)
	}
	if got := stopClause(engine.PITRTarget{Type: "lsn", LSN: "37000000012800001"}, 0); got != "STOPATMARK = N'lsn:37000000012800001'" {
		t.Errorf("lsn: %s", got)
	}
}

func TestLocalTime(t *testing.T) {
	wall := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC) // SQL Server's local 15:00 at UTC+7
	got := localTime(map[string]any{"t": wall}, "t", 7*time.Hour)
	if !got.Equal(time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("localTime = %v", got)
	}
}

// a chain of log backups for the plan tests
func logFile(id int64, db, first, last string, at time.Time, marks ...string) store.LogFile {
	h := &Header{Database: db, Type: "log", FirstLSN: first, LastLSN: last, RecoveryModel: "FULL", Start: at}
	for _, m := range marks {
		h.Marks = append(h.Marks, Mark{Name: m, LSN: first})
	}
	if marks == nil {
		h.Marks = []Mark{}
	}
	return store.LogFile{ID: id, Seq: id, FileName: "trn" + string(rune('0'+id)), FirstTS: at.Add(-15 * time.Minute), LastTS: at, SizeBytes: 100, Meta: headerMeta(h)}
}

func TestPlan(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	// the headers as the catalog keeps them: JSON
	run := &store.Run{ID: 5, Method: store.MethodMSSQL, Type: store.TypeFull, Meta: map[string]any{"mssql": map[string]any{
		"shop":  headerMeta(&Header{Database: "shop", Type: "full", FirstLSN: "100", LastLSN: "150", RecoveryModel: "FULL", Finish: t0}),
		"stats": headerMeta(&Header{Database: "stats", Type: "full", FirstLSN: "10", LastLSN: "20", RecoveryModel: "SIMPLE", Finish: t0}),
	}}}
	d := Driver{}
	files := []store.LogFile{
		logFile(1, "shop", "90", "120", t0.Add(-10*time.Minute)), // before the backup ended: not needed
		logFile(2, "shop", "120", "200", t0.Add(15*time.Minute)),
		logFile(3, "shop", "200", "300", t0.Add(30*time.Minute), "before_migration"),
		logFile(4, "shop", "300", "400", t0.Add(45*time.Minute)),
	}
	plan := func(tg engine.PITRTarget, files []store.LogFile) (*engine.LogPlan, error) {
		base, err := d.Base(run, &tg)
		if err != nil {
			return nil, err
		}
		return d.Plan(&engine.PlanInput{Base: base, Files: files, Target: tg, Loc: time.UTC})
	}
	ids := func(p *engine.LogPlan) []int64 { return p.FileIDs() }
	at := t0.Add(20 * time.Minute)
	p, err := plan(engine.PITRTarget{Type: "time", Time: &at}, files)
	if err != nil || len(ids(p)) != 2 || ids(p)[0] != 2 || ids(p)[1] != 3 {
		t.Fatalf("time plan: %v %v", p, err)
	}
	if st := p.State.(*replayState); st.problems["stats"] == "" || len(st.ids["shop"]) != 2 {
		t.Errorf("SIMPLE database not refused: %+v", st)
	}
	if !p.Coverage.Equal(t0.Add(30 * time.Minute)) {
		t.Errorf("coverage %v", p.Coverage)
	}
	if p, err = plan(engine.PITRTarget{Type: "mark", Name: "before_migration"}, files); err != nil || len(ids(p)) != 2 {
		t.Errorf("mark plan: %v %v", p, err)
	}
	if _, err = plan(engine.PITRTarget{Type: "mark", Name: "nope"}, files); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown mark: %v", err)
	}
	if p, err = plan(engine.PITRTarget{Type: "lsn", LSN: "250"}, files); err != nil || len(ids(p)) != 2 {
		t.Errorf("lsn plan: %v %v", p, err)
	}
	if p, err = plan(engine.PITRTarget{Type: "latest"}, files); err != nil || len(ids(p)) != 3 {
		t.Errorf("latest plan: %v %v", p, err)
	}
	late := t0.Add(2 * time.Hour)
	if _, err = plan(engine.PITRTarget{Type: "time", Time: &late}, files); err == nil || !strings.Contains(err.Error(), "only archived up to") {
		t.Errorf("time after the archive: %v", err)
	}
	// a log backup taken elsewhere: the chain breaks after file 2
	gap := []store.LogFile{files[1], logFile(5, "shop", "260", "300", t0.Add(30*time.Minute))}
	if _, err = plan(engine.PITRTarget{Type: "latest"}, gap); err != nil {
		t.Errorf("latest with a gap: %v", err)
	}
	if _, err = plan(engine.PITRTarget{Type: "time", Time: &late}, gap); err == nil || !strings.Contains(err.Error(), "breaks") {
		t.Errorf("time past a gap: %v", err)
	}
	// the first log backup after the backup starts too late
	if _, err = plan(engine.PITRTarget{Type: "time", Time: &at}, files[2:]); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("gap after the backup: %v", err)
	}
	// a base after the target
	early := t0.Add(-time.Hour)
	if _, err := d.Base(run, &engine.PITRTarget{Type: "time", Time: &early}); err == nil {
		t.Error("a backup that ended after the target is a base")
	}
}

func TestCheckFetched(t *testing.T) {
	t0 := time.Now()
	f2, f3 := logFile(2, "shop", "120", "200", t0), logFile(3, "shop", "200", "300", t0)
	f4 := logFile(4, "shop", "310", "400", t0)
	plan := &engine.LogPlan{State: &replayState{target: engine.PITRTarget{Type: "latest"}, ids: map[string][]int64{"shop": {2, 3}}, problems: map[string]string{}}}
	c := Driver{}.NewCheck(nil, plan)
	if _, err := c.Add(engine.LocalLog{Name: "2", File: &f2}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add(engine.LocalLog{Name: "4", File: &f4}); err == nil {
		t.Error("a file that does not continue the chain was accepted")
	}
	if _, err := c.Add(engine.LocalLog{Name: "3", File: &f3}); err != nil {
		t.Fatal(err)
	}
	rp, err := c.Finish()
	if err != nil || len(rp.State.(*replayState).files["shop"]) != 2 {
		t.Errorf("finish: %v %v", rp, err)
	}
}

func TestHeadersRoundTrip(t *testing.T) {
	h := &Header{Database: "shop", Type: "full", FirstLSN: "37000000012800001", LastLSN: "37000000016000001", BackupSetGUID: "A1B2", RecoveryModel: "FULL",
		Finish: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	meta := map[string]any{"mssql": map[string]any{"shop": headerMeta(h)}}
	got := headersOf(meta)["shop"]
	if got == nil || got.LastLSN != h.LastLSN || !got.Finish.Equal(h.Finish) || !got.PITR() {
		t.Errorf("round trip: %+v", got)
	}
	if lh := logHeader(headerMeta(h)); lh == nil || lh.Database != "shop" {
		t.Errorf("logHeader: %+v", lh)
	}
	if headersOf(map[string]any{}) != nil {
		t.Error("headers from nothing")
	}
}

func TestErrText(t *testing.T) {
	if got := osError(errorString("Cannot open backup device 'x'. Operating system error 5(Access is denied.).")); got != 5 {
		t.Errorf("osError = %d", got)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
