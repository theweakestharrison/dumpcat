package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"dumpcat/internal/tunnel"
)

const (
	pingEvery   = 25 * time.Second
	idleTimeout = 80 * time.Second // no frame from the agent for this long: dead
	openTimeout = 20 * time.Second
)

// Info is the live state of an agent.
type Info struct {
	Online     bool      `json:"online"`
	Since      time.Time `json:"since"`
	LastSeen   time.Time `json:"last_seen"`
	RemoteAddr string    `json:"remote_addr"`
	Version    string    `json:"version"`
	Hostname   string    `json:"hostname"`
	OS         string    `json:"os"`
	Allow      []string  `json:"allow"`
	Streams    int       `json:"streams"`
	Caps       *Caps     `json:"caps"` // nil: an agent from before physical backups
}

// Manager is the server side: agent sessions and the local tunnel ports.
type Manager struct {
	// Auth maps an agent key to the agent id (an error rejects the connection).
	Auth func(key string) (int64, error)
	// OnHello records a (re)connection, e.g. to persist "last seen".
	OnHello func(id int64, info Info)
	Logf    func(format string, args ...any)

	mu       sync.Mutex
	sessions map[int64]*session
	tunnels  map[string]*localPort
	pending  map[string]*pending
	streams  map[int64]int
	active   map[*streamConn]int64 // physical backups running, by agent
	closed   bool
}

func NewManager() *Manager {
	return &Manager{sessions: map[int64]*session{}, tunnels: map[string]*localPort{}, pending: map[string]*pending{},
		streams: map[int64]int{}, active: map[*streamConn]int64{}}
}

type session struct {
	agentID int64
	ws      *websocket.Conn
	wmu     sync.Mutex
	info    Info
	done    chan struct{}
	once    sync.Once
}

func (s *session) send(m Message) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return websocket.JSON.Send(s.ws, m)
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.done)
		s.ws.Close()
	})
}

type pending struct {
	agentID int64
	conn    chan *streamConn
	fail    chan error
	gone    chan struct{}
}

type localPort struct {
	agentID int64
	target  Target
	ln      net.Listener
	port    int
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

func (m *Manager) authenticate(w http.ResponseWriter, r *http.Request) (int64, bool) {
	key := bearer(r)
	if key == "" || m.Auth == nil {
		http.Error(w, "agent key required", http.StatusUnauthorized)
		return 0, false
	}
	id, err := m.Auth(key)
	if err != nil {
		http.Error(w, "invalid agent key", http.StatusUnauthorized)
		return 0, false
	}
	return id, true
}

// agents send no Origin worth checking: the key authenticates them
func noOriginCheck(*websocket.Config, *http.Request) error { return nil }

// ServeControl handles an agent's control connection.
func (m *Manager) ServeControl(w http.ResponseWriter, r *http.Request, remote string) {
	id, ok := m.authenticate(w, r)
	if !ok {
		return
	}
	websocket.Server{Handshake: noOriginCheck, Handler: func(ws *websocket.Conn) { m.runSession(id, ws, remote) }}.ServeHTTP(w, r)
}

func (m *Manager) runSession(id int64, ws *websocket.Conn, remote string) {
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello Message
	if err := websocket.JSON.Receive(ws, &hello); err != nil || hello.Type != "hello" {
		return
	}
	now := time.Now()
	sess := &session{agentID: id, ws: ws, done: make(chan struct{}),
		info: Info{Online: true, Since: now, LastSeen: now, RemoteAddr: remote, Version: hello.Version, Hostname: hello.Hostname, OS: hello.OS, Allow: hello.Allow, Caps: hello.Caps}}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	old := m.sessions[id]
	m.sessions[id] = sess
	m.mu.Unlock()
	if old != nil {
		old.close() // a reconnect (or a second process with the same key) replaces the old session
	}
	if m.OnHello != nil {
		m.OnHello(id, sess.info)
	}
	m.logf("agent #%d connected from %s (%s, %s, DumpCat %s)", id, remote, hello.Hostname, hello.OS, hello.Version)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-sess.done:
				return
			case <-t.C:
				if sess.send(Message{Type: "ping"}) != nil {
					sess.close()
					return
				}
			}
		}
	}()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(idleTimeout))
		var msg Message
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			break
		}
		m.mu.Lock()
		sess.info.LastSeen = time.Now()
		if msg.Type == "error" {
			if p := m.pending[msg.ID]; p != nil && p.agentID == id {
				select {
				case p.fail <- errors.New(msg.Error):
				default:
				}
			}
		}
		m.mu.Unlock()
	}
	sess.close()
	m.mu.Lock()
	current := m.sessions[id] == sess
	if current {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if current {
		m.logf("agent #%d disconnected", id)
	}
}

