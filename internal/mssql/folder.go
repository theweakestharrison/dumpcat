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
	"time"

	"dumpcat/internal/engine"
	"dumpcat/internal/store"
	"dumpcat/internal/sysinfo"
)

// BACKUP writes on the SQL Server host and RESTORE reads there, so every
// backup file passes through a folder on that host: Server is its path as
// SQL Server names it, Files how DumpCat reaches it. That is DumpCat's own
// file system when DumpCat runs on the SQL Server host (the same path) or
// the folder is shared (\\nas\sqlbackup for both, a Windows share mounted
// on Linux, a volume of a container), or else the `dumpcat agent` on the
// SQL Server host, started with --allow-mssql-dir, which reads and writes
// the files there for DumpCat. SQL Server writes each .bak / .trn into the
// folder, DumpCat reads it into the usual pipeline (compression,
// encryption, SHA-256, every storage of the job) and deletes it; a restore
// does the opposite.
type folder struct {
	Server string
	Files  Files
}

// Files is the backup folder as DumpCat reaches it. Names are file names
// in the folder, always DumpCat's own (fileName).
type Files interface {
	// Open opens a file SQL Server wrote (a regular file) and returns its size.
	Open(ctx context.Context, name string) (io.ReadCloser, int64, error)
	// Create writes a new file for SQL Server to read; it must not exist.
	Create(ctx context.Context, name string, r io.Reader) (int64, error)
	// Remove deletes a file; one that is gone is no error.
	Remove(ctx context.Context, name string) error
	// Free checks the folder and returns its free bytes (-1: unknown).
	Free(ctx context.Context) (int64, error)
	// Dir is the folder's path where Files opens it.
	Dir() string
	// String names the folder in logs and messages.
	String() string
}

// AgentFolder reaches the backup folder of a server behind a `dumpcat
// agent` that gives DumpCat SQL Server's backup folder (set at startup);
// dir is the folder SQL Server writes to, for the hint when the agent does
// not.
var AgentFolder func(agentID int64, dir string) (Files, error)

// localFiles is the folder in DumpCat's own file system.
type localFiles string

func (d localFiles) path(name string) string { return filepath.Join(string(d), name) }
func (d localFiles) Dir() string             { return string(d) }
func (d localFiles) String() string          { return string(d) }

func (d localFiles) Open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	f, size, err := openLocal(d.path(name))
	if err != nil {
		return nil, 0, err
	}
	return f, size, nil
}

func (d localFiles) Create(_ context.Context, name string, r io.Reader) (int64, error) {
	return writeLocal(d.path(name), r)
}

func (d localFiles) Remove(_ context.Context, name string) error {
	removeLocal(d.path(name))
	return nil
}

