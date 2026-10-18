package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFileBOMAndCRLF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dumpcat.env")
	// as written by some Windows editors: UTF-8 BOM and CRLF line endings
	body := "\ufeffDUMPCAT_TEST_LISTEN=0.0.0.0:9090\r\n# comment\r\nDUMPCAT_TEST_DATA=\"C:\\ProgramData\\DumpCat\\data\"\r\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("DUMPCAT_TEST_LISTEN")
	os.Unsetenv("DUMPCAT_TEST_DATA")
	t.Cleanup(func() { os.Unsetenv("DUMPCAT_TEST_LISTEN"); os.Unsetenv("DUMPCAT_TEST_DATA") })
	if err := LoadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("DUMPCAT_TEST_LISTEN"); v != "0.0.0.0:9090" {
		t.Fatalf("DUMPCAT_TEST_LISTEN = %q", v)
	}
	if v := os.Getenv("DUMPCAT_TEST_DATA"); v != `C:\ProgramData\DumpCat\data` {
		t.Fatalf("DUMPCAT_TEST_DATA = %q", v)
	}
}
