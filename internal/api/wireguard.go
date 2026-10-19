package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"dumpcat/internal/store"
	"dumpcat/internal/wg"
)

// wgPrefixes are the addresses routed to a peer: its VPN address and the
// networks behind it.
func wgPrefixes(p *store.WGPeer) []netip.Prefix {
	var out []netip.Prefix
	if a, err := netip.ParseAddr(p.Address); err == nil {
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	for _, n := range p.Networks {
		if pf, err := netip.ParsePrefix(n); err == nil {
			out = append(out, pf.Masked())
		}
	}
	return out
}

func wgRoutes(p *store.WGPeer, ip netip.Addr) bool {
	for _, pf := range wgPrefixes(p) {
		if pf.Contains(ip) {
			return true
		}
	}
	return false
}

// WireGuardConfig builds the runtime configuration from the store; nil when
// WireGuard is turned off.
func WireGuardConfig(st *store.Store) (*wg.Config, error) {
	w := st.GetWireGuard()
	if !w.Enabled {
		return nil, nil
	}
	addr, err := netip.ParsePrefix(w.Address)
	if err != nil {
		return nil, fmt.Errorf("WireGuard address: %w", err)
	}
	key, err := wg.ParseKey(w.PrivateKey)
	if err != nil {
		return nil, errors.New("the WireGuard key cannot be read (was the master key changed?)")
	}
	peers, err := st.ListWGPeers()
	if err != nil {
		return nil, err
	}
	cfg := &wg.Config{ListenPort: w.ListenPort, Address: addr, PrivateKey: key}
	for i := range peers {
		p := &peers[i]
		pub, err := wg.ParseKey(p.PublicKey)
		if err != nil {
			continue
		}
		var psk wg.Key
		if p.PresharedKey != "" {
			psk, _ = wg.ParseKey(p.PresharedKey)
		}
		cfg.Peers = append(cfg.Peers, wg.Peer{ID: p.ID, Name: p.Name, PublicKey: pub, PresharedKey: psk, AllowedIPs: wgPrefixes(p), Endpoint: p.Endpoint})
	}
	return cfg, nil
}

func (s *Server) applyWG() error {
	if s.WG == nil {
		return errors.New("WireGuard is not available")
	}
	cfg, err := WireGuardConfig(s.Store)
	if err != nil {
		return err
	}
	return s.WG.Apply(cfg)
}

// wgDefaults fills what an unconfigured WireGuard needs and creates the key.
func (s *Server) wgDefaults() (store.WireGuard, error) {
	w := s.Store.GetWireGuard()
	if w.ListenPort == 0 {
		w.ListenPort = wg.DefaultPort
	}
	if w.Address == "" {
		w.Address = wg.DefaultAddress
	}
	if _, err := wg.ParseKey(w.PrivateKey); err != nil {
		k, err := wg.GenerateKey()
		if err != nil {
			return w, err
		}
		w.PrivateKey, w.PublicKey = k.String(), k.Public().String()
		if err := s.Store.SaveWireGuard(w); err != nil {
			return w, err
		}
	}
	return w, nil
}

// wgEndpoint is the host:port peers connect to: the configured endpoint,
// else the host of the base URL or of this request.
func (s *Server) wgEndpoint(r *http.Request, w store.WireGuard) string {
	ep := strings.TrimSpace(w.Endpoint)
	if ep == "" {
		ep = s.serverURL(r)
		if u, err := url.Parse(ep); err == nil && u.Hostname() != "" {
			ep = u.Hostname()
		}
	}
	if _, _, err := net.SplitHostPort(ep); err == nil {
		return ep
	}
	return net.JoinHostPort(strings.Trim(ep, "[]"), strconv.Itoa(w.ListenPort))
}

func (s *Server) wgPeerView(p *store.WGPeer, status map[int64]wg.PeerStatus) map[string]any {
	servers, _ := s.Store.WGPeerServers(p.ID)
	if servers == nil {
		servers = []string{}
	}
	return map[string]any{"peer": p, "status": status[p.ID], "servers": servers}
}

func (s *Server) wgGet(w http.ResponseWriter, r *http.Request) error {
	cfg := s.Store.GetWireGuard()
	if cfg.ListenPort == 0 {
		cfg.ListenPort = wg.DefaultPort
	}
	if cfg.Address == "" {
		cfg.Address = wg.DefaultAddress
	}
	peers, err := s.Store.ListWGPeers()
	if err != nil {
		return err
	}
	status := map[int64]wg.PeerStatus{}
	state := wg.State{}
	if s.WG != nil {
		status, state = s.WG.Status(), s.WG.State()
	}
	items := make([]map[string]any, 0, len(peers))
	for i := range peers {
		items = append(items, s.wgPeerView(&peers[i], status))
	}
	return ok(w, map[string]any{"config": cfg, "state": state, "peers": items, "endpoint": s.wgEndpoint(r, cfg)})
}

func (s *Server) wgUpdate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Enabled    bool   `json:"enabled"`
		ListenPort int    `json:"listen_port"`
		Address    string `json:"address"`
		Endpoint   string `json:"endpoint"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	cfg, err := s.wgDefaults()
	if err != nil {
		return err
	}
	if in.ListenPort == 0 {
		in.ListenPort = wg.DefaultPort
	}
	if in.ListenPort < 1 || in.ListenPort > 65535 {
		return badRequest("invalid UDP port")
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(in.Address))
	if err != nil {
		return badRequest("the address must be DumpCat's VPN address with the subnet, e.g. %s", wg.DefaultAddress)
	}
	if (prefix.Addr().Is4() && (prefix.Bits() < 8 || prefix.Bits() > 30)) || (prefix.Addr().Is6() && (prefix.Bits() < 48 || prefix.Bits() > 126)) {
		return badRequest("the VPN subnet must leave room for the peers (IPv4: /8 to /30)")
	}
	if prefix.Addr() == prefix.Masked().Addr() {
		return badRequest("%s is the network address: use a host address such as %s", prefix.Addr(), prefix.Addr().Next())
	}
	peers, err := s.Store.ListWGPeers()
	if err != nil {
		return err
	}
	for _, p := range peers {
		a, err := netip.ParseAddr(p.Address)
		if err == nil && (!prefix.Contains(a) || a == prefix.Addr()) {
			return badRequest("peer %s has the address %s, which does not fit %s: delete the peer first", p.Name, p.Address, prefix)
		}
	}
	ep := strings.TrimSpace(in.Endpoint)
	if strings.Contains(ep, "://") || strings.ContainsAny(ep, " /") {
		return badRequest("the endpoint is a host name or IP address, optionally with :port (e.g. backup.example.com)")
	}
	cfg.Enabled, cfg.ListenPort, cfg.Address, cfg.Endpoint = in.Enabled, in.ListenPort, prefix.String(), ep
	if err := s.Store.SaveWireGuard(cfg); err != nil {
		return err
	}
	s.audit(r, "wireguard_update", "wireguard", fmt.Sprintf("enabled=%v port=%d address=%s", cfg.Enabled, cfg.ListenPort, cfg.Address))
	applyErr := s.applyWG()
	out := map[string]any{"config": cfg}
	if s.WG != nil {
		out["state"] = s.WG.State()
	}
	if applyErr != nil {
		out["error"] = applyErr.Error()
	}
	return ok(w, out)
}

type wgPeerInput struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	Networks []string `json:"networks"`
}

// checkNetworks validates the networks routed to a peer.
func checkNetworks(in []string, vpn netip.Prefix, others []store.WGPeer, self int64) ([]string, error) {
	out := []string{}
	var mine []netip.Prefix
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		pf, err := netip.ParsePrefix(n)
		if err != nil {
			a, aerr := netip.ParseAddr(n)
			if aerr != nil {
				return nil, badRequest("%q is not a network (CIDR such as 192.168.10.0/24)", n)
			}
			pf = netip.PrefixFrom(a, a.BitLen())
		}
		pf = pf.Masked()
		if pf.Bits() == 0 {
			return nil, badRequest("%s would send all traffic to the peer", pf)
		}
		if pf.Overlaps(vpn) {
			return nil, badRequest("%s overlaps the VPN subnet %s", pf, vpn.Masked())
		}
		for _, m := range mine {
			if m.Overlaps(pf) {
				return nil, badRequest("%s overlaps %s", pf, m)
			}
		}
		for i := range others {
			if others[i].ID == self {
				continue
			}
			for _, o := range wgPrefixes(&others[i]) {
				if o.Overlaps(pf) {
					return nil, badRequest("%s is already routed to peer %s", pf, others[i].Name)
				}
			}
		}
		mine = append(mine, pf)
		out = append(out, pf.String())
	}
	return out, nil
}

func nextWGAddress(vpn netip.Prefix, peers []store.WGPeer) (netip.Addr, error) {
	used := map[netip.Addr]bool{vpn.Addr(): true}
	for _, p := range peers {
		if a, err := netip.ParseAddr(p.Address); err == nil {
			used[a] = true
		}
	}
	a := vpn.Masked().Addr().Next()
	for i := 0; i < 65536 && vpn.Contains(a); i++ {
		n := a.Next()
		last := !vpn.Contains(n) // IPv4 broadcast
		if !used[a] && !(last && a.Is4()) {
			return a, nil
		}
		a = n
	}
	return netip.Addr{}, badRequest("no free address left in %s", vpn)
}

// wgSetup is what a database host needs: its configuration file (with its
// private key: returned once) and how to install it.
func (s *Server) wgSetup(r *http.Request, p *store.WGPeer, priv, psk wg.Key, cfg store.WireGuard) (map[string]any, error) {
	server, err := wg.ParseKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	vpn, err := netip.ParsePrefix(cfg.Address)
	if err != nil {
		return nil, err
	}
	addr, err := netip.ParseAddr(p.Address)
	if err != nil {
		return nil, err
	}
	endpoint := s.wgEndpoint(r, cfg)
	return map[string]any{
		"peer":           p,
		"config":         wg.PeerConfig(p.Name, priv, addr, server.Public(), psk, endpoint, vpn.Addr()),
		"endpoint":       endpoint,
		"server_address": vpn.Addr().String(),
		"enabled":        cfg.Enabled,
	}, nil
}

func (s *Server) wgPeerCreate(w http.ResponseWriter, r *http.Request) error {
	var in wgPeerInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return badRequest("peer name is required (max 100 characters)")
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	cfg, err := s.wgDefaults()
	if err != nil {
		return err
	}
	vpn, err := netip.ParsePrefix(cfg.Address)
	if err != nil {
		return err
	}
	peers, err := s.Store.ListWGPeers()
	if err != nil {
		return err
	}
	var addr netip.Addr
	if a := strings.TrimSpace(in.Address); a != "" {
		if addr, err = netip.ParseAddr(a); err != nil || !vpn.Contains(addr) || addr == vpn.Addr() || addr == vpn.Masked().Addr() {
			return badRequest("the peer address must be a free address in %s", vpn.Masked())
		}
		for _, p := range peers {
			if p.Address == addr.String() {
				return badRequest("%s is already used by peer %s", addr, p.Name)
			}
		}
	} else if addr, err = nextWGAddress(vpn, peers); err != nil {
		return err
	}
	nets, err := checkNetworks(in.Networks, vpn, peers, 0)
	if err != nil {
		return err
	}
	priv, err := wg.GenerateKey()
	if err != nil {
		return err
	}
	psk, err := wg.GeneratePSK()
	if err != nil {
		return err
	}
	p := &store.WGPeer{Name: name, PublicKey: priv.Public().String(), PresharedKey: psk.String(), Address: addr.String(), Networks: nets}
	if err := s.Store.CreateWGPeer(p); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a peer with this name already exists")
		}
		return err
	}
	s.audit(r, "wireguard_peer_create", "wireguard:"+p.Name, p.Address)
	out, err := s.wgSetup(r, p, priv, psk, cfg)
	if err != nil {
		return err
	}
	if err := s.applyWG(); err != nil {
		out["error"] = err.Error()
	}
	return ok(w, out)
}

func (s *Server) wgPeerUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in wgPeerInput
	if err := readJSON(r, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 100 {
		return badRequest("peer name is required (max 100 characters)")
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	p, err := s.Store.GetWGPeer(id)
	if err != nil {
		return err
	}
	cfg, err := s.wgDefaults()
	if err != nil {
		return err
	}
	vpn, err := netip.ParsePrefix(cfg.Address)
	if err != nil {
		return err
	}
	peers, err := s.Store.ListWGPeers()
	if err != nil {
		return err
	}
	nets, err := checkNetworks(in.Networks, vpn, peers, id)
	if err != nil {
		return err
	}
	next := *p
	next.Name, next.Networks = name, nets
	// servers using the peer must stay reachable
	servers, err := s.Store.ListServers()
	if err != nil {
		return err
	}
	for _, sv := range servers {
		if sv.WGPeerID == nil || *sv.WGPeerID != id {
			continue
		}
		if ip, err := netip.ParseAddr(sv.Host); err == nil && !wgRoutes(&next, ip.Unmap()) {
			return badRequest("server %s uses %s, which would no longer be routed to this peer", sv.Name, sv.Host)
		}
	}
	if err := s.Store.UpdateWGPeer(&next); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return badRequest("a peer with this name already exists")
		}
		return err
	}
	s.audit(r, "wireguard_peer_update", "wireguard:"+p.Name, fmt.Sprintf("name=%s networks=%s", next.Name, strings.Join(next.Networks, ",")))
	out := s.wgPeerView(&next, nil)
	if err := s.applyWG(); err != nil {
		out["error"] = err.Error()
	}
	return ok(w, out)
}

// wgPeerRotate issues new keys: the old configuration stops working at once.
func (s *Server) wgPeerRotate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	p, err := s.Store.GetWGPeer(id)
	if err != nil {
		return err
	}
	cfg, err := s.wgDefaults()
	if err != nil {
		return err
	}
	priv, err := wg.GenerateKey()
	if err != nil {
		return err
	}
	psk, err := wg.GeneratePSK()
	if err != nil {
		return err
	}
	p.PublicKey, p.PresharedKey = priv.Public().String(), psk.String()
	if err := s.Store.SetWGPeerKeys(id, p.PublicKey, p.PresharedKey); err != nil {
		return err
	}
	s.audit(r, "wireguard_peer_rotate", "wireguard:"+p.Name, "")
	out, err := s.wgSetup(r, p, priv, psk, cfg)
	if err != nil {
		return err
	}
	if s.WG != nil {
		s.WG.DropPeer(id)
	}
	if err := s.applyWG(); err != nil {
		out["error"] = err.Error()
	}
	return ok(w, out)
}

func (s *Server) wgPeerDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	s.wgMu.Lock()
	defer s.wgMu.Unlock()
	p, err := s.Store.GetWGPeer(id)
	if err != nil {
		return err
	}
	if servers, _ := s.Store.WGPeerServers(id); len(servers) > 0 {
		return badRequest("the peer is used by server(s): %s — switch them to another connection first", strings.Join(servers, ", "))
	}
	if err := s.Store.DeleteWGPeer(id); err != nil {
		return err
	}
	if s.WG != nil {
		s.WG.DropPeer(id)
	}
	s.audit(r, "wireguard_peer_delete", "wireguard:"+p.Name, p.Address)
	_ = s.applyWG()
	return ok(w, map[string]bool{"ok": true})
}
