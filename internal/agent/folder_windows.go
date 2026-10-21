//go:build windows

package agent

// noFollow: os.Lstat and os.SameFile catch a link or a swapped file.
const noFollow = 0

// shareWithServer: a file created in the folder inherits the folder's
// permissions, which already let SQL Server's service account in.
func shareWithServer(p, dir string) error { return nil }
