//go:build windows

package main

import (
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceName is the Windows service name registered by scripts/install.ps1.
const serviceName = "DumpCat"

// inService reports whether the process was started by the Windows service manager.
func inService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runService hands control to the service manager; run blocks until stop is closed.
func runService(run func(stop <-chan struct{}) error) error {
	return svc.Run(serviceName, &winService{run: run, stopWait: 60 * time.Second})
}

type winService struct {
	run      func(stop <-chan struct{}) error
	stopWait time.Duration
}

func (w *winService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- w.run(stop) }()
	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case err := <-done:
			// the server stopped on its own (e.g. the port is already in use)
			if err != nil {
				log.Printf("service stopped: %v", err)
				return true, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Printf("service stop requested, shutting down...")
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(w.stopWait / time.Millisecond)}
				close(stop)
				select {
				case <-done:
				case <-time.After(w.stopWait):
					log.Printf("shutdown did not finish within %s", w.stopWait)
				}
				return false, 0
			}
		}
	}
}
