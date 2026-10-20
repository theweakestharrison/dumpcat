package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/drivers"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

type serverInput struct {
	ID              int64  `json:"id"`
	Engine          string `json:"engine"` // "" = mysql
	Name            string `json:"name"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Socket          string `json:"socket"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	TLSMode         string `json:"tls_mode"`
	CACert          string `json:"ca_cert"`
	Flavor          string `json:"flavor"`
	Datadir         string `json:"datadir"`
	ServiceName     string `json:"service_name"`
	OSUser          string `json:"os_user"`
	Notes           string `json:"notes"`
	RestoreUsername string `json:"restore_username"`
	RestorePassword string `json:"restore_password"`
	AgentID         int64  `json:"agent_id"`   // 0 = connect directly
	WGPeerID        int64  `json:"wg_peer_id"` // 0 = not through WireGuard
}

func (in *serverInput) apply(sv *store.Server) error {
	if in.Engine == "" {
		in.Engine = store.EngineMySQL
	}
	drv := drivers.For(in.Engine)
	if drv == nil {
		return badRequest("invalid database engine %q", in.Engine)
	}
	if sv.ID > 0 {
		if in.Engine != sv.EngineName() {
			// the password belongs to another kind of server too
			if in.Password == "" && sv.Password != "" {
				return badRequest("re-enter the password when changing the database engine")
			}
		}
		// Never send a stored password to a different destination: an
		// operator could otherwise point the server at their own host.
		port := in.Port
		if port == 0 {
			port = drv.DefaultPort()
		}
		moved := strings.TrimSpace(in.Host) != sv.Host || port != sv.Port || strings.TrimSpace(in.Socket) != sv.Socket ||
			in.AgentID != idOf(sv.AgentID) || in.WGPeerID != idOf(sv.WGPeerID)
		// weaker TLS, or another CA, lets someone on the path pose as the server: same rule
		tlsRank := map[string]int{"": 0, "disabled": 0, "required": 1, "verify": 2}
		if tlsRank[in.TLSMode] < tlsRank[sv.TLSMode] || (sv.TLSMode == "verify" && strings.TrimSpace(in.CACert) != sv.CACert) {
			moved = true
		}
		if (moved || strings.TrimSpace(in.Username) != sv.Username) && in.Password == "" && sv.Password != "" {
			return badRequest("re-enter the password when changing host/port/socket/user or weakening TLS")
		}
		ru := strings.TrimSpace(in.RestoreUsername)
		if ru != "" && (moved || ru != sv.RestoreUsername) && in.RestorePassword == "" && sv.RestorePassword != "" {
			return badRequest("re-enter the restore password when changing host/port/socket/restore user")
		}
	}
	if in.AgentID > 0 && in.WGPeerID > 0 {
		return badRequest("connect through an agent or through WireGuard, not both")
	}
	sv.AgentID, sv.WGPeerID = nil, nil
	if in.AgentID > 0 {
		id := in.AgentID
		sv.AgentID = &id
	}
	if in.WGPeerID > 0 {
		id := in.WGPeerID
		sv.WGPeerID = &id
	}
	sv.Engine = in.Engine
	sv.Name = strings.TrimSpace(in.Name)
	sv.Host = strings.TrimSpace(in.Host)
	sv.Port = in.Port
	sv.Socket = strings.TrimSpace(in.Socket)
	sv.Username = strings.TrimSpace(in.Username)
	if in.Password != "" {
		sv.Password = in.Password
	}
	sv.TLSMode = in.TLSMode
	sv.CACert = strings.TrimSpace(in.CACert)
	sv.Flavor = in.Flavor
	sv.Datadir = strings.TrimSpace(in.Datadir)
	sv.ServiceName = strings.TrimSpace(in.ServiceName)
	sv.OSUser = strings.TrimSpace(in.OSUser)
	sv.Notes = in.Notes
	sv.RestoreUsername = strings.TrimSpace(in.RestoreUsername)
	if in.RestorePassword != "" {
		sv.RestorePassword = in.RestorePassword
	}
	if sv.RestoreUsername == "" {
		sv.RestorePassword = ""
	}
	sv.HasPassword = sv.Password != ""
	sv.HasRestorePassword = sv.RestorePassword != ""
	if sv.Port == 0 {
		sv.Port = drv.DefaultPort()
	}
	if sv.Flavor == "" || sv.Engine != store.EngineMySQL {
		sv.Flavor = "auto" // MySQL's flavors only
	}
	switch sv.TLSMode {
	case "", "disabled", "required", "verify":
	default:
		return badRequest("invalid tls_mode")
	}
	switch sv.Flavor {
	case "auto", "mysql", "mariadb", "percona":
	default:
		return badRequest("invalid flavor")
	}
	if sv.Name == "" {
		return badRequest("server name is required")
	}
	if sv.Host == "" && sv.Socket == "" {
		return badRequest("host or socket is required")
	}
	if sv.Username == "" {
		return badRequest("user is required")
	}
	// these values go into option files and command lines run as the service user: a line break
	// would add options of its own
	for label, v := range map[string]string{"host": sv.Host, "socket": sv.Socket, "user": sv.Username, "restore user": sv.RestoreUsername,
		"datadir": sv.Datadir, "service name": sv.ServiceName, "OS user": sv.OSUser, "name": sv.Name} {
		if strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return badRequest("the %s contains a line break or another control character", label)
		}
	}
	if sv.ServiceName != "" && !serviceNameRe.MatchString(sv.ServiceName) {
		return badRequest("invalid service name: letters, digits and @ . _ : - only, e.g. mariadb or mysql@main")
	}
	if sv.OSUser != "" && !osUserRe.MatchString(sv.OSUser) {
		return badRequest("invalid OS user: a Linux user name such as mysql")
	}
	if sv.Datadir != "" && (!strings.HasPrefix(sv.Datadir, "/") && !filepath.IsAbs(sv.Datadir)) {
		return badRequest("the datadir must be an absolute path")
	}
	if strings.HasPrefix(sv.Host, "-") || strings.HasPrefix(sv.Username, "-") || strings.HasPrefix(sv.RestoreUsername, "-") {
		return badRequest("the host and user names cannot start with -")
	}
	return nil
}

