package runner

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/drivers"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// TargetCheck describes how a backup fits the instance it is about to be
// restored to: a different server, version or flavor than the source.
type TargetCheck struct {
	OK         bool               `json:"ok"`
	Error      string             `json:"error,omitempty"`
	Engine     string             `json:"engine"`
	Version    string             `json:"version"`
	Flavor     string             `json:"flavor"`
	Hostname   string             `json:"hostname"`
	SameServer bool               `json:"same_server"`
	Existing   []string           `json:"existing"` // user databases already on the target
	Warnings   []string           `json:"warnings"`
	Suggest    engine.RestoreOpts `json:"suggest"`
}

// CheckRestoreTarget connects to a server with its restore account and
// compares it with the backup. Connection errors are reported in the result.
func (r *Runner) CheckRestoreTarget(ctx context.Context, runID, serverID int64) (*TargetCheck, error) {
	run, err := r.Store.GetRun(runID)
	if err != nil {
		return nil, err
	}
	sv, err := r.Store.GetServer(serverID)
	if err != nil {
		return nil, fmt.Errorf("target server: %w", err)
	}
	tc := &TargetCheck{Existing: []string{}, Warnings: []string{}, Engine: sv.EngineName()}
	drv := drivers.ForMethod(run.Method)
	if drv == nil || drivers.ForServer(sv) != drv {
		if drv != nil {
			tc.Error = fmt.Sprintf("a %s backup restores onto a %s server", drv.Product(), drv.Product())
		} else {
			tc.Error = fmt.Sprintf("unknown backup method %q", run.Method)
		}
		return tc, nil
	}
	tc.SameServer = r.sameServer(run, sv)
	info, err := drv.Inspect(ctx, sv.ForRestore(), false)
	if err != nil {
		tc.Error = "could not connect with the restore account: " + err.Error()
		return tc, nil
	}
	tc.OK = true
	tc.Version, tc.Hostname = info.Version, info.Hostname
	tc.Flavor = targetFlavorOf(sv, info)
	for _, d := range info.Databases {
		if !d.System {
			tc.Existing = append(tc.Existing, d.Name)
		}
	}
	warn, sug := drv.Compat(run, sv, info, tc.SameServer)
	tc.Warnings, tc.Suggest = append(tc.Warnings, warn...), sug
	return tc, nil
}

func targetFlavorOf(s *store.Server, info *dbconn.ServerInfo) string {
	if s.Flavor != "" && s.Flavor != "auto" {
		return s.Flavor
	}
	return info.Flavor
}

// sameServer tells whether sv is the server the backup was taken from: the
// same entry, or another entry with the same connection endpoint.
func (r *Runner) sameServer(run *store.Run, sv *store.Server) bool {
	if run.ServerID != nil && *run.ServerID == sv.ID {
		return true
	}
	if run.ServerID == nil {
		return false
	}
	src, err := r.Store.GetServer(*run.ServerID)
	if err != nil {
		return false
	}
	// addresses only compare when both are seen from the same place
	if src.Route() != sv.Route() {
		return false
	}
	if src.Socket != "" && sv.Socket == src.Socket && isLocal(src.Host) && isLocal(sv.Host) {
		return true
	}
	return src.Host != "" && strings.EqualFold(src.Host, sv.Host) && src.Port == sv.Port
}

func isLocal(h string) bool { return h == "" || h == "localhost" }

var reVersion = regexp.MustCompile(`^(\d+)\.(\d+)`)

func parseVersion(v string) (major, minor int) {
	m := reVersion.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, 0
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return
}

// compatNotes is the MySQL driver's comparison of a backup with a target.
func compatNotes(run *store.Run, tgtFlavor, tgtVersion string, same bool) ([]string, engine.RestoreOpts) {
	return engine.MySQLCompat(run, tgtFlavor, tgtVersion, same)
}

// inspectTarget connects to the restore target, logs what it is and how it
// compares with the backup.
func (r *Runner) inspectTarget(ctx context.Context, drv engine.Driver, run *store.Run, target *store.Server, req RestoreRequest, lg *tasklog.Logger) (*dbconn.ServerInfo, error) {
	info, err := drv.Inspect(ctx, target, false)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the target server: %w", err)
	}
	flavor := targetFlavorOf(target, info)
	same := r.sameServer(run, target)
	where := "another instance"
	if same {
		where = "the source server"
	}
	lg.Printf("Target: %s — %s %s on %s (%s)", target.Name, flavor, info.Version, info.Hostname, where)
	warn, _ := drv.Compat(run, target, info, same)
	for _, w := range warn {
		lg.Printf("NOTE: %s", w)
	}
	if o := req.opts(); o.Any() {
		lg.Printf("Portability: %s", strings.Join(o.Labels(), ", "))
	}
	return info, nil
}
