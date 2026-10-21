//go:build windows

package mssql

// shareWithServer: a file created in the folder inherits the folder's
// permissions, which already let SQL Server's service account in.
func shareWithServer(p, dir string) error { return nil }
