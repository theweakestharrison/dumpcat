//go:build !windows

package main

import "errors"

func inService() bool { return false }

func runService(func(stop <-chan struct{}) error) error {
	return errors.New("service mode is only available on Windows")
}
