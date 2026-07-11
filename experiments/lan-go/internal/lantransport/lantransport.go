// Package lantransport is the lan-go probe transport (BAKEOFF question: "is
// iroh's weight worth it, or is a hand-rolled LAN mesh good enough?"). It is a
// zero-config LAN mesh built from two commodity pieces:
//
//   - discovery: mDNS/DNS-SD via github.com/grandcat/zeroconf. Each daemon
//     advertises a "_clip-lan._udp" service whose TXT record carries the room,
//     the device identity fingerprint and the actual QUIC port, and browses for
//     peers advertising the SAME room.
//   - transport: quic-go direct connections secured by a persisted self-signed
//     TLS cert whose SHA-256 fingerprint IS the device identity (PROTOCOL §3,
//     Syncthing-style). Envelopes ride one-per-QUIC-uni-stream; images ride
//     inline in blob_data for Phase 0.
//
// Same-host note: two instances bind distinct ephemeral UDP ports (":0") and
// advertise the real port, so two daemons on one machine discover and connect.
// To avoid a duplicate reciprocal dial, the peer with the lexicographically
// smaller fingerprint initiates the connection; the other accepts.
package lantransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
	"github.com/quic-go/quic-go"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/transport"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

const (
	serviceType = "_clip-lan._udp"
	mdnsDomain  = "local."
	alpn        = "clip-lan/0"
)

// Transport implements transport.Transport over quic-go + mDNS.
type Transport struct {
	room       string
	deviceName string
	fp         string
	cert       tls.Certificate

	serverTLS *tls.Config
	clientTLS *tls.Config
	quicConf  *quic.Config

	ln      *quic.Listener
	port    int
	zserver *zeroconf.Server

	recvFn transport.ReceiveFunc

	mu      sync.Mutex
	peers   map[string]*peerConn // keyed by peer fingerprint
	dialing map[string]bool

	ctx    context.Context
	cancel context.CancelFunc
}

type peerConn struct {
	fp   string
	name string
	addr string
	conn *quic.Conn
}

// New builds the transport, loading (or creating) the persistent TLS identity.
func New(cfg *config.Store, room, deviceName string) (*Transport, error) {
	cert, fp, err := loadOrCreateIdentity(cfg)
	if err != nil {
		return nil, err
	}
	t := &Transport{
		room:       room,
		deviceName: deviceName,
		fp:         fp,
		cert:       cert,
		peers:      make(map[string]*peerConn),
		dialing:    make(map[string]bool),
		serverTLS: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAnyClientCert, // request the peer cert; verify by fp (TOFU)
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{alpn},
		},
		clientTLS: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true, // verify server by fingerprint, not a CA (TOFU)
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{alpn},
		},
		quicConf: &quic.Config{
			MaxIdleTimeout:  60 * time.Second,
			KeepAlivePeriod: 15 * time.Second,
		},
	}
	return t, nil
}

func (t *Transport) Identity() string                   { return t.fp }
func (t *Transport) OnReceive(fn transport.ReceiveFunc) { t.recvFn = fn }

// Start binds the QUIC listener on an ephemeral UDP port, advertises it over
// mDNS, and begins accepting connections and browsing for same-room peers.
func (t *Transport) Start(ctx context.Context) error {
	t.ctx, t.cancel = context.WithCancel(ctx)

	ln, err := quic.ListenAddr(":0", t.serverTLS, t.quicConf)
	if err != nil {
		return fmt.Errorf("quic listen: %w", err)
	}
	t.ln = ln
	t.port = ln.Addr().(*net.UDPAddr).Port

	instance := fmt.Sprintf("clip-%s-%d", t.fp[:8], os.Getpid())
	txt := []string{
		"room=" + t.room,
		"fp=" + t.fp,
		"port=" + strconv.Itoa(t.port),
		"name=" + t.deviceName,
	}
	zs, err := zeroconf.Register(instance, serviceType, mdnsDomain, t.port, txt, nil)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("mdns register: %w", err)
	}
	t.zserver = zs

	log.Printf("lan transport up: quic=:%d fp=%s room=%q instance=%s", t.port, t.fp[:16], t.room, instance)

	go t.acceptLoop()
	go t.browseLoop()
	return nil
}

