//go:build !windows

package execx

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func lookupIDs(name string) (uid, gid uint32, groups []uint32, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("OS user %s: %w", name, err)
	}
	ui, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gi, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, nil, fmt.Errorf("OS user %s: unexpected ids %s:%s", name, u.Uid, u.Gid)
	}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	return uint32(ui), uint32(gi), groups, nil
}

// AsUser makes cmd run as the OS user name (DumpCat running as root).
func AsUser(cmd *exec.Cmd, name string) error {
	uid, gid, groups, err := lookupIDs(name)
	if err != nil {
		return err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}
	return nil
}

// Reachable tells whether the OS user name may enter every directory above
// dir (the search permission a process of that user needs to open it).
func Reachable(dir, name string) error {
	uid, gid, groups, err := lookupIDs(name)
	if err != nil {
		return err
	}
	in := map[uint32]bool{gid: true}
	for _, g := range groups {
		in[g] = true
	}
	for d := filepath.Dir(filepath.Clean(dir)); ; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if ok {
			mode := fi.Mode().Perm()
			okx := mode&0o001 != 0 || (st.Uid == uid && mode&0o100 != 0) || (in[st.Gid] && mode&0o010 != 0)
			if !okx && uid != 0 {
				return fmt.Errorf("%s cannot enter %s (mode %s)", name, d, mode)
			}
		}
		if d == filepath.Dir(d) {
			return nil
		}
	}
}
