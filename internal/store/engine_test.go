package store

import (
	"path/filepath"
	"testing"

	"dumpcat/internal/secret"
)

// Servers saved before engines existed (or without one) are MySQL; the
// engine decides the default port.
func TestServerEngine(t *testing.T) {
	dir := t.TempDir()
	box, err := secret.Load(filepath.Join(dir, "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(dir, "dumpcat.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	my := &Server{Name: "my", Host: "db", Username: "bk", Flavor: "auto"}
	pg := &Server{Name: "pg", Engine: EnginePostgres, Host: "db", Username: "bk", Flavor: "auto"}
	for _, s := range []*Server{my, pg} {
		if err := st.CreateServer(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`UPDATE servers SET engine='' WHERE id=?`, my.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetServer(my.ID)
	if got.EngineName() != EngineMySQL || got.DefaultPort() != 3306 {
		t.Fatalf("blank engine: %q %d", got.EngineName(), got.DefaultPort())
	}
	got, _ = st.GetServer(pg.ID)
	if got.Engine != EnginePostgres || got.DefaultPort() != 5432 {
		t.Fatalf("postgres: %q %d", got.Engine, got.DefaultPort())
	}
	if _, port, _, _ := got.Addr(); port != 5432 {
		t.Fatalf("postgres address port %d", port)
	}
	got.Socket, got.Host = "/var/run/postgresql", ""
	if network, addr := got.AgentTarget(); network != "unix" || addr != "/var/run/postgresql/.s.PGSQL.5432" {
		t.Fatalf("agent target %s %s", network, addr)
	}
	got.Engine = EngineMySQL
	got.Socket = "/run/mysqld/mysqld.sock"
	if _, addr := got.AgentTarget(); addr != "/run/mysqld/mysqld.sock" {
		t.Fatalf("mysql socket %s", addr)
	}
}