func (t *Transport) acceptLoop() {
	for {
		conn, err := t.ln.Accept(t.ctx)
		if err != nil {
			return // listener closed / ctx cancelled
		}
		go t.handleConn(conn, true)
	}
}

// browseLoop repeatedly browses for same-room peers. A fresh short-lived
// resolver per round forces re-querying, so peers that appear later (or after a
// reconnect) are rediscovered.
func (t *Transport) browseLoop() {
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}
		t.browseOnce(3 * time.Second)
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
}

func (t *Transport) browseOnce(dur time.Duration) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		log.Printf("lan: mdns resolver: %v", err)
		return
	}
	entries := make(chan *zeroconf.ServiceEntry, 8)
	ctx, cancel := context.WithTimeout(t.ctx, dur)
	defer cancel()
	go func() {
		for e := range entries {
			t.handleEntry(e)
		}
	}()
	if err := resolver.Browse(ctx, serviceType, mdnsDomain, entries); err != nil {
		log.Printf("lan: mdns browse: %v", err)
	}
	<-ctx.Done()
}

// handleEntry inspects a discovered service and, if it is a same-room peer we
// should dial (smaller fingerprint initiates), connects to it.
func (t *Transport) handleEntry(e *zeroconf.ServiceEntry) {
	room, fp, port := parseTXT(e.Text)
	if fp == "" || room != t.room {
		return
	}
	if fp == t.fp {
		return // ourselves
	}
	if port == 0 {
		port = e.Port
	}
	// Only the smaller-fingerprint side dials; the other accepts.
	if t.fp >= fp {
		return
	}

	t.mu.Lock()
	_, connected := t.peers[fp]
	pending := t.dialing[fp]
	if connected || pending {
		t.mu.Unlock()
		return
	}
	t.dialing[fp] = true
	t.mu.Unlock()

	addr := pickAddr(e, port)
	if addr == "" {
		t.clearDialing(fp)
		return
	}
	go t.dial(addr, fp)
}

func (t *Transport) dial(addr, expectFP string) {
	defer t.clearDialing(expectFP)

	ctx, cancel := context.WithTimeout(t.ctx, 8*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, addr, t.clientTLS, t.quicConf)
	if err != nil {
		log.Printf("lan: dial %s failed: %v", addr, err)
		return
	}
	t.handleConn(conn, false)
}

func (t *Transport) clearDialing(fp string) {
	t.mu.Lock()
	delete(t.dialing, fp)
	t.mu.Unlock()
}

// handleConn registers a (dialed or accepted) connection keyed by the peer's
// TLS fingerprint and reads envelopes until it drops.
func (t *Transport) handleConn(conn *quic.Conn, accepted bool) {
	cs := conn.ConnectionState()
	if len(cs.TLS.PeerCertificates) == 0 {
		_ = conn.CloseWithError(0, "no peer certificate")
		return
	}
	peerFP := certFP(cs.TLS.PeerCertificates[0].Raw)
	if peerFP == t.fp {
		_ = conn.CloseWithError(0, "self")
		return
	}

	t.mu.Lock()
	if existing, ok := t.peers[peerFP]; ok && existing.conn != conn {
		// Already connected (reciprocal race): keep the existing one.
		t.mu.Unlock()
		_ = conn.CloseWithError(0, "duplicate")
		return
	}
	t.peers[peerFP] = &peerConn{fp: peerFP, conn: conn, addr: conn.RemoteAddr().String()}
	n := len(t.peers)
	t.mu.Unlock()

	dir := "dialed"
	if accepted {
		dir = "accepted"
	}
	log.Printf("lan: peer %s %s (%s) peers=%d", peerFP[:16], dir, conn.RemoteAddr(), n)

	defer func() {
		t.mu.Lock()
		if cur, ok := t.peers[peerFP]; ok && cur.conn == conn {
			delete(t.peers, peerFP)
		}
		t.mu.Unlock()
		_ = conn.CloseWithError(0, "closed")
		log.Printf("lan: peer %s disconnected", peerFP[:16])
	}()

	for {
		s, err := conn.AcceptUniStream(t.ctx)
		if err != nil {
			return
		}
		go t.readStream(s)
	}
}

