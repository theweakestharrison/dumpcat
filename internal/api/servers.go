package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

type serverInput struct {
	ID              int64  `json:"id"`
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
	if sv.ID > 0 {
		// Never send a stored password to a different destination: an
		// operator could otherwise point the server at their own host.
		port := in.Port
		if port == 0 {
			port = 3306
		}
		moved := strings.TrimSpace(in.Host) != sv.Host || port != sv.Port || strings.TrimSpace(in.Socket) != sv.Socket ||
			in.AgentID != idOf(sv.AgentID) || in.WGPeerID != idOf(sv.WGPeerID)
		if (moved || strings.TrimSpace(in.Username) != sv.Username) && in.Password == "" && sv.Password != "" {
			return badRequest("re-enter the password when changing host/port/socket/user")
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
		sv.Port = 3306
	}
	if sv.Flavor == "" {
		sv.Flavor = "auto"
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
	return nil
}

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
	if info, err := dbconn.Inspect(ctx, sv, false); err == nil {
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
	if err := in.apply(sv); err != nil {
		return err
	}
	if err := s.checkRoute(sv); err != nil {
		return err
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

type methodSupport struct {
	Method  string `json:"method"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

var reVer = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func verParts(s string) (int, int, int) {
	m := reVer.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return a, b, c
}

func (s *Server) supportFor(sv *store.Server, info *dbconn.ServerInfo) []methodSupport {
	flavor := sv.Flavor
	if flavor == "" || flavor == "auto" {
		flavor = info.Flavor
	}
	var out []methodSupport
	dump := s.Tools.DumpTool(flavor)
	client := s.Tools.Get(engine.ClientToolName(flavor))
	ms := methodSupport{Method: store.MethodMysqldump, OK: dump.Found && client.Found}
	if ms.OK {
		ms.Message = dump.Version
	} else if !dump.Found {
		ms.Message = "Missing " + dump.Name + ": " + dump.Hint
	} else {
		ms.Message = "Missing client " + client.Name + " (needed for restores): " + client.Hint
	}
	out = append(out, ms)
	md := s.Tools.Get("mydumper")
	ml := s.Tools.Get("myloader")
	ms = methodSupport{Method: store.MethodMydumper, OK: md.Found && ml.Found}
	if ms.OK {
		ms.Message = md.Version
	} else {
		ms.Message = "Missing mydumper/myloader: " + md.Hint
	}
	out = append(out, ms)
	bname, sname := engine.PhysicalToolNames(flavor)
	bt := s.Tools.Get(bname)
	st := s.Tools.Get(sname)
	ms = methodSupport{Method: store.MethodPhysical, OK: bt.Found && st.Found}
	dd := sv.Datadir
	if dd == "" {
		dd = info.Datadir
	}
	switch {
	case sv.WGPeerID != nil && *sv.WGPeerID > 0:
		ms.OK = false
		ms.Message = "Not through WireGuard: physical backups copy the datadir, so the backup tool must run on the database host — connect through an agent started with --allow-physical, or install DumpCat there"
	case sv.AgentID != nil && *sv.AgentID > 0:
		ms = s.agentPhysicalSupport(*sv.AgentID, flavor, info.Version, dd)
	case !bt.Found:
		ms.Message = "Missing " + bname + ": " + bt.Hint
	case !st.Found:
		ms.Message = "Missing " + sname + ": " + st.Hint
	default:
		var note string
		ms.OK, note = physicalToolMatches(flavor, bt.Version, info.Version)
		ms.Message = bt.Version + note
		if dd != "" {
			if _, err := os.Stat(dd); err != nil {
				ms.OK = false
				ms.Message += " — datadir " + dd + " is not accessible from this machine (physical backups must run on the database server itself, with read access to the datadir, or through an agent started with --allow-physical)"
			}
		}
	}
	out = append(out, ms)
	return out
}

// physicalToolMatches checks the backup tool's version against the server's;
// note explains a mismatch (or a patch-level advice).
func physicalToolMatches(flavor, toolVersion, serverVersion string) (ok bool, note string) {
	sMaj, sMin, sPatch := verParts(serverVersion)
	if flavor == "mariadb" {
		tMaj, tMin, tPatch := verParts(toolVersion)
		if tMaj != sMaj || tMin != sMin {
			return false, fmt.Sprintf(" — does NOT match server version %d.%d (mariabackup must match the MariaDB version)", sMaj, sMin)
		} else if tPatch < sPatch {
			return true, " — use a mariabackup with the same patch level as the server"
		}
		return true, ""
	}
	// "xtrabackup version 8.0.35-30 based on MySQL server 8.0.35"
	tMaj, tMin, _ := verParts(toolVersion)
	if sMaj == 5 && sMin == 7 && !(tMaj == 2 && tMin == 4) {
		return false, " — MySQL 5.7 requires Percona XtraBackup 2.4"
	} else if sMaj >= 8 && (tMaj != sMaj || tMin != sMin) {
		return false, fmt.Sprintf(" — MySQL %d.%d requires XtraBackup %d.%d", sMaj, sMin, sMaj, sMin)
	}
	return true, ""
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
	ms.OK, note = physicalToolMatches(flavor, t.Version, serverVersion)
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
	info, err := dbconn.Inspect(ctx, sv, true)
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
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	info, err := dbconn.Inspect(ctx, sv, r.URL.Query().Get("sizes") != "0")
	if err != nil {
		return ok(w, map[string]any{"ok": false, "error": err.Error()})
	}
	s.Store.UpdateServerDetected(id, info.Version, info.Datadir)
	return ok(w, map[string]any{"ok": true, "info": info, "support": s.supportFor(sv, info)})
}
