//go:build windows

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// relaunch starts exe again. A Windows service cannot restart itself, so a
// detached helper waits for this process to stop and then starts the service.
func relaunch(exe string, service bool) error {
	if service {
		cmd := exec.Command(exe, "update-restart", "-service", serviceName, "-pid", strconv.Itoa(os.Getpid()))
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("could not schedule the restart of the %s service: %w", serviceName, err)
		}
		log.Printf("restarting the %s service", serviceName)
		return nil
	}
	// console: start the new binary with the same arguments in this console
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start %s: %w", exe, err)
	}
	log.Printf("restarted %s (pid %d)", exe, cmd.Process.Pid)
	return nil
}

// updateRestartCmd (internal) waits until the old service process has exited and
// the service is stopped, then starts it again.
func updateRestartCmd(args []string) {
	fs := flag.NewFlagSet("update-restart", flag.ExitOnError)
	name := fs.String("service", serviceName, "service name")
	pid := fs.Int("pid", 0, "process to wait for")
	_ = fs.Parse(args)
	if *pid > 0 {
		if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(*pid)); err == nil {
			windows.WaitForSingleObject(h, 120000)
			windows.CloseHandle(h)
		}
	}
	m, err := mgr.Connect()
	if err != nil {
		os.Exit(1)
	}
	defer m.Disconnect()
	s, err := m.OpenService(*name)
	if err != nil {
		os.Exit(1)
	}
	defer s.Close()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if st, err := s.Query(); err == nil && st.State == svc.Stopped {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	for i := 0; i < 15; i++ {
		if err := s.Start(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	os.Exit(1)
}
