// Package server hosts rooms over SSH using charmbracelet/wish +
// charmbracelet/ssh with public-key auth. This is room-go's differentiator:
// a device is authorized by its SSH key (PROTOCOL §3) — no pairing protocol.
//
// Unlike sshbbs (which serves a bubbletea TUI via the wish/bubbletea
// middleware), the native-client tier here uses a *raw* SSH session as a byte
// pipe: the client execs the room name as the SSH command, then both sides
// exchange length-prefixed CBOR envelopes (wire.WriteFrame/ReadFrame) over the
// channel. The server is content-agnostic — it relays opaque frames between
// the sessions of a room via the broker.
package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	gossh "golang.org/x/crypto/ssh"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/broker"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

const ctxKeyFP = "room.fp"

// Config configures the room server.
type Config struct {
	Addr    string
	HostKey string
	// AuthorizedKeys, when non-empty, is the set of SSH public-key
	// fingerprints (ssh.FingerprintSHA256) allowed to connect. Empty means
	// trust-all (Phase 0 default): any key connects, and its fingerprint is
	// still captured as the client's identity. Documented in README.
	AuthorizedKeys map[string]bool
}

// New builds the wish SSH server.
func New(cfg Config, b *broker.Broker) (*ssh.Server, error) {
	pemBytes, err := hostKeyPEM(cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	return wish.NewServer(
		wish.WithAddress(cfg.Addr),
		wish.WithHostKeyPEM(pemBytes), // load/generate ourselves; avoids keygen chmod of a shared dir like /tmp
		wish.WithPublicKeyAuth(publicKeyAuth(cfg.AuthorizedKeys)),
		wish.WithMiddleware(
			relayMiddleware(b),
		),
	)
}

// hostKeyPEM loads the OpenSSH host key at path, generating a fresh ed25519 key
// (written 0600) if it is missing. It creates any missing parent directories but
// never chmods a pre-existing one — so a key path under a shared dir such as
// /tmp works for an unprivileged user. (wish.WithHostKeyPath delegates to
// charmbracelet/keygen, which chmods the key's parent dir and thus fails with
// EPERM on /tmp.)
func hostKeyPEM(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return pemBytes, nil
}

// publicKeyAuth authorizes a connecting device by its SSH public key and
// stashes the fingerprint as the session identity.
func publicKeyAuth(allowed map[string]bool) ssh.PublicKeyHandler {
	return func(ctx ssh.Context, key ssh.PublicKey) bool {
		fp := gossh.FingerprintSHA256(key)
		if len(allowed) > 0 && !allowed[fp] {
			log.Warn("rejecting unauthorized key", "fp", fp, "user", ctx.User())
			return false
		}
		ctx.SetValue(ctxKeyFP, fp)
		return true
	}
}

// relayMiddleware is the innermost handler: it joins the session to a room and
// relays frames. wish middleware wraps handlers, so we ignore `next` (this is
// the terminal behavior for the native-client tier).
func relayMiddleware(b *broker.Broker) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(sess ssh.Session) {
			room := "default"
			if cmd := sess.Command(); len(cmd) > 0 && cmd[0] != "" {
				room = cmd[0]
			}
			fp, _ := sess.Context().Value(ctxKeyFP).(string)

			s := &broker.Session{Room: room, FP: fp, Out: make(chan []byte, 256)}
			b.Register(s)
			log.Info("session joined", "room", room, "fp", short(fp), "peers", b.RoomCount(room))
			defer func() {
				b.Unregister(s)
				log.Info("session left", "room", room, "fp", short(fp), "peers", b.RoomCount(room))
			}()

			// Writer goroutine: drain the outbound queue to the SSH channel.
			done := make(chan struct{})
			go func() {
				defer close(done)
				for frame := range s.Out {
					if err := wire.WriteFrame(sess, frame); err != nil {
						return
					}
				}
			}()

			// Reader loop: read framed envelopes from the client and fan them
			// out to the room's other sessions. The server never decodes the
			// envelope — it relays opaque frames.
			for {
				frame, err := wire.ReadFrame(sess)
				if err != nil {
					if err != io.EOF {
						log.Debug("read frame", "err", err)
					}
					break
				}
				b.Broadcast(room, s, frame)
			}

			close(s.Out)
			<-done
		}
	}
}

func short(fp string) string {
	if len(fp) > 20 {
		return fp[:20] + "…"
	}
	return fp
}
