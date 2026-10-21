package runner

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"dumpcat/internal/agent"
	"dumpcat/internal/auth"
	"dumpcat/internal/mssql"
	"dumpcat/internal/store"
)

// e2eAgent runs a `dumpcat agent` inside the test, the way one runs on a
// SQL Server host that DumpCat cannot share a folder with: it forwards the
// connections to target and gives DumpCat SQL Server's backup folder dir
// (--allow-mssql-dir). DumpCat (the runner) then reaches both only through
// the agent's WebSocket connections. It returns the agent's id.
func e2eAgent(t *testing.T, st *store.Store, target, dir string) int64 {
	t.Helper()
	const key = "dca_e2e_mssql_folder"
	a := &store.Agent{Name: "e2e-agent", KeyHash: auth.SHA256Hex(key), KeyPrefix: key[:12]}
	if err := st.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	mgr := agent.NewManager()
	mgr.Auth = func(k string) (int64, error) {
		if k == key {
			return a.ID, nil
		}
		return 0, errors.New("bad key")
	}
	logf := func(string, ...any) {}
	if os.Getenv("DUMPCAT_E2E_VERBOSE") != "" {
		logf = log.Printf
	}
	mgr.Logf = logf
	mux := http.NewServeMux()
	mux.HandleFunc(agent.PathConnect, func(w http.ResponseWriter, r *http.Request) { mgr.ServeControl(w, r, r.RemoteAddr) })
	mux.HandleFunc(agent.PathStream, mgr.ServeStream)
	ts := httptest.NewServer(mux)
	al, err := agent.ParseAllowList([]string{target})
	if err != nil {
		t.Fatal(err)
	}
	cl := &agent.Client{Server: ts.URL, Key: key, Allow: al, Version: "e2e", MSSQLDir: dir, Logf: logf}
	ctx, cancel := context.WithCancel(context.Background())
	go cl.Run(ctx)
	oldEndpoint, oldFolder := store.TunnelEndpoint, mssql.AgentFolder
	store.TunnelEndpoint = func(id int64, network, addr string) (string, int, error) {
		return mgr.Endpoint(id, agent.Target{Net: network, Addr: addr})
	}
	mssql.AgentFolder = func(id int64, dir string) (mssql.Files, error) {
		f, err := mgr.Folder(id, a.Name, dir)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	t.Cleanup(func() {
		cancel()
		mgr.Close()
		ts.Close()
		store.TunnelEndpoint, mssql.AgentFolder = oldEndpoint, oldFolder
	})
	for i := 0; !mgr.Status(a.ID).Online; i++ {
		if i > 250 {
			t.Fatal("the agent did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f := mgr.Status(a.ID).Caps.MSSQL; f == nil || f.Dir == "" || f.Error != "" {
		t.Fatalf("the agent's folder: %+v", f)
	}
	return a.ID
}
