// Package config owns the daemon's on-disk state: the config directory
// layout, persisted settings (SPEC §5), and the client's SSH identity key
// (whose fingerprint is the device identity per PROTOCOL §3).
package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

func encodePEM(b *pem.Block) []byte { return pem.EncodeToMemory(b) }

// Settings are the persisted config keys (SPEC §5). Only the Phase 0 subset
// is acted upon; the rest are stored for forward-compatibility.
type Settings struct {
	AutoCopy        string `json:"auto_copy"`         // notify | on | off
	DeviceName      string `json:"device_name"`       // shown to peers
	Room            string `json:"room"`              // default room
	Server          string `json:"server"`            // user@host:port (set by `join`)
	Internet        string `json:"internet"`          // on | off (Phase 3)
	BroadcastOnCopy string `json:"broadcast_on_copy"` // on | off (Phase 3)
}

func defaultSettings() Settings {
	host, _ := os.Hostname()
	if host == "" {
		host = "room-device"
	}
	return Settings{
		AutoCopy:        "notify",
		DeviceName:      host,
		Room:            "default",
		Internet:        "off",
		BroadcastOnCopy: "off",
	}
}

// Store is a config directory with its settings + identity key.
type Store struct {
	Dir string

	mu       sync.Mutex
	settings Settings
}

// Open loads (or initializes) the config directory.
func Open(dir string) (*Store, error) {
	if dir == "" {
		var err error
		dir, err = DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, settings: defaultSettings()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) configPath() string { return filepath.Join(s.Dir, "config.json") }
func (s *Store) keyPath() string    { return filepath.Join(s.Dir, "id_ed25519") }

// BlobDir is where received image PNGs are materialized for `recv`.
func (s *Store) BlobDir() string { return filepath.Join(s.Dir, "blobs") }

func (s *Store) load() error {
	b, err := os.ReadFile(s.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return s.save() // write defaults on first run
		}
		return err
	}
	// Merge onto defaults so missing keys keep their defaults.
	cur := defaultSettings()
	if err := json.Unmarshal(b, &cur); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	s.settings = cur
	return nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.configPath())
}

// Get returns a snapshot of the settings.
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// SetKey updates one setting by key (SPEC §5 `config set`).
func (s *Store) SetKey(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch key {
	case "auto_copy":
		switch value {
		case "notify", "on", "off":
		default:
			return fmt.Errorf("auto_copy must be notify|on|off, got %q", value)
		}
		s.settings.AutoCopy = value
	case "device_name":
		s.settings.DeviceName = value
	case "room":
		s.settings.Room = value
	case "server":
		s.settings.Server = value
	case "internet":
		s.settings.Internet = value
	case "broadcast_on_copy":
		s.settings.BroadcastOnCopy = value
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return s.save()
}

// GetKey returns one setting by key.
func (s *Store) GetKey(key string) (string, error) {
	cur := s.Get()
	switch key {
	case "auto_copy":
		return cur.AutoCopy, nil
	case "device_name":
		return cur.DeviceName, nil
	case "room":
		return cur.Room, nil
	case "server":
		return cur.Server, nil
	case "internet":
		return cur.Internet, nil
	case "broadcast_on_copy":
		return cur.BroadcastOnCopy, nil
	default:
		return "", fmt.Errorf("unknown config key %q", key)
	}
}

// SetServer persists the server target (used by `join`).
func (s *Store) SetServer(target string) error { return s.SetKey("server", target) }

// Signer loads the client's SSH identity key, generating and persisting an
// ed25519 key on first use. Its fingerprint is the device identity.
func (s *Store) Signer() (gossh.Signer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.keyPath())
	if err == nil {
		key, perr := gossh.ParseRawPrivateKey(b)
		if perr != nil {
			return nil, fmt.Errorf("parse identity key: %w", perr)
		}
		return gossh.NewSignerFromKey(key)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	// Generate a fresh ed25519 key and persist it in OpenSSH PEM format.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pem, err := gossh.MarshalPrivateKey(priv, "room-go client key")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.keyPath(), encodePEM(pem), 0o600); err != nil {
		return nil, err
	}
	return gossh.NewSignerFromKey(priv)
}

// Fingerprint returns the SSH public-key fingerprint of the identity key.
func (s *Store) Fingerprint() (string, error) {
	signer, err := s.Signer()
	if err != nil {
		return "", err
	}
	return gossh.FingerprintSHA256(signer.PublicKey()), nil
}

// AuthorizedKeyLine returns the `authorized_keys`-style public-key line to
// allowlist this device on the server.
func (s *Store) AuthorizedKeyLine() (string, error) {
	signer, err := s.Signer()
	if err != nil {
		return "", err
	}
	return string(gossh.MarshalAuthorizedKey(signer.PublicKey())), nil
}

// DefaultDir resolves the per-OS config dir (SPEC §5): macOS
// ~/Library/Application Support/room, Linux $XDG_CONFIG_HOME/room, etc.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "room"), nil
}
