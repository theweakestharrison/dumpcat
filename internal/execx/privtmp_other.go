//go:build !linux

package execx

// PrivateTmp: only systemd services (Linux) have a private /tmp.
func PrivateTmp(dir string) string { return "" }
