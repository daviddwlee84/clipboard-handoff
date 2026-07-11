package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// session records what a single daemon run has fetched/written so `clear`
// (SPEC §8) can revert precisely: the append-file's size at session start (the
// truncation offset) and the list of files written into save_dir this session.
// The transient store (blob cache + in-memory buffer) is cleared wholesale, so
// it needs no per-item tracking here.
type session struct {
	mu sync.Mutex

	textFilePath   string // the text_file sink path this session is tracking
	textFileOffset int64  // its size when it became the sink (bytes before us)
	savedFiles     []string
}

// newSession captures the session-start state for the currently-configured
// text_file (SPEC §8: "the text_file size at session start").
func newSession(textFile string) *session {
	s := &session{}
	if textFile != "" {
		s.textFilePath = textFile
		s.textFileOffset = fileSize(textFile)
	}
	return s
}

// setTextFile re-captures the offset when the text_file sink is (re)configured
// mid-session via `config set text_file`. Switching to a new path records that
// file's current size as the new session-start offset, so a later `clear --all`
// only removes what this session appended.
func (s *session) setTextFile(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == s.textFilePath {
		return
	}
	s.textFilePath = path
	if path == "" {
		s.textFileOffset = 0
		return
	}
	s.textFileOffset = fileSize(path)
}

func (s *session) recordSaved(path string) {
	s.mu.Lock()
	s.savedFiles = append(s.savedFiles, path)
	s.mu.Unlock()
}

// ---- clear (SPEC §8) --------------------------------------------------------

// clearTransient purges the always-safe transient store: the in-memory received
// buffer and the fetched-blob cache (which also holds `recv --emit-path` temp
// files, since those are materialized there).
func (d *Daemon) clearTransient() {
	d.mu.Lock()
	d.buffer = nil
	d.mu.Unlock()

	dir := d.cfg.BlobDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	log.Info("cleared transient store", "buffer", "emptied", "blobs", len(entries))
}

// clearSinks reverts this session's sink writes: truncate text_file back to its
// session-start offset and delete the files written into save_dir this session.
// It never touches pre-session content and never removes a whole directory.
func (d *Daemon) clearSinks() {
	d.sess.mu.Lock()
	path, off := d.sess.textFilePath, d.sess.textFileOffset
	saved := d.sess.savedFiles
	d.sess.savedFiles = nil
	d.sess.mu.Unlock()

	if path != "" {
		if fi, err := os.Stat(path); err == nil && fi.Size() > off {
			if err := os.Truncate(path, off); err != nil {
				log.Warn("clear: truncate text_file", "path", path, "err", err)
			}
		}
	}
	for _, f := range saved {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			log.Warn("clear: remove save_dir file", "path", f, "err", err)
		}
	}
	log.Info("cleared session sinks", "text_file", path, "offset", off, "save_dir_files", len(saved))
}

// applyClearOnExit maps a clear_on_exit policy to a scope and runs it. When
// non-interactive (SIGTERM, or `ask` with no TTY), `ask` falls back to
// transient (SPEC §8).
func (d *Daemon) applyClearOnExit(policy string, interactive bool) {
	scope := policy
	if policy == "ask" && !interactive {
		scope = "transient"
	}
	d.applyClearScope(scope)
}

// applyClearScope runs a concrete clear scope ("never" | "transient" | "all").
func (d *Daemon) applyClearScope(scope string) {
	switch scope {
	case "all":
		d.clearTransient()
		d.clearSinks()
	case "transient":
		d.clearTransient()
	case "never", "":
		// keep everything
	default:
		log.Warn("unknown clear scope; keeping everything", "scope", scope)
	}
}

// triggerStop signals the Run loop to shut the daemon down (idempotent).
func (d *Daemon) triggerStop() {
	d.stopOnce.Do(func() { close(d.stopReq) })
}

// ---- sinks (SPEC §3) --------------------------------------------------------

