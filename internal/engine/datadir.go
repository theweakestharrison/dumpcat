package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dumpcat/internal/execx"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// PhysicalRestorer prepares a physical backup (a copy of the data
// directory) on the DumpCat host and can replace a local data directory.
// Optional: MySQL's xtrabackup/mariabackup restore is still the runner's.
type PhysicalRestorer interface {
	RestorePhysical(env *Env, src RestoreSource, spec *PhysicalSpec) error
}

// PhysicalSpec is one physical restore.
type PhysicalSpec struct {
	Run *store.Run
	// Dir is where the data directory is prepared (missing or empty) when
	// it is not applied.
	Dir string
	// Replay is a checked point-in-time recovery (nil: the backup itself).
	Replay *Replay
	// Apply replaces a data directory of this machine (nil: prepare only).
	Apply *ApplySpec
	// OSUser owns the prepared directory and runs the recovery when DumpCat
	// runs as root (Linux).
	OSUser string
}

// ApplySpec is a local data directory to replace (Linux, admins).
type ApplySpec struct {
	Datadir string
	Service string // systemd unit ("" = detect)
	DataDir string // DumpCat's own data directory, never touched
}

var (
	unitNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,127}$`)
	osUserRe   = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
)

// System directories a restore never replaces: these, and everything under them...
var systemTrees = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/proc", "/run", "/sbin", "/sys", "/usr"}

// ...and these themselves (a datadir may live under them).
var systemDirs = []string{"/home", "/media", "/mnt", "/opt", "/root", "/srv", "/tmp", "/var", "/var/lib", "/var/log", "/var/tmp"}

// CheckDatadir refuses a data directory, service or OS user that would make
// a restore, which runs as root and renames, replaces and chowns the
// directory, act on anything but a database's data directory. The values
// may come from a server record that operators can edit. A non-empty
// directory must hold one of markers; product names the database in the
// message.
func CheckDatadir(datadir, service, osUser, dumpcatDir string, markers []string, product string) error {
	if !unitNameRe.MatchString(service) {
		return fmt.Errorf("invalid service name %q", service)
	}
	if !osUserRe.MatchString(osUser) {
		return fmt.Errorf("invalid OS user %q", osUser)
	}
	clean := filepath.Clean(datadir)
	if clean != datadir || !filepath.IsAbs(clean) || strings.ContainsAny(clean, "\x00\r\n") {
		return fmt.Errorf("invalid datadir %q: use a clean absolute path", datadir)
	}
	slash := filepath.ToSlash(clean)
	for _, d := range systemTrees {
		if slash == d || strings.HasPrefix(slash, d+"/") {
			return fmt.Errorf("refusing to replace %s: it is a system directory", clean)
		}
	}
	for _, d := range systemDirs {
		if slash == d {
			return fmt.Errorf("refusing to replace %s: it is a system directory", clean)
		}
	}
	if own, err := filepath.Abs(dumpcatDir); err == nil {
		if rel, err := filepath.Rel(own, clean); err == nil && !strings.HasPrefix(rel, "..") {
			return fmt.Errorf("refusing to replace %s: it is inside DumpCat's data directory", clean)
		}
		if rel, err := filepath.Rel(clean, own); err == nil && !strings.HasPrefix(rel, "..") {
			return fmt.Errorf("refusing to replace %s: it contains DumpCat's data directory", clean)
		}
	}
	entries, err := os.ReadDir(clean)
	if err != nil || len(entries) == 0 {
		return nil // missing or empty: nothing is replaced
	}
	for _, e := range entries {
		for _, m := range markers {
			if e.Name() == m {
				return nil
			}
		}
	}
	return fmt.Errorf("refusing to replace %s: it does not look like a %s data directory (none of %s)", clean, product, strings.Join(markers, ", "))
}

// RunSimple runs a command with its output in the log.
func RunSimple(ctx context.Context, lg *tasklog.Logger, name string, args ...string) error {
	lg.Printf("$ %s", execx.Quote(name, args))
	cmd := execx.Command(ctx, name, args...)
	w := lg.Writer("[" + filepath.Base(name) + "] ")
	defer w.Close()
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}
