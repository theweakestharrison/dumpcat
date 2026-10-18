// Package execx wraps os/exec with process-group cancellation, low-priority
// scheduling and shell hooks.
package execx

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Command creates a command bound to ctx. Cancelling ctx terminates the whole
// process group (Linux) so that child processes do not linger.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcAttrs(cmd)
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// LowPriority wraps a command with nice/ionice when available (Linux).
func LowPriority(name string, args []string) (string, []string) {
	if runtime.GOOS != "linux" {
		return name, args
	}
	if p, err := exec.LookPath("ionice"); err == nil {
		args = append([]string{"-c2", "-n7", name}, args...)
		name = p
	}
	if p, err := exec.LookPath("nice"); err == nil {
		args = append([]string{"-n", "10", name}, args...)
		name = p
	}
	return name, args
}

// Shell returns a command running a shell script line.
func Shell(ctx context.Context, script string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return Command(ctx, "cmd", "/C", script)
	}
	return Command(ctx, "/bin/sh", "-c", script)
}

// SplitArgs splits a user supplied argument string honoring simple quotes.
func SplitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	has := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			has = true
		case r == ' ' || r == '\t' || r == '\n':
			if cur.Len() > 0 || has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 || has {
		out = append(out, cur.String())
	}
	return out
}

// Quote renders args for logging.
func Quote(name string, args []string) string {
	parts := []string{name}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$;&|<>*?()") {
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}
