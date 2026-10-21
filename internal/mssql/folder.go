package mssql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

// BACKUP writes on the SQL Server host and RESTORE reads there, so every
// backup file passes through a folder that SQL Server and DumpCat both reach,
// seen under two paths: Server, as SQL Server names it on its own host, and
// Local, as DumpCat opens it. They are the same path when DumpCat runs on the
// SQL Server host; otherwise the folder is a share (\\nas\sqlbackup for
// both, or a Windows share mounted on Linux) or a volume of a container.
// SQL Server writes each .bak / .trn there, DumpCat reads it into the usual
// pipeline (compression, encryption, SHA-256, every storage of the job) and
// deletes it; a restore does the opposite.
type folder struct {
	Server string
	Local  string
}

// windowsPath tells whether p is a path of a Windows host: C:\…, \\host\share.
func windowsPath(p string) bool {
	return len(p) >= 2 && p[1] == ':' || strings.HasPrefix(p, `\\`) || strings.Contains(p, `\`)
}

// server is the path SQL Server opens for name.
func (f folder) server(name string) string {
	return joinServer(f.Server, name)
}

// joinServer joins a file name to a directory of the SQL Server host.
func joinServer(dir, name string) string {
	sep := "/"
	if windowsPath(dir) {
		sep = `\`
	}
	return strings.TrimRight(dir, `\/`) + sep + name
}

func (f folder) local(name string) string { return filepath.Join(f.Local, name) }

// resolveFolder picks the folder of a server: the one configured, or the
// instance's default backup folder when DumpCat runs on the SQL Server host.
func resolveFolder(s *store.Server, defaultDir string) (folder, error) {
	srv := strings.TrimSpace(s.BackupDir)
	if srv == "" {
		srv = strings.TrimSpace(defaultDir)
	}
	if srv == "" {
		return folder{}, errors.New("no backup folder: enter the folder SQL Server writes its backups to (its default backup folder could not be read)")
	}
	local := strings.TrimSpace(s.BackupDirLocal)
	if local == "" {
		if s.Tunneled() {
			return folder{}, fmt.Errorf("DumpCat reaches this server through an agent or WireGuard, so it cannot open %s on the SQL Server host: share that folder with the DumpCat host and enter it under \"Backup folder seen from DumpCat\"", srv)
		}
		if windowsPath(srv) != (runtime.GOOS == "windows") {
			return folder{}, fmt.Errorf("SQL Server writes its backups to %s, a path of another kind of host than DumpCat's: share or mount that folder on the DumpCat host and enter it under \"Backup folder seen from DumpCat\"", srv)
		}
		local = srv
	}
	if !filepath.IsAbs(local) {
		return folder{}, fmt.Errorf("the backup folder seen from DumpCat must be an absolute path: %q", local)
	}
	return folder{Server: srv, Local: local}, nil
}

// fileName returns a name of DumpCat's own in the folder: random, so that
// tasks (and DumpCat installations sharing the folder) never meet.
func fileName(db, ext string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	safe := engine.SafeFileName(db)
	if len(safe) > 60 {
		safe = safe[:60]
	}
	return "dumpcat-" + hex.EncodeToString(b) + "-" + safe + ext
}

// openLocal opens a file SQL Server wrote: a regular file only.
func openLocal(p string) (*os.File, int64, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// writeLocal creates a new file in the folder for SQL Server to read.
func writeLocal(p string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = shareWithServer(p, filepath.Dir(p))
	}
	if err != nil {
		os.Remove(p)
	}
	return n, err
}

// removeLocal deletes a file of DumpCat's own from the folder.
func removeLocal(p string) {
	if fi, err := os.Lstat(p); err == nil && (fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0) {
		_ = os.Remove(p)
	}
}

// osError reads the operating system error SQL Server reports for a
// backup device ("Operating system error 5(Access is denied.)").
func osError(err error) int {
	s := errText(err)
	i := strings.Index(s, "Operating system error ")
	if i < 0 {
		return 0
	}
	n := 0
	for _, c := range s[i+len("Operating system error "):] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// deviceError explains why SQL Server could not open a file of the folder.
func deviceError(err error, f folder, path, account string) error {
	who := "SQL Server's service account"
	if account != "" {
		who += " (" + account + ")"
	}
	switch osError(err) {
	case 2, 3:
		if f.Local != f.Server {
			return fmt.Errorf("SQL Server cannot find %s: %s on the DumpCat host and %s on the SQL Server host are not the same folder (%s)", path, f.Local, f.Server, errText(err))
		}
		return fmt.Errorf("SQL Server cannot find %s: is DumpCat running on the SQL Server host? Otherwise share the folder and enter it under \"Backup folder seen from DumpCat\" (%s)", path, errText(err))
	case 5:
		return fmt.Errorf("%s may not use %s: give it Modify permission on that folder (Windows) or make the folder belong to the mssql user (Linux: chown mssql:mssql) (%s)", who, f.Server, errText(err))
	case 112:
		return fmt.Errorf("the disk of %s is full on the SQL Server host (%s)", f.Server, errText(err))
	}
	return errors.New(errText(err))
}

// probe checks that SQL Server reads what DumpCat writes into the folder:
// a small file is written through the local path and read by SQL Server
// through its own (RESTORE HEADERONLY: "not a backup" means it was read).
// unchecked says why SQL Server's side could not be checked ("": it was).
func probe(ctx context.Context, db *sql.DB, f folder, account string) (unchecked string, err error) {
	fi, err := os.Stat(f.Local)
	if err != nil {
		return "", fmt.Errorf("the backup folder %s cannot be opened on the DumpCat host: %v", f.Local, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("the backup folder %s is not a directory", f.Local)
	}
	name := fileName("probe", ".txt")
	if _, err := writeLocal(f.local(name), strings.NewReader("DumpCat checks that SQL Server reads this folder. This file is not a backup and can be deleted.\n")); err != nil {
		return "", fmt.Errorf("DumpCat cannot write into the backup folder %s: %v", f.Local, err)
	}
	defer removeLocal(f.local(name))
	_, err = db.ExecContext(ctx, "RESTORE HEADERONLY FROM DISK = "+QuoteString(f.server(name)))
	switch {
	case err == nil, hasError(err, 3241, 3254):
		return "", nil // read: not a backup ("incorrectly formed", "the volume is empty")
	case hasError(err, 3201):
		return "", deviceError(err, f, f.server(name), account)
	case hasError(err, 262, 229, 297, 300, 15247):
		return "the account may not run RESTORE HEADERONLY", nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return errText(err), nil
}
