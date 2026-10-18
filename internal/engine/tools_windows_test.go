//go:build windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsToolDirs(t *testing.T) {
	pf := t.TempDir()
	for _, d := range []string{`MariaDB 10.11\bin`, `MariaDB 11.4\bin`, `MySQL\MySQL Server 8.0\bin`, `MySQL\MySQL Server 8.4\bin`} {
		if err := os.MkdirAll(filepath.Join(pf, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ProgramFiles", pf)
	t.Setenv("ProgramW6432", pf)
	t.Setenv("ProgramFiles(x86)", "")
	var got []string
	for _, d := range windowsToolDirs() {
		if strings.HasPrefix(d, pf) {
			got = append(got, strings.TrimPrefix(d, pf+`\`))
		}
	}
	want := []string{`MySQL\MySQL Server 8.4\bin`, `MySQL\MySQL Server 8.0\bin`, `MariaDB 11.4\bin`, `MariaDB 10.11\bin`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWindowsFindsMariaDBDump(t *testing.T) {
	pf := t.TempDir()
	bin := filepath.Join(pf, "MariaDB 11.4", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "mariadb-dump.exe"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ProgramFiles", pf)
	t.Setenv("ProgramW6432", pf)
	t.Setenv("DUMPCAT_TOOLS_DIR", "")
	if p := lookup("mariadb-dump"); p != filepath.Join(bin, "mariadb-dump.exe") {
		t.Fatalf("lookup = %q", p)
	}
}
