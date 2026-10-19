// Package wg runs DumpCat's built-in WireGuard endpoint. It is userspace
// (wireguard-go with a gVisor network stack): no network interface, route or
// firewall rule is created on the DumpCat host. Database hosts join with
// standard WireGuard and DumpCat dials their databases through the tunnel;
// the tools (mysqldump, mysql...) reach them through local 127.0.0.1 ports.
package wg

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"dumpcat/internal/tunnel"
)

const (
	DefaultPort    = 51820
	DefaultAddress = "10.66.0.1/24"
	// Keepalive is what peers send; with it a live peer handshakes at least
	// every two minutes.
	Keepalive  = 25
	onlineFor  = 3 * time.Minute
	dialTimout = 8 * time.Second
	mtu        = 1420
)

// ErrDisabled: WireGuard is turned off.
var ErrDisabled = errors.New("WireGuard is turned off")

// Key is a Curve25519 key.
type Key [32]byte

// GenerateKey returns a new private key.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return k, err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}

// GeneratePSK returns a random preshared key.
func GeneratePSK() (Key, error) {
	var k Key
	_, err := rand.Read(k[:])
	return k, err
}

// Public returns the public key of a private key.
func (k Key) Public() Key {
	var p Key
	priv, err := ecdh.X25519().NewPrivateKey(k[:])
	if err != nil { // 32 bytes is always valid
		return p
	}
	copy(p[:], priv.PublicKey().Bytes())
	return p
}

func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }
func (k Key) IsZero() bool   { return k == Key{} }
func (k Key) hex() string    { return hex.EncodeToString(k[:]) }

// ParseKey reads a base64 key as wg and wg-quick print it.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return k, errors.New("invalid WireGuard key")
	}
	copy(k[:], b)
	return k, nil
}

// Peer is a database host (or a gateway to a network of them).
type Peer struct {
	ID           int64
	Name         string
	PublicKey    Key
	PresharedKey Key
	AllowedIPs   []netip.Prefix // its VPN address /32 (/128) plus the networks behind it
	// Endpoint is where the peer was last seen (ip:port). After a restart
	// DumpCat starts the handshake there itself instead of waiting up to two
	// minutes for the peer's next one.
	Endpoint string
}

// Config is the whole endpoint. A nil config stops it.
type Config struct {
	ListenPort int
	Address    netip.Prefix // DumpCat's VPN address and the VPN subnet, e.g. 10.66.0.1/24
	PrivateKey Key
	Peers      []Peer
}

// PeerStatus is the live state of a peer.
type PeerStatus struct {
	Online        bool      `json:"online"`
	LastHandshake time.Time `json:"last_handshake"`
	Endpoint      string    `json:"endpoint"`
	RxBytes       int64     `json:"rx_bytes"`
	TxBytes       int64     `json:"tx_bytes"`
	Connections   int       `json:"connections"`
}

// Manager owns the WireGuard device and the local ports.
type Manager struct {
	Logf func(format string, args ...any)
	// OnEndpoint is called when a peer shows up at a new address, to keep it
	// across restarts.
	OnEndpoint func(peerID int64, endpoint string)

	mu      sync.Mutex
	dev     *device.Device
	tnet    *netstack.Net
	cfg     *Config
	err     error // why the last Apply failed
	ports   map[string]*localPort
	conns   map[int64]int
	known   map[string]string // public key (hex) -> last endpoint
	stop    chan struct{}     // ends the watcher of the current device
	started time.Time
	closed  bool
}

type localPort struct {
	peerID int64
	addr   string
	ln     net.Listener
	port   int
}

