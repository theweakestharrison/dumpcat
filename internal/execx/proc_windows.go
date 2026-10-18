//go:build windows

package execx

import "os/exec"

func setProcAttrs(cmd *exec.Cmd) {}