var (
	serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,127}$`)
	osUserRe      = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
)

func idOf(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// checkRoute validates the agent or WireGuard peer a server is reached through.
func (s *Server) checkRoute(sv *store.Server) error {
	if sv.AgentID != nil {
		if _, err := s.Store.GetAgent(*sv.AgentID); err != nil {
			return badRequest("agent #%d does not exist", *sv.AgentID)
		}
	}
	if sv.WGPeerID != nil {
		p, err := s.Store.GetWGPeer(*sv.WGPeerID)
		if err != nil {
			return badRequest("WireGuard peer #%d does not exist", *sv.WGPeerID)
		}
		if sv.Socket != "" && (sv.Host == "" || sv.Host == "localhost") {
			return badRequest("a unix socket cannot be reached through WireGuard: enter the database host's WireGuard address (%s)", p.Address)
		}
		ip, err := netip.ParseAddr(sv.Host)
		if err != nil {
			return badRequest("through WireGuard the host must be an IP address, e.g. the peer's address %s", p.Address)
		}
		if !wgRoutes(p, ip.Unmap()) {
			return badRequest("%s is not routed to WireGuard peer %s: use its address %s or add the network to the peer", ip, p.Name, p.Address)
		}
	}
	return nil
}

func (s *Server) serversList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListServers()
	if err != nil {
		return err
	}
	if list == nil {
		list = []store.Server{}
	}
	return ok(w, list)
}

func (s *Server) serverGet(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	sv, err := s.Store.GetServer(id)
	if err != nil {
		return err
	}
	return ok(w, sv)
}

func (s *Server) serverCreate(w http.ResponseWriter, r *http.Request) error {
	var in serverInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	sv := &store.Server{}
	if err := in.apply(sv); err != nil {
		return err
	}
	if err := s.checkRoute(sv); err != nil {
		return err
	}
	if err := s.Store.CreateServer(sv); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a server with this name already exists")
		}
		return err
	}
	s.audit(r, "server_create", "server:"+sv.Name, sv.Host)
	go s.detectServer(sv.ID)
	return ok(w, sv)
}

func (s *Server) detectServer(id int64) {
	sv, err := s.Store.GetServer(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drv := drivers.ForServer(sv)
	if drv == nil {
		return
	}
	if info, err := drv.Inspect(ctx, sv, false); err == nil {
		s.Store.UpdateServerDetected(id, info.Version, info.Datadir)
	}
}

func (s *Server) serverUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in serverInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	sv, err := s.Store.GetServer(id)
	if err != nil {
		return err
	}
	before := sv.EngineName()
	if err := in.apply(sv); err != nil {
		return err
	}
	if err := s.checkRoute(sv); err != nil {
		return err
	}
	if sv.EngineName() != before {
		if n, _ := s.Store.ServerInUse(id); n > 0 {
			return badRequest("server is used by %d job(s) of its current engine — delete or reassign them before changing the engine", n)
		}
		if n := s.Store.DrillsUsingServer(id); n > 0 {
			return badRequest("server is the sandbox of %d restore drill(s) — change or delete them before changing the engine", n)
		}
	}
	if err := s.Store.UpdateServer(sv); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a server with this name already exists")
		}
		return err
	}
	s.audit(r, "server_update", "server:"+sv.Name, "")
	go s.detectServer(sv.ID)
	return ok(w, sv)
}

func (s *Server) serverDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	sv, err := s.Store.GetServer(id)
	if err != nil {
		return err
	}
	if n, _ := s.Store.ServerInUse(id); n > 0 {
		return badRequest("server is used by %d job(s) — delete or reassign them first", n)
	}
	if n := s.Store.DrillsUsingServer(id); n > 0 {
		return badRequest("server is the sandbox of %d restore drill(s) — change or delete them first", n)
	}
	if err := s.Store.DeleteServer(id); err != nil {
		return err
	}
	s.audit(r, "server_delete", "server:"+sv.Name, "")
	return ok(w, map[string]bool{"ok": true})
}

type methodSupport = engine.MethodSupport

// supportFor tells which backup methods of the server's engine can run. A
// MySQL server behind an agent is judged by what the agent reported.
func (s *Server) supportFor(sv *store.Server, info *dbconn.ServerInfo) []methodSupport {
	drv := drivers.ForServer(sv)
	if drv == nil {
		return nil
	}
	out := drv.Support(sv, info, s.Tools)
	if sv.EngineName() == store.EngineMySQL && sv.AgentID != nil && *sv.AgentID > 0 && (sv.WGPeerID == nil || *sv.WGPeerID == 0) {
		dd := sv.Datadir
		if dd == "" {
			dd = info.Datadir
		}
		for i := range out {
			if out[i].Method == store.MethodPhysical {
				out[i] = s.agentPhysicalSupport(*sv.AgentID, engine.Flavor(sv, info), info.Version, dd)
			}
		}
	}
	return out
}

// agentPhysicalSupport tells whether the agent can run physical backups on
// the database host, from what it reported when it connected.
func (s *Server) agentPhysicalSupport(agentID int64, flavor, serverVersion, datadir string) methodSupport {
	ms := methodSupport{Method: store.MethodPhysical}
	name := fmt.Sprintf("#%d", agentID)
	if a, err := s.Store.GetAgent(agentID); err == nil {
		name = a.Name
	}
	if s.Agents == nil {
		ms.Message = "Agents are not enabled"
		return ms
	}
	in := s.Agents.Status(agentID)
	bname, sname := engine.PhysicalToolNames(flavor)
	if p := in.PhysicalProblem(bname); p != "" {
		ms.Message = "Agent " + name + ": " + p
		return ms
	}
	t := in.Caps.Physical.Tool(bname)
	var note string
	ms.OK, note = engine.PhysicalToolMatches(flavor, t.Version, serverVersion)
	ms.Message = t.Version + " on " + in.Hostname + " (agent " + name + ")" + note
	if p := in.DatadirProblem(datadir); p != "" {
		ms.OK = false
		ms.Message += " — " + p
	}
	// restores extract and prepare the backup where DumpCat runs
	if !s.Tools.Get(sname).Found || !s.Tools.Get(bname).Found {
		ms.Message += " — to restore these backups, install " + sname + " and " + bname + " on the DumpCat host too"
	}
	return ms
}

// engineView describes a database engine to the UI: the server form's
// choice, the job editor's method cards and the restore page's options.
type engineView struct {
	Engine         string                 `json:"engine"`
	Label          string                 `json:"label"`
	Product        string                 `json:"product"`
	DefaultPort    int                    `json:"default_port"`
	Methods        []engine.Method        `json:"methods"`
	RestoreOptions []engine.RestoreOption `json:"restore_options"`
	Drills         bool                   `json:"drills"`
	Log            *engine.LogInfo        `json:"log,omitempty"` // continuous log archiving and PITR
}

func (s *Server) enginesList(w http.ResponseWriter, r *http.Request) error {
	var out []engineView
	for _, d := range drivers.All() {
		_, sb := d.(engine.Sandbox)
		v := engineView{Engine: d.Engine(), Label: d.Label(), Product: d.Product(), DefaultPort: d.DefaultPort(),
			Methods: d.Methods(), RestoreOptions: d.RestoreOptions(), Drills: sb}
		if la, ok := d.(engine.LogArchiver); ok {
			info := la.LogInfo()
			v.Log = &info
		}
		out = append(out, v)
	}
	return ok(w, out)
}

func (s *Server) serverTestUnsaved(w http.ResponseWriter, r *http.Request) error {
	var in serverInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	sv := &store.Server{}
	if in.ID > 0 {
		if ex, err := s.Store.GetServer(in.ID); err == nil {
			sv = ex
		}
	}
	if in.Name == "" {
		in.Name = "test"
	}
	if err := in.apply(sv); err != nil {
		return err
	}
	if err := s.checkRoute(sv); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	info, err := drivers.ForServer(sv).Inspect(ctx, sv, true)
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	return ok(w, map[string]any{"ok": true, "info": info, "support": s.supportFor(sv, info)})
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	sv, err := s.Store.GetServer(id)
	if err != nil {
		return err
	}
	drv := drivers.ForServer(sv)
	if drv == nil {
		return ok(w, map[string]any{"ok": false, "error": fmt.Sprintf("unknown database engine %q", sv.Engine)})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	info, err := drv.Inspect(ctx, sv, r.URL.Query().Get("sizes") != "0")
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	s.Store.UpdateServerDetected(id, info.Version, info.Datadir)
	return ok(w, map[string]any{"ok": true, "info": info, "support": s.supportFor(sv, info)})
}