func NewManager() *Manager {
	return &Manager{ports: map[string]*localPort{}, conns: map[int64]int{}, known: map[string]string{}}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Apply starts, reconfigures or (cfg == nil) stops the endpoint. Changing the
// port, address or key restarts it; peers are updated in place, without
// dropping the sessions of the others.
func (m *Manager) Apply(cfg *Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("shutting down")
	}
	m.err = nil
	if cfg == nil {
		m.stopLocked()
		return nil
	}
	if err := validate(cfg); err != nil {
		m.err = err
		return err
	}
	if m.dev != nil && m.cfg.ListenPort == cfg.ListenPort && m.cfg.Address == cfg.Address && m.cfg.PrivateKey == cfg.PrivateKey {
		if err := m.dev.IpcSet(peersUAPI(m.cfg, cfg, m.endpoints(cfg))); err != nil {
			m.err = err
			return err
		}
		m.cfg = cfg
		return nil
	}
	m.stopLocked()
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{cfg.Address.Addr()}, nil, mtu)
	if err != nil {
		m.err = err
		return err
	}
	logger := &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf:   func(format string, args ...any) { m.logf("wireguard: "+format, args...) },
	}
	dev := device.NewDevice(tdev, conn.NewStdNetBind(), logger)
	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\nreplace_peers=true\n", cfg.PrivateKey.hex(), cfg.ListenPort) + peersUAPI(nil, cfg, m.endpoints(cfg))
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		m.err = fmt.Errorf("WireGuard: %w", err)
		return m.err
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		m.err = fmt.Errorf("WireGuard: cannot listen on UDP port %d: %w", cfg.ListenPort, err)
		return m.err
	}
	m.dev, m.tnet, m.cfg, m.started = dev, tnet, cfg, time.Now()
	m.stop = make(chan struct{})
	go m.watch(dev, m.stop)
	m.logf("WireGuard listening on UDP port %d, address %s, %d peer(s)", cfg.ListenPort, cfg.Address, len(cfg.Peers))
	return nil
}

func validate(cfg *Config) error {
	if cfg.ListenPort < 1 || cfg.ListenPort > 65535 {
		return errors.New("invalid WireGuard port")
	}
	if !cfg.Address.IsValid() {
		return errors.New("invalid WireGuard address")
	}
	if cfg.PrivateKey.IsZero() {
		return errors.New("no WireGuard key")
	}
	return nil
}

// endpoints are the last known addresses of the peers of cfg: seen by this
// process, else remembered from before.
func (m *Manager) endpoints(cfg *Config) map[string]string {
	out := map[string]string{}
	for _, p := range cfg.Peers {
		ep := m.known[p.PublicKey.hex()]
		if ep == "" {
			ep = p.Endpoint
		}
		if _, err := netip.ParseAddrPort(ep); err == nil {
			out[p.PublicKey.hex()] = ep
		}
	}
	return out
}

// peersUAPI moves the device's peers from old (nil: none) to cfg. Peers that
// stay keep their session; endpoints are only set for new peers.
func peersUAPI(old, cfg *Config, endpoints map[string]string) string {
	var b strings.Builder
	had := map[Key]bool{}
	if old != nil {
		keep := map[Key]bool{}
		for _, p := range cfg.Peers {
			keep[p.PublicKey] = true
		}
		for _, p := range old.Peers {
			had[p.PublicKey] = true
			if !keep[p.PublicKey] {
				fmt.Fprintf(&b, "public_key=%s\nremove=true\n", p.PublicKey.hex())
			}
		}
	}
	for _, p := range cfg.Peers {
		fmt.Fprintf(&b, "public_key=%s\n", p.PublicKey.hex())
		if !p.PresharedKey.IsZero() {
			fmt.Fprintf(&b, "preshared_key=%s\n", p.PresharedKey.hex())
		}
		if ep := endpoints[p.PublicKey.hex()]; ep != "" && !had[p.PublicKey] {
			fmt.Fprintf(&b, "endpoint=%s\n", ep)
		}
		b.WriteString("replace_allowed_ips=true\n")
		for _, a := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", a.Masked())
		}
	}
	return b.String()
}

// ipcPeer is one peer of the device's state.
type ipcPeer struct {
	key       string // hex
	endpoint  string
	handshake time.Time
	rx, tx    int64
}

func readPeers(dev *device.Device) []ipcPeer {
	raw, err := dev.IpcGet()
	if err != nil {
		return nil
	}
	var out []ipcPeer
	var sec, nsec int64
	flush := func() {
		if n := len(out); n > 0 && sec > 0 {
			out[n-1].handshake = time.Unix(sec, nsec)
		}
		sec, nsec = 0, 0
	}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "public_key":
			flush()
			out = append(out, ipcPeer{key: v})
		case "endpoint":
			if n := len(out); n > 0 {
				out[n-1].endpoint = v
			}
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ = strconv.ParseInt(v, 10, 64)
		case "rx_bytes":
			if n := len(out); n > 0 {
				out[n-1].rx, _ = strconv.ParseInt(v, 10, 64)
			}
		case "tx_bytes":
			if n := len(out); n > 0 {
				out[n-1].tx, _ = strconv.ParseInt(v, 10, 64)
			}
		}
	}
	flush()
	return out
}

