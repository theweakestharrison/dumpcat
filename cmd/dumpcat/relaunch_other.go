//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
)

// relaunch replaces this process with exe (same PID, so systemd keeps tracking it).
func relaunch(exe string, service bool) error {
	log.Printf("restarting %s", exe)
	return syscall.Exec(exe, append([]string{exe}, os.Args[1:]...), os.Environ())
}

func updateRestartCmd(args []string) {
	fmt.Fprintln(os.Stderr, "update-restart is only used on Windows")
	os.Exit(2)
}
