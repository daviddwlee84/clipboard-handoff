package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/wire"
)

// newTestDaemon builds a daemon wired to a temp config dir, without a transport
// (the sink/clear logic under test never touches the network).
func newTestDaemon(t *testing.T) (*Daemon, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := config.Open(dir, "lan-test")
	if err != nil {
		t.Fatalf("config.Open: %v", err)
	}
	if err := os.MkdirAll(store.BlobDir(), 0o700); err != nil {
		t.Fatalf("mkdir blobdir: %v", err)
	}
	d := &Daemon{
		cfg:          store,
		fp:           "self-fp",
		deviceName:   "tester",
		dedupe:       NewDedupe(64),
		subs:         make(map[chan *ipc.Item]struct{}),
		textBaseline: make(map[string]int64),
		done:         make(chan struct{}),
	}
	return d, store
}

func textItem(text, device string) *ipc.Item {
	return &ipc.Item{Envelope: wire.Envelope{
		Type: wire.TypeText, Text: text, DeviceName: device, Sender: "peer-fp", TS: 1720000000000,
	}}
}

func fileItem(name string, data []byte) *ipc.Item {
	return &ipc.Item{Envelope: wire.Envelope{
		Type: wire.TypeFile, Filename: name, Sender: "peer-fp", TS: 1720000000000,
		Blob: &wire.Blob{Hash: wire.HashHex(data), Size: uint64(len(data))}, BlobData: data,
	}}
}

// TestSaveToDirDedup: two files with the same name land as name + "name (2).ext".
func TestSaveToDirDedup(t *testing.T) {
	d, _ := newTestDaemon(t)
	dir := t.TempDir()

	d.saveToDir(dir, "photo.png", []byte("aaa"))
	d.saveToDir(dir, "photo.png", []byte("bbb"))
	d.saveToDir(dir, "photo.png", []byte("ccc"))

	for _, want := range []string{"photo.png", "photo (2).png", "photo (3).png"} {
		if !pathExists(filepath.Join(dir, want)) {
			t.Errorf("expected de-duplicated file %q to exist", want)
		}
	}
	if len(d.sessionSaveFiles) != 3 {
		t.Fatalf("expected 3 tracked session save files, got %d", len(d.sessionSaveFiles))
	}
}

// TestFileSinkRouting: a received file with save_dir set writes the exact bytes;
// a text item with save_dir set writes nothing (files/images only, SPEC §3).
func TestFileSinkRouting(t *testing.T) {
	d, store := newTestDaemon(t)
	dir := t.TempDir()
	if err := store.SetKey("save_dir", dir); err != nil {
		t.Fatalf("set save_dir: %v", err)
	}

	payload := []byte{0x00, 0x01, 0xff, 'b', 'i', 'n'}
	d.routeSinks(fileItem("report.bin", payload))
	got, err := os.ReadFile(filepath.Join(dir, "report.bin"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("saved file bytes mismatch")
	}

	// A text item must not create a file in save_dir.
	d.routeSinks(textItem("hello", "peerA"))
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("text item should not write to save_dir; dir has %d entries", len(entries))
	}
}