type seenAt struct {
	id       int64
	endpoint string
}

// learnLocked records where the peers are now; it returns the changes.
func (m *Manager) learnLocked() []seenAt {
	if m.dev == nil {
		return nil
	}
	ids := map[string]int64{}
	for _, p := range m.cfg.Peers {
		ids[p.PublicKey.hex()] = p.ID
	}
	var out []seenAt
	for _, p := range readPeers(m.dev) {
		if p.endpoint == "" || m.known[p.key] == p.endpoint {
			continue
		}
		m.known[p.key] = p.endpoint
		if id, ok := ids[p.key]; ok {
			out = append(out, seenAt{id, p.endpoint})
		}
	}
	return out
}

func (m *Manager) report(changes []seenAt) {
	if m.OnEndpoint == nil {
		return
	}
	for _, c := range changes {
		m.OnEndpoint(c.id, c.endpoint)
	}
}

// watch keeps the peers' addresses current while dev runs.
func (m *Manager) watch(dev *device.Device, stop chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.mu.Lock()
			var changes []seenAt
			if m.dev == dev {
				changes = m.learnLocked()
			}
			m.mu.Unlock()
			m.report(changes)
		}
	}
}

// stopLocked closes the device; local ports stay and fail with ErrDisabled.
func (m *Manager) stopLocked() {
	if m.dev != nil {
		changes := m.learnLocked()
		go m.report(changes)
		close(m.stop)
		m.dev.Close()
		m.logf("WireGuard stopped")
	}
	m.dev, m.tnet, m.cfg = nil, nil, nil
}

// State is the overall status for the UI.
type State struct {
	Running bool      `json:"running"`
	Since   time.Time `json:"since"`
	Error   string    `json:"error"`
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := State{Running: m.dev != nil}
	if m.dev != nil {
		st.Since = m.started
	}
	if m.err != nil {
		st.Error = m.err.Error()
	}
	return st
}

// Status returns the live state of every peer, by peer id.
func (m *Manager) Status() map[int64]PeerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]PeerStatus{}
	if m.dev == nil {
		return out
	}
	ids := map[string]int64{}
	for _, p := range m.cfg.Peers {
		ids[p.PublicKey.hex()] = p.ID
		out[p.ID] = PeerStatus{Connections: m.conns[p.ID]}
	}
	for _, p := range readPeers(m.dev) {
		id, ok := ids[p.key]
		if !ok {
			continue
		}
		st := PeerStatus{Endpoint: p.endpoint, RxBytes: p.rx, TxBytes: p.tx, LastHandshake: p.handshake, Connections: m.conns[id]}
		st.Online = !p.handshake.IsZero() && time.Since(p.handshake) < onlineFor
		out[id] = st
	}
	return out
}

func (m *Manager) peerLocked(id int64) *Peer {
	if m.cfg == nil {
		return nil
	}
	for i := range m.cfg.Peers {
		if m.cfg.Peers[i].ID == id {
			return &m.cfg.Peers[i]
		}
	}
	return nil
}

// Routes reports whether addr (ip:port) is routed to the peer.
func (p *Peer) Routes(ip netip.Addr) bool {
	for _, a := range p.AllowedIPs {
		if a.Contains(ip) {
			return true
		}
	}
	return false
}

