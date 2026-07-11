package daemon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/config"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/wire"
)

// newTestDaemon builds a daemon backed by a temp config dir with the given
// sink settings, without connecting to any server. text_file is configured
// before New so the session-start offset is captured from any pre-existing
// content (SPEC §8).
func newTestDaemon(t *testing.T, saveDir, textFile string) (*Daemon, *config.Store) {
	t.Helper()
	cfgDir := t.TempDir()
	store, err := config.Open(cfgDir)
	if err != nil {
		t.Fatalf("config.Open: %v", err)
	}
	if err := store.SetKey("auto_copy", "off"); err != nil {
		t.Fatalf("set auto_copy: %v", err)
	}
	if saveDir != "" {
		if err := store.SetKey("save_dir", saveDir); err != nil {
			t.Fatalf("set save_dir: %v", err)
		}
	}
	if textFile != "" {
		if err := store.SetKey("text_file", textFile); err != nil {
			t.Fatalf("set text_file: %v", err)
		}
	}
	if err := os.MkdirAll(store.BlobDir(), 0o700); err != nil {
		t.Fatalf("mkdir blobdir: %v", err)
	}
	d, err := New(store, filepath.Join(cfgDir, "d.sock"), "test")
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return d, store
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 20), uint8(y * 20), 64, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func textEnv(id, text, device string) *wire.Envelope {
	return &wire.Envelope{V: wire.Version, MsgID: id, Type: wire.TypeText, Mime: wire.MimeText, Text: text, DeviceName: device, TS: 1720000000000}
}

func imageEnv(id string, png []byte, filename string) *wire.Envelope {
	return &wire.Envelope{V: wire.Version, MsgID: id, Type: wire.TypeImage, Mime: wire.MimePNG, Filename: filename,
		Blob: &wire.Blob{Hash: wire.HashHex(png), Size: uint64(len(png)), W: 2, H: 2}, BlobData: png}
}

func fileEnv(id string, data []byte, filename string) *wire.Envelope {
	return &wire.Envelope{V: wire.Version, MsgID: id, Type: wire.TypeFile, Mime: wire.MimeOctet, Filename: filename,
		Blob: &wire.Blob{Hash: wire.HashHex(data), Size: uint64(len(data))}, BlobData: data}
}

