package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"dumpcat/internal/sysinfo"
)

type localBackend struct {
	root string
}

func newLocal(cfg map[string]string) (*localBackend, error) {
	p := strings.TrimSpace(cfg["path"])
	if p == "" {
		return nil, errors.New("no path configured")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	return &localBackend{root: abs}, nil
}

func (l *localBackend) Type() string     { return "local" }
func (l *localBackend) Describe() string { return l.root }
func (l *localBackend) IsLocal() bool    { return true }

func (l *localBackend) LocalPath(rel string) string {
	rel, _ = CleanRel(rel)
	return filepath.Join(l.root, filepath.FromSlash(rel))
}

func (l *localBackend) UploadDir(ctx context.Context, localDir, rel string, op *Op) error {
	dst := l.LocalPath(rel)
	if filepath.Clean(dst) == filepath.Clean(localDir) {
		return nil
	}
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		op.logger().Printf("copy %s -> %s", e.Name(), dst)
		if err := copyFile(ctx, filepath.Join(localDir, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, &ctxReader{ctx, in}); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (l *localBackend) PutStream(ctx context.Context, rel string, r io.Reader, op *Op) error {
	dst := l.LocalPath(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, &ctxReader{ctx, r}); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (l *localBackend) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	return os.Open(l.LocalPath(rel))
}

func (l *localBackend) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	return os.ReadFile(l.LocalPath(rel))
}

func (l *localBackend) List(ctx context.Context, rel string) ([]Entry, error) {
	dir := l.LocalPath(rel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Entry{}, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{Path: joinRel(rel, e.Name()), Name: e.Name(), Size: info.Size(), IsDir: e.IsDir(), ModTime: info.ModTime()})
	}
	return out, nil
}

func (l *localBackend) FindFiles(ctx context.Context, rel, name string, maxDepth int) ([]Entry, error) {
	base := l.LocalPath(rel)
	var out []Entry
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return nil
		}
		r, _ := filepath.Rel(base, p)
		depth := len(strings.Split(filepath.ToSlash(r), "/"))
		if d.IsDir() {
			if r != "." && depth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == name {
			info, _ := d.Info()
			e := Entry{Path: joinRel(rel, filepath.ToSlash(r)), Name: d.Name()}
			if info != nil {
				e.Size, e.ModTime = info.Size(), info.ModTime()
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func (l *localBackend) DeleteDir(ctx context.Context, rel string) error {
	rel, err := CleanRel(rel)
	if err != nil || rel == "" {
		return errors.New("refusing to delete the storage root directory")
	}
	return os.RemoveAll(l.LocalPath(rel))
}

func (l *localBackend) DeleteFile(ctx context.Context, rel string) error {
	err := os.Remove(l.LocalPath(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (l *localBackend) DirSize(ctx context.Context, rel string) (int64, error) {
	var total int64
	err := filepath.WalkDir(l.LocalPath(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

func (l *localBackend) Test(ctx context.Context) (string, error) {
	if err := os.MkdirAll(l.root, 0o750); err != nil {
		return "", fmt.Errorf("could not create the directory: %w", err)
	}
	f := filepath.Join(l.root, ".dumpcat-write-test")
	if err := os.WriteFile(f, []byte("ok"), 0o640); err != nil {
		return "", fmt.Errorf("could not write: %w", err)
	}
	os.Remove(f)
	total, free, err := sysinfo.DiskUsage(l.root)
	if err != nil {
		return "Write/read OK", nil
	}
	return fmt.Sprintf("Write/read OK — %s free / %s", humanBytes(free), humanBytes(total)), nil
}

func (l *localBackend) About(ctx context.Context) (*Usage, error) {
	if err := os.MkdirAll(l.root, 0o750); err != nil {
		return nil, err
	}
	total, free, err := sysinfo.DiskUsage(l.root)
	if err != nil {
		return nil, err
	}
	return &Usage{Total: total, Free: free, Used: total - free}, nil
}

func (l *localBackend) Download(ctx context.Context, rel, localPath string, op *Op) error {
	return copyFile(ctx, l.LocalPath(rel), localPath)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
