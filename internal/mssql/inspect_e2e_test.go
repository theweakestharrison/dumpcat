package mssql

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"dumpcat/internal/store"
)

// TestInspectE2E inspects a real SQL Server as a sysadmin: no warning about
// permissions or the backup folder, the method works; a folder seen from
// DumpCat that is not SQL Server's is found out. Like TestMSSQLE2E:
//
//	DUMPCAT_E2E_MSSQL='sa:secret@127.0.0.1:1433' DUMPCAT_E2E_MSSQL_DIR=/var/opt/mssql/dumpcat-e2e go test ./internal/mssql -run InspectE2E -v
func TestInspectE2E(t *testing.T) {
	dsn := os.Getenv("DUMPCAT_E2E_MSSQL")
	if dsn == "" {
		t.Skip("set DUMPCAT_E2E_MSSQL=user:password@host:port to run against a disposable SQL Server")
	}
	at := strings.LastIndex(dsn, "@")
	user, pass, _ := strings.Cut(dsn[:at], ":")
	host, portS, _ := strings.Cut(dsn[at+1:], ":")
	port, _ := strconv.Atoi(portS)
	s := &store.Server{Engine: store.EngineMSSQL, Name: "e2e", Host: host, Port: port, Username: user, Password: pass,
		BackupDir: os.Getenv("DUMPCAT_E2E_MSSQL_DIR"), BackupDirLocal: os.Getenv("DUMPCAT_E2E_MSSQL_LOCAL")}
	ctx := context.Background()
	db, err := Open(ctx, s, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const odd = "dumpcat_e2e_ms_insp o'd]d"
	db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+QuoteName(odd))
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+QuoteName(odd)); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+QuoteName(odd))
	info, err := Driver{}.Inspect(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s on %s (%s), %s; folder %s; account %s; grants %v", info.Edition, info.Version, info.Platform, info.Hostname, info.CurrentUser,
		info.BackupDir, info.ServiceAccount, info.Grants)
	for _, w := range info.Warnings {
		if !strings.Contains(w, "SIMPLE") {
			t.Errorf("warning for a sysadmin: %s", w)
		}
	}
	if info.BackupDirError != "" {
		t.Errorf("backup folder: %s", info.BackupDirError)
	}
	found := false
	for _, d := range info.Databases {
		found = found || d.Name == odd && !d.System
	}
	if !found {
		t.Errorf("%s not listed: %+v", odd, info.Databases)
	}
	if ms := (Driver{}).Support(s, info, nil); !ms[0].OK {
		t.Errorf("support: %s", ms[0].Message)
	}
	// a folder DumpCat sees that SQL Server does not
	wrong := *s
	wrong.BackupDir, wrong.BackupDirLocal = "/var/opt/mssql/no-such-folder", t.TempDir()
	if info, err = (Driver{}).Inspect(ctx, &wrong, false); err != nil || !strings.Contains(info.BackupDirError, "not the same folder") {
		t.Errorf("a wrong mapping: %v %q", err, info.BackupDirError)
	}
}
