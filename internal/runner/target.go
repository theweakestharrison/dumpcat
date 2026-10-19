package runner

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// TargetCheck describes how a backup fits the instance it is about to be
// restored to: a different server, version or flavor than the source.
type TargetCheck struct {
	OK         bool                `json:"ok"`
	Error      string              `json:"error,omitempty"`
	Version    string              `json:"version"`
	Flavor     string              `json:"flavor"`
	Hostname   string              `json:"hostname"`
	SameServer bool                `json:"same_server"`
	Existing   []string            `json:"existing"` // user databases already on the target
	Warnings   []string            `json:"warnings"`
	Suggest    engine.PortableOpts `json:"suggest"`
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
	tc := &TargetCheck{Existing: []string{}, Warnings: []string{}}
	tc.SameServer = r.sameServer(run, sv)
	info, err := dbconn.Inspect(ctx, sv.ForRestore(), false)
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
	warn, sug := compatNotes(run, tc.Flavor, tc.Version, tc.SameServer)
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

func isMariaDB(flavor, version string) bool {
	return flavor == "mariadb" || strings.Contains(strings.ToLower(version), "mariadb")
}

// compatNotes compares the source of a backup with the target instance and
// suggests the rewrites a logical restore needs to load there.
func compatNotes(run *store.Run, tgtFlavor, tgtVersion string, same bool) ([]string, engine.PortableOpts) {
	var warn []string
	var sug engine.PortableOpts
	srcVersion, _ := run.Meta["server_version"].(string)
	srcFlavor, _ := run.Meta["flavor"].(string)
	srcMaria, tgtMaria := isMariaDB(srcFlavor, srcVersion), isMariaDB(tgtFlavor, tgtVersion)
	sMaj, sMin := parseVersion(srcVersion)
	tMaj, tMin := parseVersion(tgtVersion)
	name := func(maria bool, v string) string {
		if maria {
			return "MariaDB " + v
		}
		return "MySQL " + v
	}
	src, tgt := name(srcMaria, srcVersion), name(tgtMaria, tgtVersion)
	known := srcVersion != "" && tgtVersion != ""

	if run.Method == store.MethodPhysical {
		switch {
		case !known:
		case srcMaria != tgtMaria:
			warn = append(warn, fmt.Sprintf("A physical backup of %s cannot be started by %s — use a logical backup to move between MySQL and MariaDB.", src, tgt))
		case sMaj != tMaj || sMin != tMin:
			warn = append(warn, fmt.Sprintf("The backup comes from %s but the target runs %s: a physical backup needs the same version series (the target's binaries will start the restored datadir).", src, tgt))
		}
		return warn, sug
	}

	if known {
		switch {
		case srcMaria != tgtMaria:
			warn = append(warn, fmt.Sprintf("Moving from %s to %s: tables, views and routines usually load once collations are converted, but engine- or flavor-specific features (JSON columns, sequences, system-versioned tables, auth plugins of restored users) may need manual fixes. Point-in-time recovery cannot replay binlogs across MySQL and MariaDB.", src, tgt))
			sug.ConvertCollations = true
		case sMaj > tMaj || (sMaj == tMaj && sMin > tMin):
			warn = append(warn, fmt.Sprintf("The target (%s) is older than the source (%s): syntax or features of the newer version may fail to load.", tgt, src))
			// utf8mb4_0900_* and utf8mb3_* collations are MySQL 8.0+
			if !srcMaria && sMaj >= 8 && tMaj < 8 {
				sug.ConvertCollations = true
			}
		}
	}
	if !same {
		// definer accounts of views/routines/triggers rarely exist on another instance
		sug.StripDefiner = true
	}
	if gtid, _ := run.Meta["gtid"].(string); gtid != "" && run.Method == store.MethodMysqldump && !srcMaria {
		warn = append(warn, "The dump sets GTID_PURGED: this fails on a server that already has a GTID history and needs SUPER. Skip it unless you are building a replica of the source.")
		sug.SkipGTIDPurged = true
	}
	return warn, sug
}

// inspectTarget connects to the restore target, logs what it is and how it
// compares with the backup, and returns its flavor.
func (r *Runner) inspectTarget(ctx context.Context, run *store.Run, target *store.Server, req RestoreRequest, lg *tasklog.Logger) (string, error) {
	info, err := dbconn.Inspect(ctx, target, false)
	if err != nil {
		return "", fmt.Errorf("could not connect to the target server: %w", err)
	}
	flavor := targetFlavorOf(target, info)
	same := r.sameServer(run, target)
	where := "another instance"
	if same {
		where = "the source server"
	}
	lg.Printf("Target: %s — %s %s on %s (%s)", target.Name, flavor, info.Version, info.Hostname, where)
	warn, _ := compatNotes(run, flavor, info.Version, same)
	for _, w := range warn {
		lg.Printf("NOTE: %s", w)
	}
	if o := req.portable(); o.Any() {
		lg.Printf("Portability: %s", strings.Join(o.Labels(), ", "))
	}
	return flavor, nil
}
