package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dumpcat/internal/store"
)

// A manifest comes from a storage that someone else may be able to write: its names end up in
// local paths (the extraction directory of a physical restore) and in CREATE DATABASE.
func TestValidateManifestRejectsPathsAndSQL(t *testing.T) {
	good := func() store.Manifest {
		return store.Manifest{Method: store.MethodMysqldump, Type: store.TypeFull, JobSlug: "shop-nightly-3", DirName: "20261019-020000-full",
			Files:     []store.BackupFile{{Name: "shop.sql.zst", Size: 10}},
			Databases: []store.DBEntry{{Name: "shop", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"}}}
	}
	m := good()
	if err := validateManifest(&m); err != nil {
		t.Fatalf("a normal manifest: %v", err)
	}
	for name, edit := range map[string]func(*store.Manifest){
		"dir ..":          func(m *store.Manifest) { m.DirName = ".." },
		"dir traversal":   func(m *store.Manifest) { m.DirName = "../../../../srv/www" },
		"dir slash":       func(m *store.Manifest) { m.DirName = "a/b" },
		"dir backslash":   func(m *store.Manifest) { m.DirName = `a\..\b` },
		"dir empty":       func(m *store.Manifest) { m.DirName = "" },
		"dir dotted":      func(m *store.Manifest) { m.DirName = "x..y" },
		"slug traversal":  func(m *store.Manifest) { m.JobSlug = "../../tmp" },
		"slug absolute":   func(m *store.Manifest) { m.JobSlug = "/etc" },
		"charset SQL":     func(m *store.Manifest) { m.Databases[0].Charset = "utf8mb4; DROP DATABASE x" },
		"collation SQL":   func(m *store.Manifest) { m.Databases[0].Collation = "x COLLATE y" },
		"file name slash": func(m *store.Manifest) { m.Files[0].Name = "../x" },
	} {
		m := good()
		edit(&m)
		if err := validateManifest(&m); err == nil {
			t.Errorf("%s: accepted %+v", name, m)
		}
	}
}

func TestCheckPhysicalTarget(t *testing.T) {
	own := t.TempDir()
	datadir := t.TempDir()
	if err := checkPhysicalTarget(datadir, "mariadb", "mysql", own); err != nil {
		t.Fatalf("an empty datadir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(datadir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPhysicalTarget(datadir, "mariadb", "mysql", own); err == nil || !strings.Contains(err.Error(), "does not look like") {
		t.Fatalf("a directory that is not a datadir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(datadir, "mysql"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkPhysicalTarget(datadir, "mysql@main", "_mysql", own); err != nil {
		t.Fatalf("a datadir: %v", err)
	}
	for name, c := range map[string][3]string{
		"service option": {datadir, "--root=/x", "mysql"},
		"service space":  {datadir, "mysql stop", "mysql"},
		"user option":    {datadir, "mysql", "-R"},
		"user colon":     {datadir, "mysql", "root:root"},
		"inside own":     {filepath.Join(own, "data"), "mysql", "mysql"},
		"own itself":     {own, "mysql", "mysql"},
		"relative":       {"var/lib/mysql", "mysql", "mysql"},
		"not clean":      {datadir + string(filepath.Separator) + ".", "mysql", "mysql"},
	} {
		if err := checkPhysicalTarget(c[0], c[1], c[2], own); err == nil {
			t.Errorf("%s: accepted %v", name, c)
		}
	}
	if runtime.GOOS != "windows" {
		for _, d := range []string{"/etc", "/etc/mysql", "/usr/lib/mysql", "/boot", "/var", "/var/lib", "/root"} {
			if err := checkPhysicalTarget(d, "mysql", "mysql", own); err == nil {
				t.Errorf("system directory %s accepted", d)
			}
		}
	}
}
