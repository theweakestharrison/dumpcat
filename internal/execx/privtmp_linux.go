package execx

import (
	"os"
	"path/filepath"
	"strings"
)

// PrivateTmp returns the mount point (/tmp or /var/tmp) holding dir when it
// is private to DumpCat's systemd service (PrivateTmp=true): other users do
// not see what is written there, and systemd deletes it when the service
// stops. "" otherwise.
func PrivateTmp(dir string) string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	return privateTmpIn(string(b), dir)
}

// privateTmpIn reads a mountinfo table: a private /tmp is a bind mount of
// a systemd-private-* folder.
func privateTmpIn(mountinfo, dir string) string {
	dir = filepath.Clean(dir)
	for _, ln := range strings.Split(mountinfo, "\n") {
		f := strings.Fields(ln)
		if len(f) < 5 {
			continue
		}
		root, mp := f[3], f[4]
		if (mp == "/tmp" || mp == "/var/tmp") && strings.Contains(root, "/systemd-private-") && (dir == mp || strings.HasPrefix(dir, mp+"/")) {
			return mp
		}
	}
	return ""
}
