// Package engine wraps the external backup/restore tools:
// mysqldump/mariadb-dump + mysql client (logical), mydumper/myloader
// (parallel logical) and Percona XtraBackup / MariaBackup (physical, hot),
// and defines the Driver interface every database engine implements
// (driver.go; MySQL in mysql*.go, PostgreSQL in internal/pgsql).
package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Found   bool   `json:"found"`
	MariaDB bool   `json:"mariadb"`
	Purpose string `json:"purpose"`
	Hint    string `json:"hint"`
	help    string
	helpOK  bool
}

type toolDef struct {
	candidates []string
	purpose    string
	hint       string
	versionArg string
}

var toolDefs = map[string]toolDef{
	"mysqldump":      {[]string{"mysqldump", "mariadb-dump"}, "Logical backup (MySQL)", "Ubuntu/Debian: apt install mysql-client | RHEL: dnf install mysql", "--version"},
	"mariadb-dump":   {[]string{"mariadb-dump", "mysqldump"}, "Logical backup (MariaDB)", "apt install mariadb-client | dnf install MariaDB-client", "--version"},
	"mysql":          {[]string{"mysql", "mariadb"}, "Logical restore (MySQL client)", "apt install mysql-client", "--version"},
	"mariadb":        {[]string{"mariadb", "mysql"}, "Logical restore (MariaDB client)", "apt install mariadb-client", "--version"},
	"mydumper":       {[]string{"mydumper"}, "Parallel logical backup (large databases)", "apt install mydumper or download the .deb/.rpm from github.com/mydumper/mydumper/releases", "--version"},
	"myloader":       {[]string{"myloader"}, "Parallel restore for mydumper", "ships with the mydumper package", "--version"},
	"xtrabackup":     {[]string{"xtrabackup"}, "Hot physical backup (MySQL/Percona)", "Percona repo: percona-release enable pxb-80 (or pxb-84-lts) && apt install percona-xtrabackup-80", "--version"},
	"xbstream":       {[]string{"xbstream"}, "Extracts xtrabackup streams", "ships with the percona-xtrabackup package", "--version"},
	"mariabackup":    {[]string{"mariadb-backup", "mariabackup"}, "Hot physical backup (MariaDB)", "apt install mariadb-backup | dnf install MariaDB-backup", "--version"},
	"mbstream":       {[]string{"mbstream"}, "Extracts mariabackup streams", "ships with the mariadb-backup package", "--version"},
	"mysqlbinlog":    {[]string{"mysqlbinlog", "mariadb-binlog"}, "Continuous binlog archiving & PITR (MySQL)", "ships with the mysql-client / mysql-server package", "--version"},
	"mariadb-binlog": {[]string{"mariadb-binlog", "mysqlbinlog"}, "Continuous binlog archiving & PITR (MariaDB)", "apt install mariadb-client | dnf install MariaDB-client", "--version"},
	"rclone":         {[]string{"rclone"}, "Uploads to S3/Google Drive/OneDrive/FTP/SFTP...", "curl https://rclone.org/install.sh | sudo bash", "version"},
}

// windowsHints replace the Linux package hints when DumpCat runs on Windows.
var windowsHints = map[string]string{
	"mydumper":   "mydumper has no Windows version: use mysqldump/mariadb-dump, or run DumpCat on Linux",
	"myloader":   "myloader has no Windows version (ships with mydumper)",
	"xtrabackup": "Percona XtraBackup has no Windows version: physical MySQL/Percona backups need DumpCat on Linux",
	"xbstream":   "xbstream has no Windows version (ships with Percona XtraBackup)",
	"rclone":     `the Windows installer puts rclone.exe in the "tools" folder next to dumpcat.exe; or download it from rclone.org/downloads`,
}

const windowsToolHint = `install MySQL or MariaDB for Windows (their bin folder is found automatically), or copy the .exe into the "tools" folder next to dumpcat.exe`

var toolOrder = []string{"mysqldump", "mariadb-dump", "mysql", "mariadb", "mydumper", "myloader", "xtrabackup", "xbstream", "mariabackup", "mbstream", "mysqlbinlog", "mariadb-binlog", "pg_dump", "pg_restore", "pg_dumpall", "rclone"}

type Tools struct {
	mu        sync.Mutex
	overrides func() map[string]string
	cache     map[string]*Tool
	pg        map[string][]PGTool // PostgreSQL tools, every major version found
}

func NewTools(overrides func() map[string]string) *Tools {
	return &Tools{overrides: overrides, cache: map[string]*Tool{}, pg: map[string][]PGTool{}}
}

func (t *Tools) Refresh() {
	t.mu.Lock()
	t.cache = map[string]*Tool{}
	t.pg = map[string][]PGTool{}
	t.mu.Unlock()
}

func searchDirs() []string {
	dirs := []string{}
	if d := os.Getenv("DUMPCAT_TOOLS_DIR"); d != "" {
		dirs = append(dirs, filepath.SplitList(d)...)
	}
	if runtime.GOOS == "windows" {
		dirs = append(dirs, windowsToolDirs()...)
	} else {
		dirs = append(dirs, "/usr/local/bin", "/usr/bin", "/bin", "/usr/local/mysql/bin", "/opt/mysql/bin", "/usr/sbin")
	}
	return dirs
}

