//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// noFollow refuses to open a symbolic link as the last part of a path.
const noFollow = syscall.O_NOFOLLOW

// shareWithServer lets SQL Server read a file the agent wrote into its
// folder: it gets the folder's owner and group (when the agent runs as
// root) or its group, and mode 0640. SQL Server on Linux runs as mssql,
// which owns the folder.
func shareWithServer(p, dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if os.Geteuid() == 0 {
			if err := os.Lchown(p, int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		} else {
			_ = os.Lchown(p, -1, int(st.Gid)) // when the agent's user is in the folder's group
		}
	}
	return os.Chmod(p, 0o640)
}