// Endpoint returns a local address that reaches addr (ip:port) through the
// peer.
func (m *Manager) Endpoint(peerID int64, addr string) (string, int, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("through WireGuard the database address must be an IP address and port: %s", addr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", 0, errors.New("shutting down")
	}
	if m.dev == nil {
		return "", 0, ErrDisabled
	}
	p := m.peerLocked(peerID)
	if p == nil {
		return "", 0, fmt.Errorf("WireGuard peer #%d does not exist", peerID)
	}
	if !p.Routes(ap.Addr().Unmap()) {
		return "", 0, fmt.Errorf("%s is not routed to WireGuard peer %s (add the network to the peer)", ap.Addr(), p.Name)
	}
	key := fmt.Sprintf("%d|%s", peerID, ap)
	if lp := m.ports[key]; lp != nil {
		return "127.0.0.1", lp.port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, err
	}
	lp := &localPort{peerID: peerID, addr: ap.String(), ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	m.ports[key] = lp
	go m.accept(lp)
	return "127.0.0.1", lp.port, nil
}

func (m *Manager) accept(lp *localPort) {
	for {
		c, err := lp.ln.Accept()
		if err != nil {
			return
		}
		go m.forward(lp, c)
	}
}

func (m *Manager) forward(lp *localPort, c net.Conn) {
	remote, err := m.dial(lp)
	if err != nil {
		m.logf("WireGuard: cannot connect to %s: %v", lp.addr, err)
		tunnel.MySQLError(c, "DumpCat WireGuard: "+err.Error())
		c.Close()
		return
	}
	m.mu.Lock()
	m.conns[lp.peerID]++
	m.mu.Unlock()
	tunnel.Pipe(c, remote)
	m.mu.Lock()
	m.conns[lp.peerID]--
	m.mu.Unlock()
}

func (m *Manager) dial(lp *localPort) (net.Conn, error) {
	m.mu.Lock()
	tnet, p := m.tnet, m.peerLocked(lp.peerID)
	var name string
	if p != nil {
		name = p.Name
	}
	m.mu.Unlock()
	if tnet == nil {
		return nil, ErrDisabled
	}
	if p == nil {
		return nil, fmt.Errorf("WireGuard peer #%d was removed", lp.peerID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimout)
	defer cancel()
	c, err := tnet.DialContext(ctx, "tcp", lp.addr)
	if err == nil {
		return c, nil
	}
	st := m.Status()[lp.peerID]
	switch {
	case st.LastHandshake.IsZero():
		return nil, fmt.Errorf("peer %s has not connected yet: start WireGuard on the database host (systemctl enable --now wg-quick@dumpcat) and check that it reaches this server on UDP port %d", name, m.port())
	case strings.Contains(err.Error(), "refused"):
		return nil, fmt.Errorf("peer %s is connected but nothing listens on %s: MySQL must listen on that address (bind-address)", name, lp.addr)
	case !st.Online:
		return nil, fmt.Errorf("peer %s is offline (last handshake %s ago): check WireGuard on the database host", name, time.Since(st.LastHandshake).Round(time.Second))
	}
	return nil, fmt.Errorf("no answer from %s through peer %s (last handshake %s ago): is WireGuard still running on the database host, and does its firewall let the WireGuard interface in?", lp.addr, name, time.Since(st.LastHandshake).Round(time.Second))
}

func (m *Manager) port() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return 0
	}
	return m.cfg.ListenPort
}

// Close stops WireGuard and closes the local ports.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.stopLocked()
	for k, lp := range m.ports {
		lp.ln.Close()
		delete(m.ports, k)
	}
}

// DropPeer closes the local ports of a peer (deleted, keys rotated).
func (m *Manager) DropPeer(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, lp := range m.ports {
		if lp.peerID == id {
			lp.ln.Close()
			delete(m.ports, k)
		}
	}
}

// PeerConfig is the wg-quick file for a database host.
func PeerConfig(name string, priv Key, address netip.Addr, server Key, psk Key, endpoint string, serverAddr netip.Addr) string {
	bits := 32
	if address.Is6() {
		bits = 128
	}
	sbits := 32
	if serverAddr.Is6() {
		sbits = 128
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# DumpCat WireGuard peer %q — install as /etc/wireguard/dumpcat.conf\n", name)
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", priv)
	fmt.Fprintf(&b, "Address = %s\n", netip.PrefixFrom(address, bits))
	b.WriteString("\n[Peer]\n# DumpCat\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", server)
	if !psk.IsZero() {
		fmt.Fprintf(&b, "PresharedKey = %s\n", psk)
	}
	fmt.Fprintf(&b, "Endpoint = %s\n", endpoint)
	// only DumpCat's address goes through the tunnel: nothing else changes on the host
	fmt.Fprintf(&b, "AllowedIPs = %s\n", netip.PrefixFrom(serverAddr, sbits))
	fmt.Fprintf(&b, "PersistentKeepalive = %d\n", Keepalive)
	return b.String()
}
