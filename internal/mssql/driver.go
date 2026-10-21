package mssql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// Driver is the Microsoft SQL Server driver.
type Driver struct{}

var (
	_ engine.LogArchiver    = Driver{}
	_ engine.Sandbox        = Driver{}
	_ engine.SandboxChecker = Driver{}
	_ engine.Differ         = Driver{}
)

func (Driver) Engine() string   { return store.EngineMSSQL }
func (Driver) Label() string    { return "Microsoft SQL Server" }
func (Driver) Product() string  { return "SQL Server" }
func (Driver) DefaultPort() int { return 1433 }

var methods = []engine.Method{
	{ID: store.MethodMSSQL, Title: "Native backup (BACKUP DATABASE)", Icon: "hdd", Differential: true, Drill: true, Estimates: true, LogArchive: true,
		Description: "SQL Server's own full and differential backups of each database, WITH CHECKSUM and compressed where the edition allows, checked with RESTORE VERIFYONLY. SQL Server writes them into its backup folder, DumpCat moves them to the job's storages. Restore databases under new names or onto other servers; scheduled log backups add point-in-time recovery.",
		Threads:     "Databases backed up at the same time (each one needs room in the backup folder until DumpCat has read it)"},
}

func (Driver) Methods() []engine.Method { return methods }

var restoreOptions = []engine.RestoreOption{
	{Key: "drop_existing", Group: "options", Label: "Replace existing databases (WITH REPLACE)",
		Help: "A database of the same name is overwritten: its sessions are closed (`SET SINGLE_USER WITH ROLLBACK IMMEDIATE`), then `RESTORE … WITH REPLACE` reuses its files. Unchecked, a restore onto a name that exists stops before anything is changed."},
}

func (Driver) RestoreOptions() []engine.RestoreOption { return restoreOptions }

// CheckName refuses a database name a restore must not create: SQL Server
// keeps 128 characters, and the system databases are never restored over.
func (Driver) CheckName(name string) error {
	if strings.TrimSpace(name) != name || name == "" || utf8.RuneCountInString(name) > 128 || !utf8.ValidString(name) ||
		strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("invalid target database name: %q", name)
	}
	if SystemDatabases[strings.ToLower(name)] {
		return fmt.Errorf("%s is a system database: restore it under another name", name)
	}
	return nil
}

// Support: BACKUP DATABASE runs on the server itself, so what matters is
// the edition, the account and the backup folder.
func (Driver) Support(sv *store.Server, info *dbconn.ServerInfo, t *engine.Tools) []engine.MethodSupport {
	ms := engine.MethodSupport{Method: store.MethodMSSQL}
	switch {
	case info.BackupDirError != "":
		ms.Message = info.BackupDirError
		return []engine.MethodSupport{ms}
	case strings.Contains(strings.Join(info.Warnings, " "), "does not write backups to a disk"):
		ms.Message = "this edition cannot write backups to a disk"
		return []engine.MethodSupport{ms}
	}
	ms.OK = true
	parts := []string{versionName(info.Major) + " " + info.Version}
	if info.Compression {
		parts = append(parts, "compressed")
	} else {
		parts = append(parts, "not compressed by the edition (DumpCat compresses)")
	}
	parts = append(parts, "backup folder "+info.BackupDir)
	ms.Message = strings.Join(parts, " · ")
	return []engine.MethodSupport{ms}
}

// Compat compares the backup's server with the target: SQL Server restores
// backups of its own version and older ones only.
func (Driver) Compat(run *store.Run, target *store.Server, info *dbconn.ServerInfo, same bool) ([]string, engine.RestoreOpts) {
	var warn []string
	srcVersion, _ := run.Meta["server_version"].(string)
	src, _ := strconv.Atoi(strings.Split(srcVersion, ".")[0])
	if src > 0 && info.Major > 0 && info.Major < src {
		warn = append(warn, fmt.Sprintf("The target (%s, %s) is older than the source (%s, %s): SQL Server cannot restore a backup of a newer version, the restore will fail.",
			versionName(info.Major), info.Version, versionName(src), srcVersion))
	}
	if strings.HasPrefix(strings.ToLower(info.Edition), "express") {
		for _, d := range run.Databases {
			if d.SizeBytes > 10<<30 {
				warn = append(warn, fmt.Sprintf("The target is SQL Server Express, which holds at most 10 GB of data per database: %s had %s.", d.Name, engine.HumanBytes(d.SizeBytes)))
			}
		}
	}
	if !same {
		warn = append(warn, "Logins are not part of a database backup: users of the restored databases whose login does not exist on the target (same SID) are listed in the restore log as orphaned.")
	}
	return warn, engine.RestoreOpts{}
}
