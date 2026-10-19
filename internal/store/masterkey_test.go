package store

import (
	"os"
	"path/filepath"
	"testing"

	"dumpcat/internal/secret"
)

// A database opened with another master key must be refused: its secrets would all read as
// empty, and jobs that encrypt backups would lose their passphrase.
func TestCheckMasterKey(t *testing.T) {
	dir := t.TempDir()
	open := func(keyFile string) (*Store, error) {
		box, err := secret.Load(filepath.Join(dir, keyFile), "")
		if err != nil {
			t.Fatal(err)
		}
		st, err := Open(filepath.Join(dir, "dumpcat.db"), box)
		if err != nil {
			t.Fatal(err)
		}
		return st, st.CheckMasterKey()
	}
	st, err := open("master.key")
	if err != nil {
		t.Fatalf("a new database: %v", err)
	}
	if err := st.CreateServer(&Server{Name: "db1", Host: "127.0.0.1", Port: 3306, Username: "bk", Password: "s3cret!", Flavor: "auto"}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if st, err := open("master.key"); err != nil {
		t.Fatalf("the same key: %v", err)
	} else {
		st.Close()
	}
	st, err = open("other.key")
	if err == nil {
		t.Fatal("another key was accepted")
	}
	st.Close()

	// a database from before the check (no marker yet) is still verified through its secrets
	st, _ = open("master.key")
	if _, err := st.db.Exec(`DELETE FROM settings WHERE key=?`, keyCheckKV); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err = open("other.key"); err == nil {
		t.Fatal("another key was accepted by an old database")
	}
	st.Close()
	if _, err := os.Stat(filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
}
