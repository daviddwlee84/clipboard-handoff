// Package config owns each probe's on-disk state: the config directory layout,
// persisted settings (SPEC §5) and a home for the transport's identity key/cert
// (whose fingerprint / PeerId is the device identity per PROTOCOL §3). Unlike
// room-go, identity material is transport-specific, so config just hands out a
// directory and file paths; each transport persists its own key there.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Settings are the persisted config keys (SPEC §5). Only the Phase 0 subset is
// acted upon; the rest are stored for forward-compatibility.
type Settings struct {
	AutoCopy        string `json:"auto_copy"`         // notify | on | off
	SaveDir         string `json:"save_dir"`          // folder sink: received image/file items (SPEC §3)
	TextFile        string `json:"text_file"`         // append sink: received text items (SPEC §3)
	ClearOnExit     string `json:"clear_on_exit"`     // ask | transient | all | never (SPEC §8)
	DeviceName      string `json:"device_name"`       // shown to peers
	Room            string `json:"room"`              // default room/topic
	Internet        string `json:"internet"`          // on | off (Phase 3)
	BroadcastOnCopy string `json:"broadcast_on_copy"` // on | off (Phase 3)
}

func defaultSettings() Settings {
	host, _ := os.Hostname()
	if host == "" {
		host = "probe-device"
	}
	return Settings{
		AutoCopy:        "notify",
		SaveDir:         "",
		TextFile:        "",
		ClearOnExit:     "ask",
		DeviceName:      host,
		Room:            "default",
		Internet:        "off",
		BroadcastOnCopy: "off",
	}
}

// Store is a config directory with its settings.
type Store struct {
	Dir string

	mu       sync.Mutex
	settings Settings
}

// Open loads (or initializes) the config directory. bin is the binary name used
// to derive the default directory when dir is empty.
func Open(dir, bin string) (*Store, error) {
	if dir == "" {
		var err error
		dir, err = DefaultDir(bin)
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

// BlobDir is where received image PNGs are materialized for `recv`.
func (s *Store) BlobDir() string { return filepath.Join(s.Dir, "blobs") }

// Path returns a file path inside the config dir, for a transport to persist
// its identity (e.g. "tls_cert.pem", "libp2p.key").
func (s *Store) Path(name string) string { return filepath.Join(s.Dir, name) }

func (s *Store) load() error {
	b, err := os.ReadFile(s.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return s.save() // write defaults on first run
		}
		return err
	}
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
	case "save_dir":
		s.settings.SaveDir = value
	case "text_file":
		s.settings.TextFile = value
	case "clear_on_exit":
		switch value {
		case "ask", "transient", "all", "never":
		default:
			return fmt.Errorf("clear_on_exit must be ask|transient|all|never, got %q", value)
		}
		s.settings.ClearOnExit = value
	case "device_name":
		s.settings.DeviceName = value
	case "room":
		s.settings.Room = value
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
	case "save_dir":
		return cur.SaveDir, nil
	case "text_file":
		return cur.TextFile, nil
	case "clear_on_exit":
		return cur.ClearOnExit, nil
	case "device_name":
		return cur.DeviceName, nil
	case "room":
		return cur.Room, nil
	case "internet":
		return cur.Internet, nil
	case "broadcast_on_copy":
		return cur.BroadcastOnCopy, nil
	default:
		return "", fmt.Errorf("unknown config key %q", key)
	}
}

// DefaultDir resolves the per-OS config dir (SPEC §5): macOS
// ~/Library/Application Support/<bin>, Linux $XDG_CONFIG_HOME/<bin>, etc.
func DefaultDir(bin string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, bin), nil
}
