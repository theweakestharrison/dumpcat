package mssql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"
)

// Header is what DumpCat records of one backup file (RESTORE HEADERONLY):
// the LSNs that chain full, differential and log backups together.
type Header struct {
	Database      string    `json:"database"`
	Type          string    `json:"type"` // full, differential, log
	FirstLSN      string    `json:"first_lsn"`
	LastLSN       string    `json:"last_lsn"`
	CheckpointLSN string    `json:"checkpoint_lsn,omitempty"`
	DatabaseLSN   string    `json:"database_backup_lsn,omitempty"`   // the full backup a differential or log backup follows
	DiffBaseLSN   string    `json:"differential_base_lsn,omitempty"` // differential: its full backup's checkpoint
	DiffBaseGUID  string    `json:"differential_base_guid,omitempty"`
	BackupSetGUID string    `json:"backup_set_guid,omitempty"`
	FamilyGUID    string    `json:"family_guid,omitempty"`
	RecoveryModel string    `json:"recovery_model,omitempty"` // FULL, BULK-LOGGED, SIMPLE
	CopyOnly      bool      `json:"copy_only,omitempty"`
	Checksums     bool      `json:"checksums,omitempty"`
	BulkLogged    bool      `json:"bulk_logged,omitempty"` // a log backup holding minimally logged changes: no STOPAT inside it
	Damaged       bool      `json:"damaged,omitempty"`
	Start         time.Time `json:"start"`
	Finish        time.Time `json:"finish"`
	Size          int64     `json:"size,omitempty"`
	Compressed    int64     `json:"compressed_size,omitempty"`
	Marks         []Mark    `json:"marks,omitempty"` // marked transactions in a log backup
}

// Mark is a marked transaction (BEGIN TRAN name WITH MARK) a log backup holds.
type Mark struct {
	Name string    `json:"name"`
	LSN  string    `json:"lsn"`
	Time time.Time `json:"time"`
}

// PITR tells whether log backups can follow a backup of this database.
func (h *Header) PITR() bool { return h.RecoveryModel == "FULL" || h.RecoveryModel == "BULK-LOGGED" }

var backupTypes = map[int64]string{1: "full", 2: "log", 5: "differential"}

// row reads the current row of rows into a map: column name → value, with
// uniqueidentifiers as text and decimals (LSNs) as digits.
func row(rows *sql.Rows) (map[string]any, error) {
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(types))
	ptrs := make([]any, len(types))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(types))
	for i, t := range types {
		v := vals[i]
		if b, ok := v.([]byte); ok {
			if t.DatabaseTypeName() == "UNIQUEIDENTIFIER" && len(b) == 16 {
				var u mssqldb.UniqueIdentifier
				if u.Scan(b) == nil {
					v = u.String()
				}
			} else {
				v = string(b)
			}
		}
		out[strings.ToLower(t.Name())] = v
	}
	return out, nil
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func num(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int16:
		return int64(v)
	case uint8:
		return int64(v)
	case float64:
		return int64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	return 0
}

func flag(m map[string]any, k string) bool { return num(m, k) != 0 }

// localTime reads a datetime column: SQL Server writes backup times in its
// own local time, the driver hands them over as UTC wall clock.
func localTime(m map[string]any, k string, offset time.Duration) time.Time {
	t, ok := m[k].(time.Time)
	if !ok || t.IsZero() {
		return time.Time{}
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC).Add(-offset)
}