// windowsToolDirs lists the "tools" folder next to dumpcat.exe and the bin
// folders of common MySQL/MariaDB installs (newest version first). MySQL comes
// before MariaDB so "mysqldump" resolves to Oracle's tool when both exist.
func windowsToolDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		dirs = append(dirs, filepath.Join(d, "tools"), d)
	}
	var patterns []string
	for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles(x86)")} {
		if pf != "" {
			patterns = append(patterns, filepath.Join(pf, "MySQL", "MySQL Server *", "bin"), filepath.Join(pf, "Percona Server *", "bin"))
		}
	}
	for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles(x86)")} {
		if pf != "" {
			patterns = append(patterns, filepath.Join(pf, "MariaDB *", "bin"))
		}
	}
	patterns = append(patterns, `C:\xampp\mysql\bin`, `C:\laragon\bin\mysql\*\bin`, `C:\wamp64\bin\mysql\*\bin`, `C:\wamp64\bin\mariadb\*\bin`)
	seen := map[string]bool{}
	for _, d := range dirs {
		seen[strings.ToLower(d)] = true
	}
	for _, p := range patterns {
		m, _ := filepath.Glob(p)
		sort.Sort(sort.Reverse(sort.StringSlice(m)))
		for _, d := range m {
			if k := strings.ToLower(d); !seen[k] {
				seen[k] = true
				dirs = append(dirs, d)
			}
		}
	}
	return dirs
}

func lookup(name string) string {
	for _, d := range searchDirs() {
		p := filepath.Join(d, name)
		if runtime.GOOS == "windows" {
			p += ".exe"
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return ""
}

func runQuick(bin string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out)
}

// Get resolves a logical tool name to a binary (cached).
func (t *Tools) Get(name string) *Tool {
	t.mu.Lock()
	if tl, ok := t.cache[name]; ok {
		t.mu.Unlock()
		return tl
	}
	t.mu.Unlock()
	if isPGTool(name) {
		tl := t.pgTool(name)
		t.mu.Lock()
		t.cache[name] = tl
		t.mu.Unlock()
		return tl
	}
	def, ok := toolDefs[name]
	tl := &Tool{Name: name}
	if !ok {
		return tl
	}
	tl.Purpose, tl.Hint = def.purpose, def.hint
	if runtime.GOOS == "windows" {
		tl.Hint = windowsToolHint
		if h, ok := windowsHints[name]; ok {
			tl.Hint = h
		}
	}
	path := ""
	if t.overrides != nil {
		if p := strings.TrimSpace(t.overrides()[name]); p != "" {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				path = p
			}
		}
	}
	if path == "" {
		for _, c := range def.candidates {
			if p := lookup(c); p != "" {
				path = p
				break
			}
		}
	}
	if path != "" {
		tl.Path = path
		tl.Found = true
		out := runQuick(path, def.versionArg)
		line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
		// "/usr/bin/mariadb-dump from 11.4..." -> "mariadb-dump from 11.4..."
		line = strings.Replace(line, path, filepath.Base(path), 1)
		tl.Version = line
		tl.MariaDB = strings.Contains(strings.ToLower(out), "mariadb")
	}
	t.mu.Lock()
	t.cache[name] = tl
	t.mu.Unlock()
	return tl
}

// Help returns (cached) --help output used for feature detection.
func (t *Tools) Help(name string) string {
	tl := t.Get(name)
	if !tl.Found {
		return ""
	}
	t.mu.Lock()
	if tl.helpOK {
		h := tl.help
		t.mu.Unlock()
		return h
	}
	t.mu.Unlock()
	h := runQuick(tl.Path, "--help")
	t.mu.Lock()
	tl.help, tl.helpOK = h, true
	t.mu.Unlock()
	return h
}

// HasFlag reports whether a tool's --help mentions the long option.
func (t *Tools) HasFlag(name, flag string) bool {
	return strings.Contains(t.Help(name), "--"+strings.TrimPrefix(flag, "--"))
}

func (t *Tools) All() []*Tool {
	out := make([]*Tool, 0, len(toolOrder))
	for _, n := range toolOrder {
		out = append(out, t.Get(n))
	}
	return out
}

// DumpTool returns the dump binary matching the server flavor.
func (t *Tools) DumpTool(flavor string) *Tool {
	if flavor == "mariadb" {
		return t.Get("mariadb-dump")
	}
	return t.Get("mysqldump")
}

func DumpToolName(flavor string) string {
	if flavor == "mariadb" {
		return "mariadb-dump"
	}
	return "mysqldump"
}

func ClientToolName(flavor string) string {
	if flavor == "mariadb" {
		return "mariadb"
	}
	return "mysql"
}

// BinlogToolName returns the binlog reader matching the server flavor that
// wrote the logs (MySQL's mysqlbinlog cannot read MariaDB GTID events).
func BinlogToolName(flavor string) string {
	if flavor == "mariadb" {
		return "mariadb-binlog"
	}
	return "mysqlbinlog"
}

func PhysicalToolNames(flavor string) (backup, stream string) {
	if flavor == "mariadb" {
		return "mariabackup", "mbstream"
	}
	return "xtrabackup", "xbstream"
}