// streamConn is the agent side of one tunneled connection; Close releases
// the WebSocket handler that owns it.
type streamConn struct {
	*websocket.Conn
	done chan struct{}
	once sync.Once
}

func (c *streamConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// ServeStream handles the data connection an agent opens for a request.
func (m *Manager) ServeStream(w http.ResponseWriter, r *http.Request) {
	id, ok := m.authenticate(w, r)
	if !ok {
		return
	}
	m.mu.Lock()
	p := m.pending[r.URL.Query().Get("id")]
	m.mu.Unlock()
	if p == nil || p.agentID != id {
		http.Error(w, "unknown stream", http.StatusNotFound)
		return
	}
	websocket.Server{Handshake: noOriginCheck, Handler: func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		c := &streamConn{Conn: ws, done: make(chan struct{})}
		select {
		case p.conn <- c:
			<-c.done // returning would close the connection
		case <-p.gone:
			ws.Close()
		}
	}}.ServeHTTP(w, r)
}

// Endpoint returns a local address that reaches target through the agent.
func (m *Manager) Endpoint(agentID int64, t Target) (string, int, error) {
	nt, err := NormalizeTarget(t)
	if err != nil {
		return "", 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", 0, errors.New("shutting down")
	}
	if m.sessions[agentID] == nil {
		return "", 0, ErrOffline
	}
	key := fmt.Sprintf("%d|%s", agentID, nt)
	if tn := m.tunnels[key]; tn != nil {
		return "127.0.0.1", tn.port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, err
	}
	tn := &localPort{agentID: agentID, target: nt, ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	m.tunnels[key] = tn
	go m.accept(tn)
	return "127.0.0.1", tn.port, nil
}

func (m *Manager) accept(tn *localPort) {
	for {
		c, err := tn.ln.Accept()
		if err != nil {
			return
		}
		go m.forward(tn, c)
	}
}

func (m *Manager) forward(tn *localPort, c net.Conn) {
	remote, err := m.request(tn.agentID, Message{Type: "open", Net: tn.target.Net, Addr: tn.target.Addr})
	if err != nil {
		m.logf("agent #%d: cannot open %s: %v", tn.agentID, tn.target, err)
		tunnel.MySQLError(c, "DumpCat agent: "+err.Error())
		c.Close()
		return
	}
	m.mu.Lock()
	m.streams[tn.agentID]++
	m.mu.Unlock()
	tunnel.Pipe(c, remote)
	m.mu.Lock()
	m.streams[tn.agentID]--
	m.mu.Unlock()
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// request sends msg (open: dial a target; physical: run a backup) and waits
// for the data connection the agent opens for it.
func (m *Manager) request(agentID int64, msg Message) (*streamConn, error) {
	m.mu.Lock()
	sess := m.sessions[agentID]
	if sess == nil {
		m.mu.Unlock()
		return nil, ErrOffline
	}
	id := randomID()
	p := &pending{agentID: agentID, conn: make(chan *streamConn), fail: make(chan error, 1), gone: make(chan struct{})}
	m.pending[id] = p
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		close(p.gone)
	}()
	msg.ID = id
	if err := sess.send(msg); err != nil {
		return nil, err
	}
	timer := time.NewTimer(openTimeout)
	defer timer.Stop()
	select {
	case c := <-p.conn:
		return c, nil
	case err := <-p.fail:
		return nil, err
	case <-sess.done:
		return nil, ErrOffline
	case <-timer.C:
		return nil, errors.New("the agent did not open the connection in time")
	}
}

// Status returns the live state of an agent.
func (m *Manager) Status(agentID int64) Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[agentID]
	if s == nil {
		return Info{}
	}
	in := s.info
	in.Streams = m.streams[agentID]
	return in
}

// Disconnect drops an agent's session, tunnel ports and physical backups
// (key rotated, agent deleted).
func (m *Manager) Disconnect(agentID int64) {
	m.mu.Lock()
	s := m.sessions[agentID]
	delete(m.sessions, agentID)
	var lns []net.Listener
	for k, tn := range m.tunnels {
		if tn.agentID == agentID {
			lns = append(lns, tn.ln)
			delete(m.tunnels, k)
		}
	}
	var running []*streamConn
	for c, id := range m.active {
		if id == agentID {
			running = append(running, c)
		}
	}
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
	for _, ln := range lns {
		ln.Close()
	}
	for _, c := range running {
		c.Close() // a revoked key also stops its physical backups
	}
}

// Close disconnects every agent (shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	ids := make([]int64, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	for _, tn := range m.tunnels {
		ids = append(ids, tn.agentID)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Disconnect(id)
	}
}
