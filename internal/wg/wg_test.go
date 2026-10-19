package wg

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func freeUDP(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestKeys(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseKey(k.String())
	if err != nil || p != k {
		t.Fatalf("round trip: %v", err)
	}
	if k.Public() == k.Public().Public() || k.Public().IsZero() {
		t.Fatal("public key")
	}
	if _, err := ParseKey("nope"); err == nil {
		t.Fatal("invalid key accepted")
	}
}

// peerHost is a database host running standard WireGuard (here: wireguard-go
// with its own network stack) and a TCP echo "database" on 10.66.0.2:3306.
func peerHost(t *testing.T, priv, server, psk Key, serverPort int) *device.Device {
	t.Helper()
	tdev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.66.0.2")}, nil, mtu)
	if err != nil {
		t.Fatal(err)
	}
	dev := device.NewDevice(tdev, conn.NewStdNetBind(), device.NewLogger(device.LogLevelSilent, ""))
	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\npublic_key=%s\npreshared_key=%s\nendpoint=127.0.0.1:%d\nallowed_ip=10.66.0.1/32\npersistent_keepalive_interval=1\n",
		priv.hex(), freeUDP(t), server.hex(), psk.hex(), serverPort)
	if err := dev.IpcSet(uapi); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	ln, err := tnet.ListenTCP(&net.TCPAddr{IP: net.ParseIP("10.66.0.2"), Port: 3306})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); dev.Close() })
	return dev
}

func TestTunnel(t *testing.T) {
	serverKey, _ := GenerateKey()
	peerKey, _ := GenerateKey()
	psk, _ := GeneratePSK()
	port := freeUDP(t)
	m := NewManager()
	m.Logf = t.Logf
	defer m.Close()

	if _, _, err := m.Endpoint(1, "10.66.0.2:3306"); err != ErrDisabled {
		t.Fatalf("disabled: %v", err)
	}
	cfg := &Config{ListenPort: port, Address: netip.MustParsePrefix(DefaultAddress), PrivateKey: serverKey,
		Peers: []Peer{{ID: 1, Name: "db-01", PublicKey: peerKey.Public(), PresharedKey: psk, AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.66.0.2/32")}}}}
	if err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if !m.State().Running {
		t.Fatal("not running")
	}
	peerHost(t, peerKey, serverKey.Public(), psk, port)

	host, lport, err := m.Endpoint(1, "10.66.0.2:3306")
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(lport)))
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 256*1024)
	_, _ = rand.Read(payload)
	go func() { _, _ = c.Write(payload) }()
	got := make([]byte, len(payload))
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("data differs")
	}
	st := m.Status()[1]
	if !st.Online || st.Endpoint == "" || st.RxBytes == 0 {
		t.Fatalf("status: %+v", st)
	}

	// an address that is not routed to the peer
	if _, _, err := m.Endpoint(1, "10.66.0.9:3306"); err == nil || !strings.Contains(err.Error(), "not routed") {
		t.Fatalf("unrouted: %v", err)
	}
	if _, _, err := m.Endpoint(1, "db.example.com:3306"); err == nil {
		t.Fatal("host names cannot be dialed through WireGuard")
	}

	// nothing listens on 3307: the client gets a MySQL error packet with the reason
	host, lport, err = m.Endpoint(1, "10.66.0.2:3307")
	if err != nil {
		t.Fatal(err)
	}
	c, err = net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(lport)))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	pkt, _ := io.ReadAll(c)
	c.Close()
	if len(pkt) < 5 || pkt[4] != 0xff || !bytes.Contains(pkt, []byte("nothing listens on")) {
		t.Fatalf("closed port: %q", pkt)
	}

	echo := func(m *Manager, what string) {
		t.Helper()
		host, lport, err := m.Endpoint(1, "10.66.0.2:3306")
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		c, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(lport)))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("%s: %q %v", what, buf, err)
		}
	}
	// adding a peer keeps the session of the others
	other, _ := GenerateKey()
	cfgMore := *cfg
	cfgMore.Peers = append(append([]Peer{}, cfg.Peers...), Peer{ID: 2, Name: "db-02", PublicKey: other.Public(), AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.66.0.3/32")}})
	if err := m.Apply(&cfgMore); err != nil {
		t.Fatal(err)
	}
	echo(m, "after adding a peer")
	// turned off and on: DumpCat knows where the peer is and handshakes at once
	// (the peer alone would wait for its session to expire, up to two minutes)
	reported := make(chan string, 4)
	m.OnEndpoint = func(id int64, ep string) { reported <- ep }
	if err := m.Apply(nil); err != nil {
		t.Fatal(err)
	}
	var seen string
	select {
	case seen = <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint not reported")
	}
	if err := m.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	echo(m, "after a restart")
	// a new process (DumpCat restarted) uses the endpoint kept in the database;
	// handshakes carry a timestamp rounded to ~16ms that must increase, so do
	// not restart faster than a real process would
	m.Close()
	time.Sleep(100 * time.Millisecond)
	m2 := NewManager()
	defer m2.Close()
	cfgSaved := *cfg
	cfgSaved.Peers = []Peer{cfg.Peers[0]}
	cfgSaved.Peers[0].Endpoint = seen
	if err := m2.Apply(&cfgSaved); err != nil {
		t.Fatal(err)
	}
	echo(m2, "in a new process")
	m = m2

	// peers are updated without a restart; a removed peer is no longer reachable
	cfg2 := *cfg
	cfg2.Peers = nil
	if err := m.Apply(&cfg2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Endpoint(1, "10.66.0.2:3306"); err == nil {
		t.Fatal("removed peer still reachable")
	}
	if err := m.Apply(nil); err != nil || m.State().Running {
		t.Fatalf("stop: %v", err)
	}
}

func TestPeerConfig(t *testing.T) {
	k, _ := GenerateKey()
	s, _ := GenerateKey()
	cfg := PeerConfig("db-01", k, netip.MustParseAddr("10.66.0.2"), s.Public(), Key{}, "backup.example.com:51820", netip.MustParseAddr("10.66.0.1"))
	for _, want := range []string{"PrivateKey = " + k.String(), "Address = 10.66.0.2/32", "PublicKey = " + s.Public().String(),
		"Endpoint = backup.example.com:51820", "AllowedIPs = 10.66.0.1/32", "PersistentKeepalive = 25"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q in\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "PresharedKey") {
		t.Error("empty preshared key written")
	}
}
