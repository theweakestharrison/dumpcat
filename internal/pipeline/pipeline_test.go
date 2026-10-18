package pipeline

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

func roundTrip(t *testing.T, comp, pass string) {
	t.Helper()
	data := make([]byte, 3<<20)
	rand.Read(data[:1<<20]) // mix of random and compressible data
	copy(data[1<<20:], bytes.Repeat([]byte("INSERT INTO t VALUES (1,'abc');\n"), 1<<16))
	var buf bytes.Buffer
	var raw, out Counter
	w, err := NewWriteChain(&buf, ChainOptions{Compression: comp, Passphrase: pass, Threads: 2, RawProgress: &raw, OutProgress: &out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.RawBytes() != int64(len(data)) || raw.N() != int64(len(data)) {
		t.Fatalf("raw counter %d/%d want %d", w.RawBytes(), raw.N(), len(data))
	}
	if w.OutBytes() != int64(buf.Len()) || out.N() != int64(buf.Len()) {
		t.Fatalf("out counter %d want %d", w.OutBytes(), buf.Len())
	}
	hr := NewHashingReader(bytes.NewReader(buf.Bytes()), nil)
	r, cleanup, err := NewReader(hr, comp, pass != "", pass)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("%s/%v: data mismatch", comp, pass != "")
	}
	io.Copy(io.Discard, hr)
	if hr.SHA256() != w.SHA256() {
		t.Fatalf("sha mismatch")
	}
}

func TestRoundTrip(t *testing.T) {
	for _, c := range []string{CompZstd, CompGzip, CompNone} {
		roundTrip(t, c, "")
		roundTrip(t, c, "correct horse battery staple")
	}
}

func TestWrongPassphrase(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriteChain(&buf, ChainOptions{Compression: CompZstd, Passphrase: "right-passphrase"})
	w.Write([]byte("secret data"))
	w.Close()
	if _, _, err := NewReader(bytes.NewReader(buf.Bytes()), CompZstd, true, "wrong-passphrase"); err == nil {
		t.Fatal("expected error with wrong passphrase")
	}
}

func TestNames(t *testing.T) {
	if Ext(CompZstd, true) != ".zst.age" || Ext(CompGzip, false) != ".gz" || Ext(CompNone, true) != ".age" {
		t.Fatal("ext")
	}
	c, e := DetectFromName("shop.sql.zst.age")
	if c != CompZstd || !e {
		t.Fatal("detect")
	}
	if StripExt("backup.xbstream.gz.age") != "backup.xbstream" {
		t.Fatal("strip")
	}
	if !strings.HasSuffix(StripExt("mydumper.tar"), ".tar") {
		t.Fatal("strip tar")
	}
}