func (d localFiles) Free(context.Context) (int64, error) {
	fi, err := os.Stat(string(d))
	if err != nil {
		return 0, fmt.Errorf("the backup folder %s cannot be opened on the DumpCat host: %v", d, err)
	}
	if !fi.IsDir() {
		return 0, fmt.Errorf("the backup folder %s is not a directory", d)
	}
	if _, free, err := sysinfo.DiskUsage(string(d)); err == nil {
		return free, nil
	}
	return -1, nil
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

// sameDir compares two paths of one host (trailing separators; Windows
// paths ignore case).
func sameDir(a, b string) bool {
	norm := func(p string) string {
		p = strings.TrimRight(p, `\/`)
		if windowsPath(p) {
			p = strings.ToLower(strings.ReplaceAll(p, "/", `\`))
		}
		return p
	}
	return norm(a) == norm(b)
}

// throughAgent tells whether the agent on the SQL Server host reaches the folder.
func (f folder) throughAgent() bool {
	_, local := f.Files.(localFiles)
	return !local
}

// same tells whether DumpCat (or its agent) opens the folder under the
// path SQL Server names it by.
func (f folder) same() bool { return sameDir(f.Files.Dir(), f.Server) }

// describe is the folder for the logs: "D:\Backup (DumpCat reads it as …)".
func (f folder) describe(verb string) string {
	switch {
	case f.throughAgent():
		return f.Server + " — DumpCat " + verb + " it through " + f.Files.String()
	case f.same():
		return f.Server
	}
	return f.Server + " (DumpCat " + verb + " it as " + f.Files.String() + ")"
}

// via says how DumpCat reaches the folder when it does not open it under
// SQL Server's path ("": it does).
func (f folder) via() string {
	switch {
	case f.throughAgent():
		return "through " + f.Files.String()
	case f.same():
		return ""
	}
	return "DumpCat: " + f.Files.String()
}

// where names the folder as DumpCat reaches it, for messages.
func (f folder) where() string {
	if f.throughAgent() {
		return "the backup folder through " + f.Files.String()
	}
	return "the backup folder " + f.Files.String()
}

// remove deletes a file of DumpCat's own from the folder, also when the
// task was canceled; a file that cannot be deleted stays (it is named
// dumpcat-…).
func (f folder) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = f.Files.Remove(ctx, name)
}

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
		switch {
		case s.AgentID != nil && *s.AgentID > 0:
			// the agent on the SQL Server host reads and writes the folder
			if AgentFolder == nil {
				return folder{}, errors.New("agent connections are not available")
			}
			fs, err := AgentFolder(*s.AgentID, srv)
			if err != nil {
				return folder{}, fmt.Errorf("%v — or share %s with the DumpCat host and enter it under \"Backup folder seen from DumpCat\"", err, srv)
			}
			return folder{Server: srv, Files: fs}, nil
		case s.Tunneled():
			return folder{}, fmt.Errorf("DumpCat reaches this server through WireGuard, so it cannot open %s on the SQL Server host: share that folder with the DumpCat host and enter it under \"Backup folder seen from DumpCat\", or connect through an agent started with --allow-mssql-dir", srv)
		case windowsPath(srv) != (runtime.GOOS == "windows"):
			return folder{}, fmt.Errorf("SQL Server writes its backups to %s, a path of another kind of host than DumpCat's: share or mount that folder on the DumpCat host and enter it under \"Backup folder seen from DumpCat\"", srv)
		}
		local = srv
	}
	if !filepath.IsAbs(local) {
		return folder{}, fmt.Errorf("the backup folder seen from DumpCat must be an absolute path: %q", local)
	}
	return folder{Server: srv, Files: localFiles(local)}, nil
}

// fileName returns a name of DumpCat's own in the folder: random, so that
// tasks (and DumpCat installations sharing the folder) never meet. Agents
// only give access to names like these (dumpcat-…, no path).
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
		switch {
		case f.throughAgent() && !f.same():
			return fmt.Errorf("SQL Server cannot find %s: the folder of %s and %s on the SQL Server host are not the same folder (%s)", path, f.Files, f.Server, errText(err))
		case !f.same():
			return fmt.Errorf("SQL Server cannot find %s: %s and %s on the SQL Server host are not the same folder (%s)", path, f.Files, f.Server, errText(err))
		case f.throughAgent():
			return fmt.Errorf("SQL Server cannot find %s, which %s wrote: does the agent run on the SQL Server host? (SQL Server in a container: start the agent with the host folder the container's backup folder is mounted from, and enter the container's path as the backup folder) (%s)", path, f.Files, errText(err))
		}
		return fmt.Errorf("SQL Server cannot find %s: is DumpCat running on the SQL Server host? Otherwise share the folder and enter it under \"Backup folder seen from DumpCat\", or connect through an agent started with --allow-mssql-dir (%s)", path, errText(err))
	case 5:
		return fmt.Errorf("%s may not use %s: give it Modify permission on that folder (Windows) or make the folder belong to the mssql user (Linux: chown mssql:mssql) (%s)", who, f.Server, errText(err))
	case 112:
		return fmt.Errorf("the disk of %s is full on the SQL Server host (%s)", f.Server, errText(err))
	}
	return errors.New(errText(err))
}

// probe checks that SQL Server reads what DumpCat writes into the folder:
// a small file is written through Files and read by SQL Server through its
// own path (RESTORE HEADERONLY: "not a backup" means it was read).
// unchecked says why SQL Server's side could not be checked ("": it was).
func probe(ctx context.Context, db *sql.DB, f folder, account string) (unchecked string, err error) {
	if _, err := f.Files.Free(ctx); err != nil {
		return "", err
	}
	name := fileName("probe", ".txt")
	if _, err := f.Files.Create(ctx, name, strings.NewReader("DumpCat checks that SQL Server reads this folder. This file is not a backup and can be deleted.\n")); err != nil {
		return "", fmt.Errorf("DumpCat cannot write into %s: %v", f.where(), err)
	}
	defer f.remove(name)
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
