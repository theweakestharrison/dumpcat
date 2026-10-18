// Package pipeline builds streaming compression + encryption + hashing chains
// so that backups never need to be fully materialized in memory or on disk.
//
// Write order: raw -> compress (zstd/gzip) -> encrypt (age, scrypt passphrase) -> sha256 -> sink
// Read order:  source -> decrypt -> decompress -> raw
//
// Files produced are standard formats: they can be restored without this
// application using `age -d` and `zstd -d` / `gunzip`.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"runtime"
	"strings"
	"sync/atomic"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
)

const (
	CompZstd = "zstd"
	CompGzip = "gzip"
	CompNone = "none"

	EncAge = "age-scrypt"
)

func NormalizeCompression(c string) string {
	switch strings.ToLower(c) {
	case CompZstd, "zst", "":
		return CompZstd
	case CompGzip, "gz":
		return CompGzip
	default:
		return CompNone
	}
}

// Ext returns the file extension for the given compression/encryption.
func Ext(comp string, encrypted bool) string {
	e := ""
	switch NormalizeCompression(comp) {
	case CompZstd:
		e = ".zst"
	case CompGzip:
		e = ".gz"
	}
	if encrypted {
		e += ".age"
	}
	return e
}

// DetectFromName infers compression and encryption from a filename.
func DetectFromName(name string) (comp string, encrypted bool) {
	n := strings.ToLower(name)
	if strings.HasSuffix(n, ".age") {
		encrypted = true
		n = strings.TrimSuffix(n, ".age")
	}
	switch {
	case strings.HasSuffix(n, ".zst"):
		comp = CompZstd
	case strings.HasSuffix(n, ".gz"):
		comp = CompGzip
	default:
		comp = CompNone
	}
	return
}

// StripExt removes compression/encryption extensions.
func StripExt(name string) string {
	for _, e := range []string{".age", ".zst", ".gz"} {
		name = strings.TrimSuffix(name, e)
	}
	return name
}

type Counter struct{ n atomic.Int64 }

func (c *Counter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

func (c *Counter) N() int64 {
	if c == nil {
		return 0
	}
	return c.n.Load()
}

func (c *Counter) Add(n int64) { c.n.Add(n) }

// WriteChain is an io.WriteCloser that compresses, encrypts and hashes.
type WriteChain struct {
	head    io.Writer
	in      Counter
	out     Counter
	hash    hash.Hash
	closers []io.Closer
	closed  bool
}

type ChainOptions struct {
	Compression string
	Level       int // 0 = default, 1 fastest .. 4 best (zstd); 1-9 gzip
	Threads     int
	Passphrase  string // empty = no encryption
	// Optional shared counters (whole-run progress)
	RawProgress *Counter
	OutProgress *Counter
}

func zstdLevel(l int) zstd.EncoderLevel {
	switch {
	case l <= 0:
		return zstd.SpeedDefault
	case l == 1:
		return zstd.SpeedFastest
	case l == 2:
		return zstd.SpeedDefault
	case l == 3:
		return zstd.SpeedBetterCompression
	default:
		return zstd.SpeedBestCompression
	}
}

func NewWriteChain(sink io.Writer, o ChainOptions) (*WriteChain, error) {
	threads := o.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	wc := &WriteChain{hash: sha256.New()}
	outs := []io.Writer{sink, wc.hash, &wc.out}
	if o.OutProgress != nil {
		outs = append(outs, o.OutProgress)
	}
	var w io.Writer = io.MultiWriter(outs...)

	if o.Passphrase != "" {
		rcp, err := age.NewScryptRecipient(o.Passphrase)
		if err != nil {
			return nil, err
		}
		// Each file gets its own random salt, so the KDF runs once per file.
		// 2^16 (64 MiB, ~0.2s) keeps servers with hundreds of databases fast
		// while staying memory-hard; use a long passphrase.
		rcp.SetWorkFactor(16)
		ew, err := age.Encrypt(w, rcp)
		if err != nil {
			return nil, err
		}
		wc.closers = append([]io.Closer{ew}, wc.closers...)
		w = ew
	}
	switch NormalizeCompression(o.Compression) {
	case CompZstd:
		zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstdLevel(o.Level)), zstd.WithEncoderConcurrency(threads))
		if err != nil {
			return nil, err
		}
		wc.closers = append([]io.Closer{zw}, wc.closers...)
		w = zw
	case CompGzip:
		lvl := o.Level
		if lvl <= 0 || lvl > 9 {
			lvl = 6
		}
		gw, err := pgzip.NewWriterLevel(w, lvl)
		if err != nil {
			return nil, err
		}
		_ = gw.SetConcurrency(1<<20, threads)
		wc.closers = append([]io.Closer{gw}, wc.closers...)
		w = gw
	}
	ins := []io.Writer{w, &wc.in}
	if o.RawProgress != nil {
		ins = append(ins, o.RawProgress)
	}
	// raw progress counters are written after the data reached the compressor
	wc.head = &orderedWriter{main: w, side: ins[1:]}
	return wc, nil
}

