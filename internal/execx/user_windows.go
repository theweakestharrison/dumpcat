//go:build windows

package execx

import (
	"errors"
	"os/exec"
)

// AsUser is not available on Windows: DumpCat's own account runs the tools.
func AsUser(cmd *exec.Cmd, name string) error {
	return errors.New("running a tool as another OS user is not supported on Windows")
}

// Reachable: Windows has no search permission bits to check.
func Reachable(dir, name string) error { return nil }
