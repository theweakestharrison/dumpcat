package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// PostgreSQL client tools come in one directory per major version
// (/usr/lib/postgresql/16/bin, C:\Program Files\PostgreSQL\16\bin): DumpCat
// finds all of them and picks, for a server, the same major or the nearest
// newer one (pg_dump refuses newer servers; pg_restore reads archives of its
// own and older versions).

var pgToolNames = []string{"pg_dump", "pg_restore", "pg_dumpall"}

func isPGTool(name string) bool {
	for _, n := range pgToolNames {
		if n == name {
			return true
		}
	}
	return false
}

const pgLinuxHint = "Ubuntu/Debian: apt install postgresql-client-<major> (the major of your server, from apt.postgresql.org) | RHEL: dnf install postgresql<major> (PGDG repository)"
const pgWindowsHint = `install PostgreSQL for Windows or its command line tools (C:\Program Files\PostgreSQL\<major>\bin is found automatically), or set the path in Settings`

var pgPurpose = map[string]string{
	"pg_dump":    "Logical backup (PostgreSQL)",
	"pg_restore": "Restore of pg_dump archives (PostgreSQL)",
	"pg_dumpall": "Roles and tablespaces (PostgreSQL)",
}

// PGTool is one installed PostgreSQL client binary.
type PGTool struct {
	Path    string
	Version string // "pg_dump (PostgreSQL) 16.15"
	Major   int
}

var rePGVersion = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)(?:\.(\d+))?`)

// PGMajor reads the major version from "pg_dump (PostgreSQL) 16.15" (9.6 → 9).
func PGMajor(version string) int {
	m := rePGVersion.FindStringSubmatch(version)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func pgDirs() []string {
	var dirs []string
	if d := os.Getenv("DUMPCAT_TOOLS_DIR"); d != "" {
		dirs = append(dirs, filepath.SplitList(d)...)
	}
	var patterns []string
	if runtime.GOOS == "windows" {
		if exe, err := os.Executable(); err == nil {
			d := filepath.Dir(exe)
			dirs = append(dirs, filepath.Join(d, "tools"), d)
		}
		for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles(x86)")} {
			if pf != "" {
				patterns = append(patterns, filepath.Join(pf, "PostgreSQL", "*", "bin"))
			}
		}
	} else {
		patterns = append(patterns, "/usr/lib/postgresql/*/bin", "/usr/pgsql-*/bin", "/opt/homebrew/opt/postgresql@*/bin", "/usr/local/opt/postgresql@*/bin")
		dirs = append(dirs, "/usr/local/pgsql/bin", "/usr/local/bin", "/usr/bin")
	}
	for _, p := range patterns {
		m, _ := filepath.Glob(p)
		dirs = append(dirs, m...)
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		k := d
		if runtime.GOOS == "windows" {
			k = strings.ToLower(d)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, d)
		}
	}
	return out
}

// PGTools lists every installed copy of a PostgreSQL tool, newest major
// first. A path set in Settings replaces the search.
func (t *Tools) PGTools(name string) []PGTool {
	t.mu.Lock()
	if l, ok := t.pg[name]; ok {
		t.mu.Unlock()
		return l
	}
	t.mu.Unlock()
	var paths []string
	if t.overrides != nil {
		if p := strings.TrimSpace(t.overrides()[name]); p != "" {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) == 0 {
		exe := name
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		for _, d := range pgDirs() {
			p := filepath.Join(d, exe)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				paths = append(paths, p)
			}
		}
		if p, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
			paths = append(paths, p)
		}
	}
	var list []PGTool
	seen := map[string]bool{}
	for _, p := range paths {
		// /usr/bin/pg_dump is often a link to (or a wrapper around) a versioned one
		key := p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			key = r
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out := strings.TrimSpace(runQuick(p, "--version"))
		line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		line = strings.Replace(line, p, filepath.Base(p), 1)
		if maj := PGMajor(line); maj > 0 {
			list = append(list, PGTool{Path: p, Version: line, Major: maj})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Major > list[j].Major })
	t.mu.Lock()
	t.pg[name] = list
	t.mu.Unlock()
	return list
}

// PGFor picks the tool for a server of major version want: the same major,
// else the nearest newer one. want 0 (unknown) takes the newest. The error
// says what to install.
func (t *Tools) PGFor(name string, want int) (PGTool, error) {
	list := t.PGTools(name)
	if len(list) == 0 {
		return PGTool{}, fmt.Errorf("%w %s on the server (install: %s)", ErrToolMissing, name, pgHint(want))
	}
	if want <= 0 {
		return list[0], nil
	}
	best := -1
	for i, tl := range list {
		if tl.Major >= want && (best < 0 || tl.Major < list[best].Major) {
			best = i
		}
	}
	if best < 0 {
		return PGTool{}, fmt.Errorf("%s %d is older than PostgreSQL %d: install the client tools of version %d or newer (%s)", name, list[0].Major, want, want, pgHint(want))
	}
	return list[best], nil
}

func pgHint(major int) string {
	h := pgLinuxHint
	if runtime.GOOS == "windows" {
		h = pgWindowsHint
	}
	if major > 0 {
		h = strings.ReplaceAll(h, "<major>", strconv.Itoa(major))
	}
	return h
}

// pgTool summarizes the copies of a PostgreSQL tool for the System page.
func (t *Tools) pgTool(name string) *Tool {
	tl := &Tool{Name: name, Purpose: pgPurpose[name], Hint: pgHint(0)}
	list := t.PGTools(name)
	if len(list) == 0 {
		return tl
	}
	tl.Found, tl.Path, tl.Version = true, list[0].Path, list[0].Version
	if len(list) > 1 {
		var others []string
		for _, o := range list[1:] {
			others = append(others, strconv.Itoa(o.Major))
		}
		tl.Version += " (also " + strings.Join(others, ", ") + ")"
	}
	return tl
}