func (t *Transport) readStream(s *quic.ReceiveStream) {
	frame, err := wire.ReadFrame(s)
	if err != nil {
		return
	}
	env, err := wire.Unmarshal(frame)
	if err != nil {
		log.Printf("lan: decode envelope: %v", err)
		return
	}
	if t.recvFn != nil {
		t.recvFn(env)
	}
}

// Broadcast opens a fresh uni stream to each peer and writes one framed
// envelope (announcement + inline blob_data for Phase 0 images).
func (t *Transport) Broadcast(env *wire.Envelope) error {
	frame, err := wire.Marshal(env)
	if err != nil {
		return err
	}
	t.mu.Lock()
	conns := make([]*quic.Conn, 0, len(t.peers))
	for _, p := range t.peers {
		conns = append(conns, p.conn)
	}
	t.mu.Unlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
		s, err := c.OpenUniStreamSync(ctx)
		cancel()
		if err != nil {
			log.Printf("lan: open stream: %v", err)
			continue
		}
		if werr := wire.WriteFrame(s, frame); werr != nil {
			log.Printf("lan: write frame: %v", werr)
		}
		_ = s.Close()
	}
	return nil
}

func (t *Transport) Peers() []transport.Peer {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]transport.Peer, 0, len(t.peers))
	for _, p := range t.peers {
		out = append(out, transport.Peer{ID: p.fp, Name: p.name, Addr: p.addr})
	}
	return out
}

func (t *Transport) Close() error {
	if t.cancel != nil {
		t.cancel()
	}
	if t.zserver != nil {
		t.zserver.Shutdown()
	}
	t.mu.Lock()
	for _, p := range t.peers {
		_ = p.conn.CloseWithError(0, "shutdown")
	}
	t.peers = map[string]*peerConn{}
	t.mu.Unlock()
	if t.ln != nil {
		_ = t.ln.Close()
	}
	return nil
}

// ---- identity + helpers ----------------------------------------------------

func certFP(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// loadOrCreateIdentity loads a persisted self-signed cert/key from the config
// dir, or generates one on first run. The SHA-256 of the cert DER is the
// device identity (PROTOCOL §3).
func loadOrCreateIdentity(cfg *config.Store) (tls.Certificate, string, error) {
	certPath := cfg.Path("tls_cert.pem")
	keyPath := cfg.Path("tls_key.pem")

	certPEM, cerr := os.ReadFile(certPath)
	keyPEM, kerr := os.ReadFile(keyPath)
	if cerr == nil && kerr == nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("parse identity cert: %w", err)
		}
		return cert, certFP(cert.Certificate[0]), nil
	}

	certPEM, keyPEM, err := generateSelfSigned()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, certFP(cert.Certificate[0]), nil
}

func generateSelfSigned() (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "clip-lan"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// parseTXT extracts room, fp and port from a service TXT record.
func parseTXT(txt []string) (room, fp string, port int) {
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "room":
			room = v
		case "fp":
			fp = v
		case "port":
			port, _ = strconv.Atoi(v)
		}
	}
	return
}

// pickAddr chooses a dialable host:port from a discovered entry, preferring
// IPv4. On the same host the advertised interface IP loops back fine.
func pickAddr(e *zeroconf.ServiceEntry, port int) string {
	for _, ip := range e.AddrIPv4 {
		if ip4 := ip.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), strconv.Itoa(port))
		}
	}
	for _, ip := range e.AddrIPv6 {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port))
	}
	return ""
}