// readHeader reads the header of the backup file at path (as SQL Server sees
// it); a file holds one backup set.
func readHeader(ctx context.Context, db *sql.DB, path string, offset time.Duration) (*Header, error) {
	rows, err := db.QueryContext(ctx, "RESTORE HEADERONLY FROM DISK = "+QuoteString(path))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var h *Header
	for rows.Next() {
		m, err := row(rows)
		if err != nil {
			return nil, err
		}
		h = &Header{Database: str(m, "databasename"), Type: backupTypes[num(m, "backuptype")],
			FirstLSN: str(m, "firstlsn"), LastLSN: str(m, "lastlsn"), CheckpointLSN: str(m, "checkpointlsn"),
			DatabaseLSN: str(m, "databasebackuplsn"), DiffBaseLSN: str(m, "differentialbaselsn"), DiffBaseGUID: str(m, "differentialbaseguid"),
			BackupSetGUID: str(m, "backupsetguid"), FamilyGUID: str(m, "familyguid"), RecoveryModel: strings.ToUpper(str(m, "recoverymodel")),
			CopyOnly: flag(m, "iscopyonly"), Checksums: flag(m, "hasbackupchecksums"), BulkLogged: flag(m, "hasbulkloggeddata"), Damaged: flag(m, "isdamaged"),
			Start: localTime(m, "backupstartdate", offset), Finish: localTime(m, "backupfinishdate", offset),
			Size: num(m, "backupsize"), Compressed: num(m, "compressedbackupsize")}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, errors.New("the file holds no backup set")
	}
	if h.Type == "" {
		return nil, errors.New("the file holds a kind of backup DumpCat does not restore (file or filegroup backup)")
	}
	return h, nil
}

// historyHeader reads the same from msdb's backup history (for accounts that
// may not run RESTORE HEADERONLY).
func historyHeader(ctx context.Context, db *sql.DB, path string, offset time.Duration) (*Header, error) {
	rows, err := db.QueryContext(ctx, `SELECT TOP 1 bs.database_name, bs.type, bs.first_lsn, bs.last_lsn, bs.checkpoint_lsn, bs.database_backup_lsn,
		bs.differential_base_lsn, bs.differential_base_guid, bs.backup_set_uuid, bs.family_guid, bs.recovery_model, bs.is_copy_only,
		bs.has_backup_checksums, bs.has_bulk_logged_data, bs.is_damaged, bs.backup_start_date, bs.backup_finish_date, bs.backup_size, bs.compressed_backup_size
		FROM msdb.dbo.backupset bs JOIN msdb.dbo.backupmediafamily mf ON mf.media_set_id = bs.media_set_id
		WHERE mf.physical_device_name = @p1 ORDER BY bs.backup_set_id DESC`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("not in msdb's backup history")
	}
	m, err := row(rows)
	if err != nil {
		return nil, err
	}
	types := map[string]string{"D": "full", "I": "differential", "L": "log"}
	return &Header{Database: str(m, "database_name"), Type: types[str(m, "type")],
		FirstLSN: str(m, "first_lsn"), LastLSN: str(m, "last_lsn"), CheckpointLSN: str(m, "checkpoint_lsn"), DatabaseLSN: str(m, "database_backup_lsn"),
		DiffBaseLSN: str(m, "differential_base_lsn"), DiffBaseGUID: str(m, "differential_base_guid"), BackupSetGUID: str(m, "backup_set_uuid"),
		FamilyGUID: str(m, "family_guid"), RecoveryModel: strings.ReplaceAll(strings.ToUpper(str(m, "recovery_model")), "_", "-"),
		CopyOnly: flag(m, "is_copy_only"), Checksums: flag(m, "has_backup_checksums"), BulkLogged: flag(m, "has_bulk_logged_data"), Damaged: flag(m, "is_damaged"),
		Start: localTime(m, "backup_start_date", offset), Finish: localTime(m, "backup_finish_date", offset),
		Size: num(m, "backup_size"), Compressed: num(m, "compressed_backup_size")}, nil
}

