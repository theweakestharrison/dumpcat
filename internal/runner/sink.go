package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"dumpcat/internal/engine"
	"dumpcat/internal/pipeline"
	"dumpcat/internal/storage"
	"dumpcat/internal/store"
)

type sinkBase struct {
	chain     pipeline.ChainOptions
	encrypted bool
	mu        sync.Mutex
	files     []store.BackupFile
}

func (s *sinkBase) chainFor(o engine.FileOpts) pipeline.ChainOptions {
	c := s.chain
	if !o.Compress {
		c.Compression = pipeline.CompNone
	}
	return c
}

func (s *sinkBase) record(f store.BackupFile) {
	s.mu.Lock()
	s.files = append(s.files, f)
	s.mu.Unlock()
}

func (s *sinkBase) Files() []store.BackupFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.BackupFile, len(s.files))
	copy(out, s.files)
	return out
}

// ---- directory sink (local storage target or staging dir) ----

type dirSink struct {
	sinkBase
	dir string
}

func newDirSink(dir string, chain pipeline.ChainOptions) (*dirSink, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &dirSink{sinkBase: sinkBase{chain: chain, encrypted: chain.Passphrase != ""}, dir: dir}, nil
}

type dirFile struct {
	s     *dirSink
	f     *os.File
	bw    *bufio.Writer
	chain *pipeline.WriteChain
	meta  store.BackupFile
	done  bool
}

func (s *dirSink) Create(name string, o engine.FileOpts) (io.WriteCloser, error) {
	c := s.chainFor(o)
	final := name + pipeline.Ext(c.Compression, s.encrypted)
	f, err := os.OpenFile(filepath.Join(s.dir, final), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	ch, err := pipeline.NewWriteChain(bw, c)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &dirFile{s: s, f: f, bw: bw, chain: ch, meta: store.BackupFile{Name: final, Kind: o.Kind, Database: o.Database}}, nil
}

func (d *dirFile) Write(p []byte) (int, error) { return d.chain.Write(p) }

func (d *dirFile) Close() error {
	if d.done {
		return nil
	}
	d.done = true
	err := d.chain.Close()
	if e := d.bw.Flush(); err == nil {
		err = e
	}
	if e := d.f.Sync(); err == nil {
		err = e
	}
	if e := d.f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	d.meta.Size = d.chain.OutBytes()
	d.meta.RawSize = d.chain.RawBytes()
	d.meta.SHA256 = d.chain.SHA256()
	d.s.record(d.meta)
	return nil
}

func (d *dirFile) Abort(err error) {
	if d.done {
		return
	}
	d.done = true
	d.chain.Close()
	d.f.Close()
	os.Remove(d.f.Name())
}

// ---- streaming sink (direct upload to remote targets, no local disk) ----

type streamTarget struct {
	loc     *store.RunLocation
	backend storage.Backend
	failed  error
}

type streamSink struct {
	sinkBase
	ctx     context.Context
	rel     string
	targets []*streamTarget
	op      *storage.Op
}

type streamFile struct {
	s     *streamSink
	chain *pipeline.WriteChain
	pws   []*io.PipeWriter
	tgts  []*streamTarget
	errs  []chan error
	mw    *tolerantMulti
	meta  store.BackupFile
	done  bool
}

func (s *streamSink) alive() []*streamTarget {
	var out []*streamTarget
	for _, t := range s.targets {
		if t.failed == nil {
			out = append(out, t)
		}
	}
	return out
}

func (s *streamSink) Create(name string, o engine.FileOpts) (io.WriteCloser, error) {
	c := s.chainFor(o)
	final := name + pipeline.Ext(c.Compression, s.encrypted)
	s.mu.Lock()
	tgts := s.alive()
	s.mu.Unlock()
	if len(tgts) == 0 {
		return nil, errors.New("no storage is still accepting data")
	}
	sf := &streamFile{s: s, meta: store.BackupFile{Name: final, Kind: o.Kind, Database: o.Database}}
	var ws []io.Writer
	for _, t := range tgts {
		pr, pw := io.Pipe()
		ch := make(chan error, 1)
		go func(t *streamTarget, pr *io.PipeReader, ch chan error) {
			err := t.backend.PutStream(s.ctx, s.rel+"/"+final, pr, s.op)
			pr.CloseWithError(err)
			ch <- err
		}(t, pr, ch)
		sf.pws = append(sf.pws, pw)
		sf.tgts = append(sf.tgts, t)
		sf.errs = append(sf.errs, ch)
		ws = append(ws, pw)
	}
	sf.mw = &tolerantMulti{ws: ws, errs: make([]error, len(ws))}
	chain, err := pipeline.NewWriteChain(sf.mw, c)
	if err != nil {
		sf.Abort(err)
		return nil, err
	}
	sf.chain = chain
	return sf, nil
}

func (f *streamFile) Write(p []byte) (int, error) { return f.chain.Write(p) }

func (f *streamFile) Close() error {
	if f.done {
		return nil
	}
	f.done = true
	cerr := f.chain.Close()
	okCount := 0
	for i, pw := range f.pws {
		if cerr != nil {
			pw.CloseWithError(cerr)
		} else if f.mw.errs[i] != nil {
			pw.CloseWithError(f.mw.errs[i])
		} else {
			pw.Close()
		}
		err := <-f.errs[i]
		if err == nil && f.mw.errs[i] != nil {
			err = f.mw.errs[i]
		}
		if err != nil || cerr != nil {
			if err == nil {
				err = cerr
			}
			f.s.mu.Lock()
			if f.tgts[i].failed == nil {
				f.tgts[i].failed = fmt.Errorf("%s: %w", f.meta.Name, err)
			}
			f.s.mu.Unlock()
			continue
		}
		okCount++
	}
	if cerr != nil {
		return cerr
	}
	if okCount == 0 {
		return fmt.Errorf("upload of %s failed on every storage", f.meta.Name)
	}
	f.meta.Size = f.chain.OutBytes()
	f.meta.RawSize = f.chain.RawBytes()
	f.meta.SHA256 = f.chain.SHA256()
	f.s.record(f.meta)
	return nil
}

func (f *streamFile) Abort(err error) {
	if f.done {
		return
	}
	f.done = true
	if f.chain != nil {
		f.chain.Close()
	}
	for i, pw := range f.pws {
		pw.CloseWithError(err)
		<-f.errs[i]
	}
}

// tolerantMulti writes to all writers, dropping the ones that fail, and only
// errors when every writer failed.
type tolerantMulti struct {
	ws   []io.Writer
	errs []error
}

func (m *tolerantMulti) Write(p []byte) (int, error) {
	alive := 0
	for i, w := range m.ws {
		if m.errs[i] != nil {
			continue
		}
		if _, err := w.Write(p); err != nil {
			m.errs[i] = err
			continue
		}
		alive++
	}
	if alive == 0 {
		return 0, errors.New("all upload streams failed")
	}
	return len(p), nil
}