// routeSinks fans a received item out to the configured folder/append-file
// sinks. It is additive and independent of the clipboard/auto_copy path.
func (d *Daemon) routeSinks(item *ipc.Item) {
	s := d.cfg.Get()
	env := &item.Envelope
	switch env.Type {
	case wire.TypeText:
		if s.TextFile != "" {
			d.appendTextSink(s.TextFile, env)
		}
	case wire.TypeImage:
		if s.SaveDir != "" {
			d.writeSaveDir(s.SaveDir, imageSaveName(env), env.BlobData)
		}
	case wire.TypeFile:
		if s.SaveDir != "" {
			d.writeSaveDir(s.SaveDir, fileSaveName(env), env.BlobData)
		}
	}
}

// writeSaveDir writes data into dir under name, de-duplicating on collision
// (`name (2).ext`), and records the path for `clear --all`.
func (d *Daemon) writeSaveDir(dir, name string, data []byte) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Warn("save_dir: mkdir", "dir", dir, "err", err)
		return
	}
	path := dedupPath(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Warn("save_dir: write", "path", path, "err", err)
		return
	}
	d.sess.recordSaved(path)
	log.Info("wrote to save_dir", "path", path, "bytes", len(data))
}

// appendTextSink appends a received text item to the append-file with the
// SPEC §3 header (`\n---\n<device> <ISO8601 ts>\n<text>\n`).
func (d *Daemon) appendTextSink(path string, env *wire.Envelope) {
	// Make sure the session offset is anchored to this path before we grow it.
	d.sess.setTextFile(path)

	device := env.DeviceName
	if device == "" {
		device = env.Sender
	}
	if device == "" {
		device = "peer"
	}
	ts := time.Now().UTC()
	if env.TS != 0 {
		ts = time.UnixMilli(int64(env.TS)).UTC()
	}
	entry := "\n---\n" + device + " " + ts.Format(time.RFC3339) + "\n" + env.Text + "\n"

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Warn("text_file: open", "path", path, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		log.Warn("text_file: append", "path", path, "err", err)
		return
	}
	log.Info("appended to text_file", "path", path)
}

// ---- small helpers ----------------------------------------------------------

// clipContent computes an item's OS-clipboard representation and the content
// hash recorded for echo suppression (SPEC §3 rule 2). Text and file produce
// clipboard text — a file copies its local path (SPEC §2) — an image produces
// PNG bytes.
func clipContent(item *ipc.Item) (isImage bool, text string, png []byte, hash string) {
	env := &item.Envelope
	switch env.Type {
	case wire.TypeImage:
		return true, "", env.BlobData, blobHash(env)
	case wire.TypeFile:
		return false, item.LocalPath, nil, wire.HashHex([]byte(item.LocalPath))
	default:
		return false, env.Text, nil, wire.HashHex([]byte(env.Text))
	}
}

func blobHash(env *wire.Envelope) string {
	if env.Blob != nil && env.Blob.Hash != "" {
		return env.Blob.Hash
	}
	return wire.HashHex(env.BlobData)
}

// imageSaveName is the save_dir name for an image: its filename if present,
// else <hash>.png.
func imageSaveName(env *wire.Envelope) string {
	if env.Filename != "" {
		return filepath.Base(env.Filename)
	}
	return blobHash(env) + ".png"
}

// fileSaveName is the save_dir name for a file: its filename if present, else
// its content hash.
func fileSaveName(env *wire.Envelope) string {
	if env.Filename != "" {
		return filepath.Base(env.Filename)
	}
	return blobHash(env)
}

// fileLabel is a short human label for a file item (notify/preview).
func fileLabel(env *wire.Envelope) string {
	if env.Filename != "" {
		return filepath.Base(env.Filename)
	}
	return blobHash(env)
}

// fileExt returns the extension (including the dot) of a filename, or "".
func fileExt(name string) string { return filepath.Ext(name) }

// dedupPath returns dir/name, or dir/name (2).ext, dir/name (3).ext, … on
// collision.
func dedupPath(dir, name string) string {
	cand := filepath.Join(dir, name)
	if !exists(cand) {
		return cand
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand = filepath.Join(dir, base+" ("+strconv.Itoa(i)+")"+ext)
		if !exists(cand) {
			return cand
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
