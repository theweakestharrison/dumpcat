package engine

import (
	"context"
	"database/sql"
	"io"
	"strings"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// Driver is everything DumpCat needs to know about one database engine.
// Jobs, schedules, the compression/encryption pipeline, storages,
// retention, verification, notifications, agents and the catalog are shared;
// a driver only inspects servers, writes backups into a Sink and loads them
// back. See docs/design-multidb.md.
type Driver interface {
	Engine() string  // stored in servers.engine: "mysql", "postgres"
	Label() string   // the choice in the server form
	Product() string // in messages: "could not connect to MySQL"
	DefaultPort() int
	// Methods lists the backup methods. Method ids are unique across drivers.
	Methods() []Method
	// RestoreOptions are the choices the restore page offers for logical restores.
	RestoreOptions() []RestoreOption
	// Inspect connects and reads the version, databases (sizes and table
	// counts with withSizes), privileges and warnings.
	Inspect(ctx context.Context, s *store.Server, withSizes bool) (*dbconn.ServerInfo, error)
	// Support tells which methods can run from this host right now.
	Support(s *store.Server, info *dbconn.ServerInfo, t *Tools) []MethodSupport
	// Backup writes the backup of spec into sink.
	Backup(env *Env, spec *BackupSpec, sink Sink) (*Result, error)
	// Restore loads a logical backup into spec.Target.
	Restore(env *Env, src RestoreSource, spec *RestoreSpec) error
	// Compat compares the source of a backup with the restore target: notes
	// for the user and the options a restore there needs.
	Compat(run *store.Run, target *store.Server, info *dbconn.ServerInfo, same bool) ([]string, RestoreOpts)
	// CheckName refuses a database name a restore must not create.
	CheckName(name string) error
}

// Method is one way of backing up a server of a driver.
type Method struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Threads     string `json:"threads"`     // what a job's thread count means
	Physical    bool   `json:"physical"`    // copies the data directory: the whole instance
	Incremental bool   `json:"incremental"` // has incremental backups (physical MySQL)
	Drill       bool   `json:"drill"`       // restore drills can test its backups
	Estimates   bool   `json:"estimates"`   // drills can compare the server's row estimates instead of counting
	LogArchive  bool   `json:"log_archive"` // continuous log archiving and point-in-time recovery
	Users       string `json:"users"`       // label of the "users & grants" option ("" = none)
	UsersHelp   string `json:"users_help"`
}

// RestoreOption is a checkbox of the restore page. Key is the field of the
// restore request; Help may quote names in `backticks`.
type RestoreOption struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Help    string   `json:"help"`
	Group   string   `json:"group"` // "options", or "compat" (ticked from the target check)
	Default bool     `json:"default"`
	Methods []string `json:"methods,omitempty"` // only for these methods (empty: all)
	Needs   string   `json:"needs,omitempty"`   // "users": only when the backup holds users or roles
}

// MethodSupport tells whether a method can run, and why not.
type MethodSupport struct {
	Method  string `json:"method"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// RestoreOpts are the rewrites of a logical restore onto another instance.
// The JSON names are the fields of the restore request.
type RestoreOpts struct {
	// MySQL
	StripDefiner      bool `json:"strip_definer"`
	ConvertCollations bool `json:"convert_collations"`
	SkipGTIDPurged    bool `json:"skip_gtid_purged"`
	// PostgreSQL
	NoOwner      bool `json:"no_owner"`      // pg_restore --no-owner
	NoPrivileges bool `json:"no_privileges"` // pg_restore --no-privileges
}

func (o RestoreOpts) Portable() PortableOpts {
	return PortableOpts{StripDefiner: o.StripDefiner, ConvertCollations: o.ConvertCollations, SkipGTIDPurged: o.SkipGTIDPurged}
}

func (o RestoreOpts) Any() bool { return o.Portable().Any() || o.NoOwner || o.NoPrivileges }

// Labels names the chosen options for the log.
func (o RestoreOpts) Labels() []string {
	l := o.Portable().Labels()
	if o.NoOwner {
		l = append(l, "no ownership (--no-owner)")
	}
	if o.NoPrivileges {
		l = append(l, "no privileges (--no-privileges)")
	}
	return l
}

// RestoreSpec is one logical restore.
type RestoreSpec struct {
	Run          *store.Run
	Target       *store.Server      // with its restore account
	Info         *dbconn.ServerInfo // the target, inspected just before
	Databases    []store.DBEntry    // the databases of the backup to restore
	Rename       map[string]string  // source name → new name ("" or missing: same name)
	DropExisting bool
	NoBinlog     bool // MySQL: SET sql_log_bin=0
	RestoreUsers bool
	Threads      int
	Opts         RestoreOpts
	// FastMode relaxes the target's durability while loading; it returns the
	// function that puts it back (MySQL; nil: off).
	FastMode func() (release func())
}

// TargetName is the name database src is restored under.
func (s *RestoreSpec) TargetName(src string) string {
	if v := strings.TrimSpace(s.Rename[src]); v != "" {
		return v
	}
	return src
}

// Renamed tells whether any database gets a new name.
func (s *RestoreSpec) Renamed() bool {
	for _, d := range s.Databases {
		if s.TargetName(d.Name) != d.Name {
			return true
		}
	}
	return false
}

// RestoreSource is the runner's side of a restore: the files of the backup
// on the chosen storage. A driver never sees storages or the catalog.
type RestoreSource interface {
	// Files maps file names without the compression/encryption extension.
	Files() map[string]store.BackupFile
	Describe() string // storage and path, for the log
	// Open returns the decoded content. done(true) reads the rest and fails
	// when the SHA-256 differs from the catalog; done(false) just closes.
	Open(ctx context.Context, f store.BackupFile) (r io.Reader, done func(ok bool) error, err error)
	// Preflight opens and decodes the start of every file: a wrong
	// passphrase or a missing file stops the restore before anything is dropped.
	Preflight(ctx context.Context, files []store.BackupFile) error
	SetPhase(phase string)
	SetEstimate(bytes int64)
}

// Sandbox is what restore drills need from a driver: read the restored
// copies and drop them again.
type Sandbox interface {
	// TempName returns db+suffix, db shortened to fit the identifier limit.
	TempName(db, suffix string) string
	OpenCheck(ctx context.Context, s *store.Server) (CheckConn, error)
	// Assert runs one read-only query inside database ("" = none) and returns
	// the first column of the first row (sql.ErrNoRows when there is none).
	Assert(ctx context.Context, s *store.Server, database, query string) (sql.NullString, error)
	// DropDatabases drops the given databases (drill copies only). left are
	// the names that could not be dropped, errs why.
	DropDatabases(ctx context.Context, s *store.Server, names []string, noLog bool, lg *tasklog.Logger) (left, errs []string, err error)
}

// CheckConn reads restored copies on a sandbox server.
type CheckConn interface {
	// Fold maps a database or table name the way the server compares them.
	Fold(name string) string
	// Tables lists the tables holding rows, named like the backup's row counts.
	Tables(ctx context.Context, db string) ([]string, error)
	// Objects counts tables and views (compared with backups made before row counting).
	Objects(ctx context.Context, db string) (int, error)
	Count(ctx context.Context, db, table string) (int64, error)
	// Estimate returns the server's own row estimates (ErrNoEstimates when it has none).
	Estimate(ctx context.Context, db string) (map[string]int64, error)
	Close() error
}

// FindMethod returns the method id of d.
func FindMethod(d Driver, id string) (Method, bool) {
	for _, m := range d.Methods() {
		if m.ID == id {
			return m, true
		}
	}
	return Method{}, false
}
