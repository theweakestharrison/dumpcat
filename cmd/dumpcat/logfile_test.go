package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dumpcat.log")
	r, err := openRotatingFile(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 10; i++ {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	r.f.Close()
	for _, name := range []string{p, p + ".1", p + ".2"} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Size() > 100 {
			t.Fatalf("%s is %d bytes, limit 100", name, st.Size())
		}
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("kept more than 2 rotated files")
	}
}
