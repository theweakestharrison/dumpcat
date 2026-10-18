// Package storage abstracts backup destinations. The local filesystem is
// handled natively; every remote (S3, Google Drive, OneDrive, FTP, SFTP,
// WebDAV, B2, Azure, Dropbox, custom) is driven through the rclone binary,
// which provides resumable/multipart uploads, retries and OAuth refresh.
package storage

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"dumpcat/internal/tasklog"
)

type Entry struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

type Usage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
}

// Op carries per-operation options.
type Op struct {
	Log        *tasklog.Logger
	BwLimit    string
	OnProgress func(pct int)
}

func (o *Op) logger() *tasklog.Logger {
	if o == nil || o.Log == nil {
		return tasklog.Discard()
	}
	return o.Log
}

type Backend interface {
	Type() string
	// Describe returns a human readable root location.
	Describe() string
	IsLocal() bool
	// LocalPath returns the absolute local path for rel (local backend only).
	LocalPath(rel string) string
	UploadDir(ctx context.Context, localDir, rel string, op *Op) error
	PutStream(ctx context.Context, rel string, r io.Reader, op *Op) error
	Open(ctx context.Context, rel string) (io.ReadCloser, error)
	ReadFile(ctx context.Context, rel string) ([]byte, error)
	List(ctx context.Context, rel string) ([]Entry, error)
	FindFiles(ctx context.Context, rel, name string, maxDepth int) ([]Entry, error)
	DeleteDir(ctx context.Context, rel string) error
	DeleteFile(ctx context.Context, rel string) error
	DirSize(ctx context.Context, rel string) (int64, error)
	Test(ctx context.Context) (string, error)
	About(ctx context.Context) (*Usage, error)
	// Download copies a remote file to a local path (multi-threaded when supported).
	Download(ctx context.Context, rel, localPath string, op *Op) error
}

var ErrNotSupported = errors.New("this storage does not support the operation")

// CleanRel normalizes a relative path and rejects traversal.
func CleanRel(rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "." {
		rel = ""
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", errors.New("invalid path")
		}
	}
	return rel, nil
}

func joinRel(a, b string) string {
	a = strings.Trim(a, "/")
	b = strings.Trim(b, "/")
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "/" + b
}
