//go:build windows

package main

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// drive runs Execute with fake service-manager channels.
func drive(t *testing.T, run func(stop <-chan struct{}) error, send ...svc.Cmd) (states []svc.State, code uint32) {
	t.Helper()
	req := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 16)
	w := &winService{run: run, stopWait: 2 * time.Second}
	type res struct {
		special bool
		code    uint32
	}
	out := make(chan res, 1)
	go func() { s, c := w.Execute(nil, req, status); out <- res{s, c} }()
	for _, c := range send {
		req <- svc.ChangeRequest{Cmd: c, CurrentStatus: svc.Status{State: svc.Running}}
	}
	var r res
	select {
	case r = <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return")
	}
	close(status)
	for s := range status {
		states = append(states, s.State)
	}
	if r.special {
		code = r.code
	}
	return states, code
}

func TestServiceStop(t *testing.T) {
	stopped := false
	run := func(stop <-chan struct{}) error { <-stop; stopped = true; return nil }
	states, code := drive(t, run, svc.Interrogate, svc.Stop)
	if !stopped || code != 0 {
		t.Fatalf("stopped=%v code=%d", stopped, code)
	}
	want := []svc.State{svc.StartPending, svc.Running, svc.Running, svc.StopPending}
	if len(states) != len(want) {
		t.Fatalf("states %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states %v, want %v", states, want)
		}
	}
}

func TestServiceRunFails(t *testing.T) {
	run := func(stop <-chan struct{}) error { return errors.New("listen tcp :6369: address already in use") }
	_, code := drive(t, run)
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
}
