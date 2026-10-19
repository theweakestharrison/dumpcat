package api

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"dumpcat/internal/agent"
	"dumpcat/internal/auth"
	"dumpcat/internal/store"
)

func newAgentKey() (key, hash, prefix string) {
	key = agent.KeyPrefix + auth.RandomToken(32)
	return key, auth.SHA256Hex(key), key[:12]
}

// serverURL is the address agents should connect to: the configured base
// URL, else the address this request came in on.
func (s *Server) serverURL(r *http.Request) string {
	if b := s.baseURL(); b != "" {
		return strings.TrimRight(b, "/")
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if s.Cfg.TrustProxy && r.Header.Get("X-Forwarded-Host") != "" {
		host = r.Header.Get("X-Forwarded-Host")
	}
	return scheme + "://" + host
}

// releaseURL is the release server the install commands download from.
func (s *Server) releaseURL() string {
	if s.Updater != nil && s.Updater.Server != nil {
		if u, _ := s.Updater.Server(); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return ""
}

// setupInfo adds what the install instructions for an agent need.
func (s *Server) setupInfo(r *http.Request, v map[string]any) map[string]any {
	v["server_url"] = s.serverURL(r)
	v["release_url"] = s.releaseURL()
	v["version"] = s.Version
	return v
}

func (s *Server) agentView(a *store.Agent) map[string]any {
	servers, _ := s.Store.AgentServers(a.ID)
	if servers == nil {
		servers = []string{}
	}
	return map[string]any{"agent": a, "status": s.Agents.Status(a.ID), "servers": servers}
}

func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.ListAgents()
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		out = append(out, s.agentView(&list[i]))
	}
	return ok(w, s.setupInfo(r, map[string]any{"items": out}))
}

func (s *Server) agentCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return badRequest("agent name is required (max 100 characters)")
	}
	key, hash, prefix := newAgentKey()
	a := &store.Agent{Name: name, KeyHash: hash, KeyPrefix: prefix}
	if err := s.Store.CreateAgent(a); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("an agent with this name already exists")
		}
		return err
	}
	s.audit(r, "agent_create", "agent:"+a.Name, "")
	v := s.agentView(a)
	v["key"] = key // shown once
	return ok(w, s.setupInfo(r, v))
}

func (s *Server) agentUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return badRequest("agent name is required (max 100 characters)")
	}
	a, err := s.Store.GetAgent(id)
	if err != nil {
		return err
	}
	if err := s.Store.RenameAgent(id, name); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("an agent with this name already exists")
		}
		return err
	}
	s.audit(r, "agent_rename", "agent:"+a.Name, name)
	a.Name = name
	return ok(w, s.agentView(a))
}

// agentRotate issues a new key; the old one stops working at once.
func (s *Server) agentRotate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	a, err := s.Store.GetAgent(id)
	if err != nil {
		return err
	}
	key, hash, prefix := newAgentKey()
	if err := s.Store.SetAgentKey(id, hash, prefix); err != nil {
		return err
	}
	s.Agents.Disconnect(id)
	s.audit(r, "agent_rotate_key", "agent:"+a.Name, "")
	a.KeyPrefix = prefix
	v := s.agentView(a)
	v["key"] = key
	return ok(w, s.setupInfo(r, v))
}

func (s *Server) agentDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	a, err := s.Store.GetAgent(id)
	if err != nil {
		return err
	}
	if servers, _ := s.Store.AgentServers(id); len(servers) > 0 {
		return badRequest("the agent is used by server(s): %s — switch them to another connection first", strings.Join(servers, ", "))
	}
	if err := s.Store.DeleteAgent(id); err != nil {
		return err
	}
	s.Agents.Disconnect(id)
	s.audit(r, "agent_delete", "agent:"+a.Name, "")
	return ok(w, map[string]bool{"ok": true})
}

// agentEndpoint serves the WebSocket endpoints agents connect to; they
// authenticate with their key, not with a user session.
func (s *Server) agentEndpoint(stream bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, rec)
			}
		}()
		if s.Agents == nil {
			http.Error(w, "agents are not enabled", http.StatusServiceUnavailable)
			return
		}
		if stream {
			s.Agents.ServeStream(w, r)
			return
		}
		s.Agents.ServeControl(w, r, s.clientIP(r))
	}
}

// AgentAuth resolves an agent key (used by the agent manager).
func AgentAuth(st *store.Store) func(key string) (int64, error) {
	return func(key string) (int64, error) {
		if !strings.HasPrefix(key, agent.KeyPrefix) {
			return 0, fmt.Errorf("not an agent key")
		}
		a, err := st.AgentByKeyHash(auth.SHA256Hex(key))
		if err != nil {
			return 0, err
		}
		return a.ID, nil
	}
}