// Text items append to text_file with the SPEC §3 header; image/file items land
// in save_dir under their filename, de-duplicated on collision.
func TestSinks_TextAppendImageAndFileToSaveDir(t *testing.T) {
	tmp := t.TempDir()
	saveDir := filepath.Join(tmp, "save")
	textFile := filepath.Join(tmp, "log.txt")

	// Pre-session content: a line already in text_file and a file already in
	// save_dir. Both must survive `clear --all`.
	if err := os.WriteFile(textFile, []byte("PRE-EXISTING\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saveDir, "keep.dat"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, _ := newTestDaemon(t, saveDir, textFile)

	// text → append with header
	d.ingest(textEnv("m1", "hello sinks", "peerA"))

	got, _ := os.ReadFile(textFile)
	s := string(got)
	if !strings.HasPrefix(s, "PRE-EXISTING\n") {
		t.Fatalf("pre-session content lost: %q", s)
	}
	if !strings.Contains(s, "\n---\n") || !strings.Contains(s, "peerA") || !strings.Contains(s, "hello sinks") {
		t.Fatalf("append header/text missing: %q", s)
	}

	// image → save_dir/<filename>
	pngBytes := testPNG(t, 2, 2)
	d.ingest(imageEnv("m2", pngBytes, "shot.png"))
	assertFileBytes(t, filepath.Join(saveDir, "shot.png"), pngBytes)

	// image with same name → dedup "shot (2).png"
	d.ingest(imageEnv("m3", pngBytes, "shot.png"))
	assertFileBytes(t, filepath.Join(saveDir, "shot (2).png"), pngBytes)

	// file → save_dir/<filename>
	data := []byte{0x00, 0x01, 'b', 'i', 'n', 0xff}
	d.ingest(fileEnv("m4", data, "report.bin"))
	assertFileBytes(t, filepath.Join(saveDir, "report.bin"), data)

	// file with same name → dedup "report (2).bin"
	d.ingest(fileEnv("m5", data, "report.bin"))
	assertFileBytes(t, filepath.Join(saveDir, "report (2).bin"), data)
}

// clear --all truncates text_file back to the session-start offset and deletes
// only the files written this session; pre-session content is untouched.
func TestClearAll_RevertsSessionSinksOnly(t *testing.T) {
	tmp := t.TempDir()
	saveDir := filepath.Join(tmp, "save")
	textFile := filepath.Join(tmp, "log.txt")

	pre := "PRE-EXISTING LINE\n"
	if err := os.WriteFile(textFile, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	preFile := filepath.Join(saveDir, "keep.dat")
	if err := os.WriteFile(preFile, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, _ := newTestDaemon(t, saveDir, textFile)

	d.ingest(textEnv("m1", "session text", "peerA"))
	d.ingest(fileEnv("m2", []byte("session-bytes"), "a.bin"))
	d.ingest(imageEnv("m3", testPNG(t, 2, 2), "b.png"))

	// Session writes are present before the clear.
	if got, _ := os.ReadFile(textFile); string(got) == pre {
		t.Fatalf("text_file was not appended this session")
	}
	if !exists(filepath.Join(saveDir, "a.bin")) || !exists(filepath.Join(saveDir, "b.png")) {
		t.Fatalf("save_dir session files missing before clear")
	}

	// clear --all → transient + session sinks.
	d.applyClearScope("all")

	// text_file reverted to exactly the pre-session content.
	got, _ := os.ReadFile(textFile)
	if string(got) != pre {
		t.Fatalf("text_file not reverted to session-start offset: %q, want %q", string(got), pre)
	}
	// Session save_dir files removed.
	if exists(filepath.Join(saveDir, "a.bin")) || exists(filepath.Join(saveDir, "b.png")) {
		t.Fatalf("session save_dir files not removed by clear --all")
	}
	// Pre-session file untouched.
	if !exists(preFile) {
		t.Fatalf("pre-session save_dir file was wrongly deleted")
	}
	if b, _ := os.ReadFile(preFile); string(b) != "keep me" {
		t.Fatalf("pre-session file content changed")
	}
}

// A file item copies its local path onto the clipboard as text (SPEC §2); text
// copies its text; an image copies PNG bytes.
func TestClipContent_TypeRouting(t *testing.T) {
	fileItem := &ipc.Item{
		Envelope:  wire.Envelope{Type: wire.TypeFile, Filename: "r.bin", Blob: &wire.Blob{Hash: "deadbeef"}},
		LocalPath: "/tmp/room-blobs/deadbeef.bin",
	}
	isImage, text, png, hash := clipContent(fileItem)
	if isImage || png != nil {
		t.Fatalf("file should not be an image clipboard payload")
	}
	if text != fileItem.LocalPath {
		t.Fatalf("file clipboard text = %q, want its path %q", text, fileItem.LocalPath)
	}
	if hash != wire.HashHex([]byte(fileItem.LocalPath)) {
		t.Fatalf("file echo-suppression hash mismatch")
	}

	textItem := &ipc.Item{Envelope: wire.Envelope{Type: wire.TypeText, Text: "hi"}}
	if isImg, txt, _, _ := clipContent(textItem); isImg || txt != "hi" {
		t.Fatalf("text clipContent = (%v, %q)", isImg, txt)
	}

	pngBytes := testPNG(t, 2, 2)
	imgItem := &ipc.Item{Envelope: wire.Envelope{Type: wire.TypeImage, Blob: &wire.Blob{Hash: wire.HashHex(pngBytes)}, BlobData: pngBytes}}
	if isImg, _, gotPNG, _ := clipContent(imgItem); !isImg || !bytes.Equal(gotPNG, pngBytes) {
		t.Fatalf("image clipContent did not return PNG bytes")
	}
}

// Setting text_file mid-session (config set text_file) anchors the truncation
// offset to that file's current size, so clear --all only removes this session's
// appended lines.
func TestSession_ConfigSetTextFileOffset(t *testing.T) {
	tmp := t.TempDir()
	textFile := filepath.Join(tmp, "log.txt")
	pre := "OLD CONTENT\n"
	if err := os.WriteFile(textFile, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	// Daemon starts with no text_file configured.
	d, store := newTestDaemon(t, "", "")

	// Simulate `config set text_file <path>` (cfg + session offset capture).
	if err := store.SetKey("text_file", textFile); err != nil {
		t.Fatal(err)
	}
	d.sess.setTextFile(textFile)

	d.ingest(textEnv("m1", "appended this session", "peerZ"))
	if got, _ := os.ReadFile(textFile); string(got) == pre {
		t.Fatalf("append did not happen")
	}

	d.applyClearScope("all")
	if got, _ := os.ReadFile(textFile); string(got) != pre {
		t.Fatalf("clear --all did not revert to config-set offset: %q", string(got))
	}
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: bytes mismatch (%d vs %d)", path, len(got), len(want))
	}
}
