package daemon

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	gossh "golang.org/x/crypto/ssh"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// conn holds the daemon's persistent SSH connection to the room server and the
// open session that carries framed envelopes. Guarded by mu.
type conn struct {
	mu        sync.Mutex
	client    *gossh.Client
	session   *gossh.Session
	in        io.WriteCloser // frames -> server
	target    string
	connected bool
}

// parseTarget splits user@host:port. Defaults: user "room", port 2222.
func parseTarget(t string) (user, addr string) {
	user = "room"
	hostport := t
	if i := strings.LastIndex(t, "@"); i >= 0 {
		user = t[:i]
		hostport = t[i+1:]
	}
	if !strings.Contains(hostport, ":") {
		hostport += ":2222"
	}
	return user, hostport
}

// connectLoop keeps a session to target alive, reconnecting on drop until the
// daemon stops. Each successful session runs recvLoop, which returns when the
// connection breaks; we then back off and redial.
func (d *Daemon) connectLoop(target string) {
	for {
		select {
		case <-d.done:
			return
		default:
		}
		if err := d.dialOnce(target); err != nil {
			log.Warn("connect to server failed", "target", target, "err", err)
		}
		// dialOnce blocks in recvLoop until the connection drops.
		select {
		case <-d.done:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Daemon) dialOnce(target string) error {
	user, addr := parseTarget(target)
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(d.signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), // Phase 0: localhost; host-key pinning is Phase 1
		Timeout:         10 * time.Second,
	}
	client, err := gossh.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return err
	}
	// Exec the room name as the SSH command; the server reads it via
	// sess.Command() and joins us to that room.
	if err := sess.Start(d.room); err != nil {
		sess.Close()
		client.Close()
		return err
	}

	d.conn.mu.Lock()
	d.conn.client = client
	d.conn.session = sess
	d.conn.in = stdin
	d.conn.target = target
	d.conn.connected = true
	d.conn.mu.Unlock()
	log.Info("connected to server", "target", target, "room", d.room)

	d.recvLoop(stdout) // blocks until the connection drops

	d.conn.mu.Lock()
	d.conn.connected = false
	d.conn.client = nil
	d.conn.session = nil
	d.conn.in = nil
	d.conn.mu.Unlock()
	sess.Close()
	client.Close()
	log.Info("disconnected from server", "target", target)
	return nil
}

// recvLoop reads framed envelopes from the server and ingests them.
func (d *Daemon) recvLoop(r io.Reader) {
	for {
		frame, err := wire.ReadFrame(r)
		if err != nil {
			return
		}
		env, err := wire.Unmarshal(frame)
		if err != nil {
			log.Warn("decode envelope", "err", err)
			continue
		}
		d.ingest(env)
	}
}

// sendFrame writes an envelope to the server over the live session.
func (d *Daemon) sendFrame(frame []byte) error {
	d.conn.mu.Lock()
	in, ok := d.conn.in, d.conn.connected
	d.conn.mu.Unlock()
	if !ok || in == nil {
		return fmt.Errorf("not connected to server")
	}
	return wire.WriteFrame(in, frame)
}

func (d *Daemon) isConnected() bool {
	d.conn.mu.Lock()
	defer d.conn.mu.Unlock()
	return d.conn.connected
}

func (d *Daemon) serverTarget() string {
	d.conn.mu.Lock()
	defer d.conn.mu.Unlock()
	return d.conn.target
}