// header reads a file's header from the file, else from msdb.
func header(ctx context.Context, db *sql.DB, path string, offset time.Duration) (*Header, error) {
	h, err := readHeader(ctx, db, path, offset)
	if err == nil {
		return h, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if h2, err2 := historyHeader(ctx, db, path, offset); err2 == nil {
		return h2, nil
	}
	return nil, err
}

// marks lists the marked transactions committed in a log range (msdb).
// msdb keeps the history of every database that ever had the name, and a
// database created again starts at similar LSNs: only marks since this one
// was created (or restored) count.
func marks(ctx context.Context, db *sql.DB, database, first, last string, offset time.Duration) ([]Mark, error) {
	rows, err := db.QueryContext(ctx, `SELECT h.mark_name, CAST(h.lsn AS nvarchar(32)), h.mark_time FROM msdb.dbo.logmarkhistory h
		JOIN sys.databases d ON d.name = h.database_name
		WHERE h.database_name = @p1 AND h.lsn >= CAST(@p2 AS numeric(25,0)) AND h.lsn < CAST(@p3 AS numeric(25,0)) AND h.mark_time >= d.create_date
		ORDER BY h.lsn`, database, first, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mark
	for rows.Next() {
		var name, lsn string
		var t time.Time
		if err := rows.Scan(&name, &lsn, &t); err != nil {
			return nil, err
		}
		out = append(out, Mark{Name: name, LSN: lsn, Time: localTime(map[string]any{"t": t}, "t", offset)})
	}
	return out, rows.Err()
}

// FileListEntry is a file of a backed-up database (RESTORE FILELISTONLY).
type FileListEntry struct {
	Logical  string
	Physical string
	Type     string // D data, L log, S FILESTREAM / memory-optimized, F full-text
	FileID   int64
}

func fileList(ctx context.Context, db *sql.DB, path string) ([]FileListEntry, error) {
	rows, err := db.QueryContext(ctx, "RESTORE FILELISTONLY FROM DISK = "+QuoteString(path))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileListEntry
	for rows.Next() {
		m, err := row(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, FileListEntry{Logical: str(m, "logicalname"), Physical: str(m, "physicalname"), Type: strings.ToUpper(str(m, "type")), FileID: num(m, "fileid")})
	}
	return out, rows.Err()
}

// headersOf reads the headers a run recorded, per database.
func headersOf(meta map[string]any) map[string]*Header {
	raw, ok := meta["mssql"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out map[string]*Header
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// logHeader reads the header an archived log backup recorded.
func logHeader(meta map[string]any) *Header {
	b, err := json.Marshal(meta)
	if err != nil {
		return nil
	}
	var h Header
	if json.Unmarshal(b, &h) != nil || h.Database == "" || h.LastLSN == "" {
		return nil
	}
	return &h
}

// headerMeta turns a header into catalog meta (JSON numbers and strings).
func headerMeta(h *Header) map[string]any {
	b, _ := json.Marshal(h)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ---- LSNs ----

// CompareLSN compares two LSNs in SQL Server's decimal form
// (numeric(25,0)): -1, 0 or 1. An empty LSN is the smallest.
func CompareLSN(a, b string) int {
	a, b = strings.TrimLeft(strings.TrimSpace(a), "0"), strings.TrimLeft(strings.TrimSpace(b), "0")
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

var (
	reLSNDecimal = regexp.MustCompile(`^\d{1,25}$`)
	reLSNHex     = regexp.MustCompile(`^(?:0x)?([0-9A-Fa-f]{1,8}):([0-9A-Fa-f]{1,8}):([0-9A-Fa-f]{1,4})$`)
)

// ParseLSN reads an LSN as people copy it: the decimal form of the backup
// history (37000000012800001) or the hex form of fn_dblog and error
// messages (00000025:00000080:0001), and returns the decimal form.
func ParseLSN(s string) (string, error) {
	s = strings.TrimSpace(s)
	if reLSNDecimal.MatchString(s) {
		return strings.TrimLeft(s, "0"), nil
	}
	m := reLSNHex.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("invalid LSN %q: enter it like 37000000012800001 or 00000025:00000080:0001", s)
	}
	vlf, _ := strconv.ParseUint(m[1], 16, 64)
	block, _ := strconv.ParseUint(m[2], 16, 64)
	slot, _ := strconv.ParseUint(m[3], 16, 64)
	// decimal = VLF × 10^15 + block × 10^5 + slot
	n := new(big.Int).Mul(new(big.Int).SetUint64(vlf), new(big.Int).Exp(big.NewInt(10), big.NewInt(15), nil))
	n.Add(n, new(big.Int).Mul(new(big.Int).SetUint64(block), big.NewInt(100000)))
	n.Add(n, new(big.Int).SetUint64(slot))
	return n.String(), nil
}