type orderedWriter struct {
	main io.Writer
	side []io.Writer
}

func (o *orderedWriter) Write(p []byte) (int, error) {
	n, err := o.main.Write(p)
	for _, s := range o.side {
		_, _ = s.Write(p[:n])
	}
	return n, err
}

func (w *WriteChain) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("write to closed chain")
	}
	return w.head.Write(p)
}

// Close flushes compressor and encryptor (in that order). It does not close the sink.
func (w *WriteChain) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	var first error
	for _, c := range w.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (w *WriteChain) RawBytes() int64 { return w.in.N() }
func (w *WriteChain) OutBytes() int64 { return w.out.N() }
func (w *WriteChain) SHA256() string  { return hex.EncodeToString(w.hash.Sum(nil)) }

// NewReader returns a reader that decrypts and decompresses src.
func NewReader(src io.Reader, comp string, encrypted bool, passphrase string) (io.Reader, func(), error) {
	r := src
	cleanup := func() {}
	if encrypted {
		if passphrase == "" {
			return nil, cleanup, errors.New("the backup is encrypted but no passphrase is available")
		}
		id, err := age.NewScryptIdentity(passphrase)
		if err != nil {
			return nil, cleanup, err
		}
		dr, err := age.Decrypt(r, id)
		if err != nil {
			return nil, cleanup, fmt.Errorf("decryption failed (wrong passphrase?): %w", err)
		}
		r = dr
	}
	switch NormalizeCompression(comp) {
	case CompZstd:
		zr, err := zstd.NewReader(r, zstd.WithDecoderMaxWindow(1<<31))
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = zr.Close
		r = zr
	case CompGzip:
		gr, err := pgzip.NewReader(r)
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { gr.Close() }
		r = gr
	}
	return r, cleanup, nil
}

// HashingReader computes sha256 and counts bytes of everything read through it.
type HashingReader struct {
	R     io.Reader
	h     hash.Hash
	N     int64
	Extra *Counter
}

func NewHashingReader(r io.Reader, extra *Counter) *HashingReader {
	return &HashingReader{R: r, h: sha256.New(), Extra: extra}
}

func (h *HashingReader) Read(p []byte) (int, error) {
	n, err := h.R.Read(p)
	if n > 0 {
		h.h.Write(p[:n])
		h.N += int64(n)
		if h.Extra != nil {
			h.Extra.Add(int64(n))
		}
	}
	return n, err
}

func (h *HashingReader) SHA256() string { return hex.EncodeToString(h.h.Sum(nil)) }

// CountingReader counts bytes into a shared counter.
type CountingReader struct {
	R io.Reader
	C *Counter
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	if n > 0 && c.C != nil {
		c.C.Add(int64(n))
	}
	return n, err
}