// TestTextAppendAndClearAll: text_file appends carry the header, and clear --all
// truncates back to the session-start offset (pre-session content intact) while
// removing only files this session wrote to save_dir.
func TestTextAppendAndClearAll(t *testing.T) {
	d, store := newTestDaemon(t)

	// Pre-existing content that predates the session must survive clear --all.
	textPath := filepath.Join(t.TempDir(), "log.txt")
	preContent := "PRE-SESSION LINE\n"
	if err := os.WriteFile(textPath, []byte(preContent), 0o644); err != nil {
		t.Fatalf("seed text_file: %v", err)
	}
	saveDir := t.TempDir()
	preFile := filepath.Join(saveDir, "keep.dat")
	if err := os.WriteFile(preFile, []byte("KEEP"), 0o644); err != nil {
		t.Fatalf("seed save_dir file: %v", err)
	}

	if err := store.SetKey("text_file", textPath); err != nil {
		t.Fatalf("set text_file: %v", err)
	}
	if err := store.SetKey("save_dir", saveDir); err != nil {
		t.Fatalf("set save_dir: %v", err)
	}
	// Record the session-start offset (as Run does).
	d.recordTextBaseline(textPath)

	// Receive text (appended) + a file (written to save_dir this session).
	d.routeSinks(textItem("first message", "peerA"))
	d.routeSinks(textItem("second message", "peerB"))
	d.routeSinks(fileItem("session.bin", []byte("session-bytes")))

	afterAppend, _ := os.ReadFile(textPath)
	if len(afterAppend) <= len(preContent) {
		t.Fatalf("text_file did not grow after appends")
	}
	if !strings.Contains(string(afterAppend), "\n---\npeerA ") || !strings.Contains(string(afterAppend), "first message") {
		t.Fatalf("append missing header/body:\n%s", afterAppend)
	}
	if !pathExists(filepath.Join(saveDir, "session.bin")) {
		t.Fatalf("session file not written to save_dir")
	}

	// clear --all reverts session sink writes.
	d.clear(true)

	reverted, err := os.ReadFile(textPath)
	if err != nil {
		t.Fatalf("read text_file after clear: %v", err)
	}
	if string(reverted) != preContent {
		t.Fatalf("clear --all should truncate to session-start offset.\n got: %q\nwant: %q", reverted, preContent)
	}
	if !pathExists(preFile) {
		t.Fatalf("clear --all deleted a pre-session save_dir file")
	}
	if pathExists(filepath.Join(saveDir, "session.bin")) {
		t.Fatalf("clear --all should delete files this session wrote")
	}
}

// TestClearTransientOnly: plain clear purges the blob cache + buffer but leaves
// sink writes (text_file / save_dir) untouched.
func TestClearTransientOnly(t *testing.T) {
	d, store := newTestDaemon(t)
	textPath := filepath.Join(t.TempDir(), "log.txt")
	if err := store.SetKey("text_file", textPath); err != nil {
		t.Fatalf("set text_file: %v", err)
	}
	d.recordTextBaseline(textPath)
	d.routeSinks(textItem("keep me", "peerA"))

	// Seed a transient blob file + a buffer entry.
	blob := filepath.Join(store.BlobDir(), "deadbeef.png")
	if err := os.WriteFile(blob, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	d.buffer = append(d.buffer, textItem("buffered", "peerA"))

	d.clear(false) // transient only

	if pathExists(blob) {
		t.Fatalf("transient clear should remove the blob cache file")
	}
	if len(d.buffer) != 0 {
		t.Fatalf("transient clear should empty the buffer, got %d", len(d.buffer))
	}
	if b, _ := os.ReadFile(textPath); len(b) == 0 {
		t.Fatalf("transient clear must NOT truncate the text_file sink")
	}
}

// TestFilePasteCopiesPathAsText: paste of a file copies its local path as
// clipboard text (SPEC §2), not image bytes.
func TestFilePasteCopiesPathAsText(t *testing.T) {
	item := &ipc.Item{
		Envelope:  wire.Envelope{Type: wire.TypeFile, Filename: "a.bin", Blob: &wire.Blob{Hash: "h", Size: 3}},
		LocalPath: "/tmp/blobs/h.bin",
	}
	isImage, data := clipboardPayload(item)
	if isImage {
		t.Fatalf("a file must not be pasted as an image")
	}
	if string(data) != "/tmp/blobs/h.bin" {
		t.Fatalf("file paste should copy the local path as text, got %q", data)
	}

	// Sanity: an image still pastes its PNG bytes.
	img := &ipc.Item{Envelope: wire.Envelope{Type: wire.TypeImage, Blob: &wire.Blob{Hash: "h"}, BlobData: []byte("PNGDATA")}}
	if isImg, d := clipboardPayload(img); !isImg || string(d) != "PNGDATA" {
		t.Fatalf("image paste should copy PNG bytes, got isImage=%v data=%q", isImg, d)
	}
}
