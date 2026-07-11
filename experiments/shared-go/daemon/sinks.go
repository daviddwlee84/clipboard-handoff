package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// routeSinks fans a received item out to the configured additive sinks
// (SPEC §3), tracking everything it writes for `clear` (SPEC §8). The clipboard
// sink is handled separately by applyAutoCopy; here we handle the folder
// (save_dir) and append-file (text_file) sinks. Never touches the clipboard.
func (d *Daemon) routeSinks(item *ipc.Item) {
	env := &item.Envelope
	s := d.cfg.Get()
	switch env.Type {
	case wire.TypeText:
		if s.TextFile != "" {
			d.appendTextFile(s.TextFile, env)
		}
	case wire.TypeImage:
		if s.SaveDir != "" {
			d.saveToDir(s.SaveDir, imageSinkName(env), env.BlobData)
		}
	case wire.TypeFile:
		if s.SaveDir != "" {
			d.saveToDir(s.SaveDir, fileSinkName(env), env.BlobData)
		}
	}
}

// imageSinkName is "<filename or hash>.png" (SPEC §3): images are always written
// as PNG, so the extension is normalized to .png.
func imageSinkName(env *wire.Envelope) string {
	name := env.Filename
	if name == "" && env.Blob != nil {
		name = env.Blob.Hash
	}
	return strings.TrimSuffix(name, filepath.Ext(name)) + ".png"
}

// fileSinkName is "<filename or hash>" (SPEC §3), keeping the original name.
func fileSinkName(env *wire.Envelope) string {
	if env.Filename != "" {
		return env.Filename
	}
	if env.Blob != nil {
		return env.Blob.Hash
	}
	return "file"
}

// appendTextFile appends a received text item to the text_file sink, preceded by
// a "\n---\n<device> <ISO8601 ts>\n" header (SPEC §3).
func (d *Daemon) appendTextFile(path string, env *wire.Envelope) {
	// Capture the session-start offset before the first append to this path so
	// `clear --all` reverts to exactly here (SPEC §8).
	d.recordTextBaseline(path)

	device := env.DeviceName
	if device == "" {
		device = short(env.Sender)
	}
	ts := time.UnixMilli(int64(env.TS)).UTC().Format(time.RFC3339)
	block := "\n---\n" + device + " " + ts + "\n" + env.Text + "\n"

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("text_file append: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		log.Printf("text_file write: %v", err)
	}
}

// saveToDir writes bytes into the folder sink under name (de-duplicated), and
// records the written path as a this-session write (SPEC §8).
func (d *Daemon) saveToDir(dir, name string, data []byte) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("save_dir mkdir: %v", err)
		return
	}
	path := dedupPath(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("save_dir write: %v", err)
		return
	}
	d.mu.Lock()
	d.sessionSaveFiles = append(d.sessionSaveFiles, path)
	d.mu.Unlock()
	log.Printf("saved to save_dir: %s", path)
}

// dedupPath returns dir/name, or dir/name (2).ext, dir/name (3).ext ... if the
// name is already taken (SPEC §3 "de-duplicated: name (2).ext on collision").
func dedupPath(dir, name string) string {
	if name == "" {
		name = "file"
	}
	candidate := filepath.Join(dir, name)
	if !pathExists(candidate) {
		return candidate
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if !pathExists(c) {
			return c
		}
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// recordTextBaseline captures the size of a text_file path at session start (the
// first time this session observes it), so a later `clear --all` truncates back
// to exactly this offset — never removing content from before the session.
func (d *Daemon) recordTextBaseline(path string) {
	if path == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.textBaseline[path]; ok {
		return
	}
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	d.textBaseline[path] = size
}

// ---- clearing (SPEC §8) ----------------------------------------------------

// clear reverts this session's data. Transient scope is always cleared; when all
// is set, session sink writes are reverted too.
func (d *Daemon) clear(all bool) {
	d.clearTransient()
	if all {
		d.clearSinks()
	}
}

// clearTransient purges the fetched-blob cache + recv --emit-path temp files (the
// daemon-owned blob dir) and the in-memory received-items buffer (SPEC §8).
func (d *Daemon) clearTransient() {
	d.mu.Lock()
	d.buffer = nil
	d.mu.Unlock()

	entries, err := os.ReadDir(d.cfg.BlobDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(d.cfg.BlobDir(), e.Name()))
	}
}

// clearSinks reverts session sink writes (SPEC §8): truncate text_file back to
// its session-start offset and delete the files this session wrote into
// save_dir. Never touches pre-session content or unrelated files.
func (d *Daemon) clearSinks() {
	d.mu.Lock()
	baselines := make(map[string]int64, len(d.textBaseline))
	for k, v := range d.textBaseline {
		baselines[k] = v
	}
	files := d.sessionSaveFiles
	d.sessionSaveFiles = nil
	d.mu.Unlock()

	for path, size := range baselines {
		if err := os.Truncate(path, size); err != nil && !os.IsNotExist(err) {
			log.Printf("clear: truncate %s: %v", path, err)
		}
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			log.Printf("clear: remove %s: %v", f, err)
		}
	}
}

// applyExitPolicy applies clear_on_exit on shutdown (SPEC §8). A resolved policy
// set by `daemon stop` wins; otherwise it reads the config, resolving the
// interactive "ask" to "transient" (a bare daemon has no TTY).
func (d *Daemon) applyExitPolicy() {
	d.mu.Lock()
	policy := d.stopPolicy
	d.mu.Unlock()
	if policy == "" {
		policy = d.cfg.Get().ClearOnExit
	}
	switch policy {
	case "ask", "":
		policy = "transient"
	}
	switch policy {
	case "never":
		return
	case "all":
		d.clear(true)
	default: // transient
		d.clear(false)
	}
	log.Printf("clear_on_exit applied: %s", policy)
}
