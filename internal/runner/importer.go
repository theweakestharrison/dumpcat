package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

type ImportResult struct {
	Found    int      `json:"found"`
	Imported int      `json:"imported"`
	Linked   int      `json:"linked"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

// ImportFromStorage scans a storage for manifest.json files and rebuilds the
// backup catalog (disaster recovery: new install pointing at old storage).
func (r *Runner) ImportFromStorage(ctx context.Context, storageID int64) (*ImportResult, error) {
	st, err := r.Store.GetStorage(storageID)
	if err != nil {
		return nil, err
	}
	b, err := r.Storage.Get(st)
	if err != nil {
		return nil, err
	}
	entries, err := b.FindFiles(ctx, "", "manifest.json", 3)
	if err != nil {
		return nil, err
	}
	jobs, _ := r.Store.ListJobs()
	jobByUUID := map[string]*store.Job{}
	for i := range jobs {
		jobByUUID[jobs[i].UUID] = &jobs[i]
	}
	res := &ImportResult{}
	var created []*store.Run
	for _, e := range entries {
		res.Found++
		data, err := b.ReadFile(ctx, e.Path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			continue
		}
		var m store.Manifest
		if err := json.Unmarshal(data, &m); err != nil || (m.Format != store.ManifestFormat && m.Format != store.LegacyManifestFormat) || m.UUID == "" {
			res.Skipped++
			continue
		}
		// layout is always <job>/<backup>/manifest.json; anything else (e.g. a
		// manifest at the storage root) could make deletion target the root
		dir, derr := storage.CleanRel(path.Dir(strings.TrimPrefix(e.Path, "/")))
		if derr != nil || strings.Count(dir, "/") != 1 {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: invalid manifest location", e.Path))
			continue
		}
		// older manifests may lack the names: they are the two levels of the storage path
		if seg := strings.SplitN(dir, "/", 2); len(seg) == 2 {
			if m.JobSlug == "" {
				m.JobSlug = seg[0]
			}
			if m.DirName == "" {
				m.DirName = seg[1]
			}
		}
		if err := validateManifest(&m); err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			continue
		}
		if existing, err := r.Store.GetRunByUUID(m.UUID); err == nil {
			if r.Store.LocationExists(existing.ID, st.ID) {
				res.Skipped++
				continue
			}
			// a copy of a known backup must hold exactly its files: a manifest that only borrows the
			// UUID must not attach other data to it (or bring a deleted backup back)
			if !sameFiles(existing.Files, m.Files) {
				res.Skipped++
				res.Errors = append(res.Errors, fmt.Sprintf("%s: same backup id as #%d but different files", e.Path, existing.ID))
				continue
			}
			if existing.Status == store.StatusFailed || existing.Status == store.StatusCanceled ||
				existing.Status == store.StatusRunning || existing.Status == store.StatusQueued {
				res.Skipped++ // never revive a backup that did not complete
				continue
			}
			sid := st.ID
			loc := &store.RunLocation{RunID: existing.ID, StorageID: &sid, StorageName: st.Name, StorageType: st.Type, Path: dir, Status: store.LocOK, SizeBytes: m.SizeBytes}
			_ = r.Store.AddLocation(loc)
			if existing.Status == store.StatusDeleted {
				existing.Status = store.StatusSuccess
				_ = r.Store.UpdateRun(existing)
			}
			res.Linked++
			continue
		}
		fin := m.FinishedAt
		run := &store.Run{
			UUID: m.UUID, JobName: m.JobName, JobUUID: m.JobUUID, JobSlug: m.JobSlug, ServerName: m.Server.Name, Method: m.Method, Type: m.Type,
			ParentUUID: m.ParentUUID, BaseUUID: m.BaseUUID, Status: store.StatusSuccess, Trigger: "import", TriggeredBy: "import",
			StartedAt: m.StartedAt, FinishedAt: &fin, SizeBytes: m.SizeBytes, RawBytes: m.RawBytes, Files: m.Files, Databases: m.Databases,
			Meta: m.Meta, Compression: m.Compression, Encrypted: m.Encryption != "", DirName: m.DirName,
		}
		if run.Meta == nil {
			run.Meta = map[string]any{}
		}
		if run.Meta["server_version"] == nil {
			run.Meta["server_version"] = m.Server.Version
		}
		if run.Meta["flavor"] == nil {
			run.Meta["flavor"] = m.Server.Flavor
		}
		if j, ok := jobByUUID[m.JobUUID]; ok {
			jid := j.ID
			run.JobID = &jid
			if run.Encrypted && j.Passphrase != "" {
				run.Passphrase = j.Passphrase
			}
		}
		// a date in the future would count as the newest backup and silence freshness alerts
		if now := time.Now(); run.StartedAt.IsZero() || run.StartedAt.After(now) {
			run.StartedAt = now
		}
		if run.FinishedAt != nil && run.FinishedAt.After(time.Now()) {
			fin := time.Now()
			run.FinishedAt = &fin
		}
		if err := r.Store.CreateRun(run); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", e.Path, err))
			continue
		}
		if m.Tables != nil {
			_ = r.Store.SetRunTables(run.ID, m.Tables)
		}
		sid := st.ID
		loc := &store.RunLocation{RunID: run.ID, StorageID: &sid, StorageName: st.Name, StorageType: st.Type, Path: dir, Status: store.LocOK, SizeBytes: m.SizeBytes}
		_ = r.Store.AddLocation(loc)
		created = append(created, run)
		res.Imported++
	}
	// resolve parent/base links of imported incrementals
	for _, run := range created {
		changed := false
		if run.ParentUUID != "" {
			if p, err := r.Store.GetRunByUUID(run.ParentUUID); err == nil {
				id := p.ID
				run.ParentID = &id
				changed = true
			}
		}
		if run.BaseUUID != "" {
			if p, err := r.Store.GetRunByUUID(run.BaseUUID); err == nil {
				id := p.ID
				run.BaseID = &id
				changed = true
			}
		}
		if changed {
			_ = r.Store.UpdateRun(run)
		}
	}
	return res, nil
}

var hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validateManifest rejects manifests that could not have been written by us.
// safeSegment: a job or backup directory name from a manifest, which ends up in local paths
// (the extraction directory of a physical restore) and in storage paths.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// charsetRe: character set and collation names, written unquoted into CREATE DATABASE.
var charsetRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func validateManifest(m *store.Manifest) error {
	// the storage that holds a manifest may be writable by someone who cannot touch this host:
	// nothing in it may name a path outside the backup's own directory
	if !safeSegment.MatchString(m.DirName) || strings.Contains(m.DirName, "..") {
		return fmt.Errorf("invalid backup directory name %q", m.DirName)
	}
	if m.JobSlug != "" && (!safeSegment.MatchString(m.JobSlug) || strings.Contains(m.JobSlug, "..")) {
		return fmt.Errorf("invalid job directory name %q", m.JobSlug)
	}
	for _, d := range m.Databases {
		if (d.Charset != "" && !charsetRe.MatchString(d.Charset)) || (d.Collation != "" && !charsetRe.MatchString(d.Collation)) {
			return fmt.Errorf("invalid character set or collation for database %q", d.Name)
		}
	}
	switch m.Method {
	case store.MethodMysqldump, store.MethodMydumper, store.MethodPhysical:
	default:
		return fmt.Errorf("invalid method %q", m.Method)
	}
	if m.Type != store.TypeFull && m.Type != store.TypeIncremental {
		return fmt.Errorf("invalid type %q", m.Type)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("manifest lists no files")
	}
	for _, f := range m.Files {
		if f.Name == "" || f.Name == "manifest.json" || strings.ContainsAny(f.Name, "/\\") || strings.Contains(f.Name, "..") {
			return fmt.Errorf("invalid file name %q", f.Name)
		}
		if f.SHA256 != "" && !hexRe.MatchString(f.SHA256) {
			return fmt.Errorf("invalid sha256 for %s", f.Name)
		}
		if f.Size < 0 {
			return fmt.Errorf("invalid size for %s", f.Name)
		}
	}
	return nil
}

// Housekeeping purges old history, logs, sessions and stale work dirs.
func (r *Runner) Housekeeping() {
	st := r.Store.GetSettings()
	r.Store.PurgeSessions()
	if st.HistoryDays > 0 {
		before := time.Now().AddDate(0, 0, -st.HistoryDays)
		ids, err := r.Store.OldFinishedRuns(before)
		if err == nil {
			for _, id := range ids {
				run, err := r.Store.GetRun(id)
				if err != nil || len(run.Locations) > 0 && run.Status != store.StatusFailed && run.Status != store.StatusCanceled {
					continue
				}
				os.Remove(r.RunLogPath(id))
				_ = r.Store.DeleteRunRow(id)
			}
		}
		if ids, err := r.Store.PurgeDrillRuns(before); err == nil {
			for _, id := range ids {
				os.Remove(r.DrillLogPath(id))
			}
		}
		r.Store.PurgeAudit(time.Now().AddDate(-1, 0, 0))
	}
	r.CleanupStaleWork()
}

// sameFiles reports whether two backups list the same files with the same sizes and checksums.
func sameFiles(a, b []store.BackupFile) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string]store.BackupFile, len(a))
	for _, f := range a {
		byName[f.Name] = f
	}
	for _, f := range b {
		g, ok := byName[f.Name]
		if !ok || g.Size != f.Size || !strings.EqualFold(g.SHA256, f.SHA256) {
			return false
		}
	}
	return true
}
