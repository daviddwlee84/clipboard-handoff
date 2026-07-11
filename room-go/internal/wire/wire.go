// Package wire is the transport-agnostic message model shared by the room
// server and the native client daemon: the Envelope (PROTOCOL §1), CBOR
// codec, length-prefixed framing (used both on the SSH channel and the local
// IPC socket), type sniffing (PROTOCOL §2) and BLAKE3 hashing.
package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
	"lukechampine.com/blake3"
)

// Version is the current protocol version (PROTOCOL §1: currently 1).
const Version uint16 = 1

// MaxFrame caps a single framed payload. Phase 0 relays image PNG bytes
// inline through the server, so this must comfortably exceed testdata/large.png
// (~210 KiB). 32 MiB leaves headroom without inviting abuse.
const MaxFrame = 32 << 20

// Blob is the content-addressed reference for an image or file (PROTOCOL §1).
// In Phase 0 the bytes ride inline in Envelope.BlobData; Phase 1 replaces that
// with a server-side pull keyed by Hash. W/H are set for images only.
type Blob struct {
	Hash string `cbor:"hash"` // BLAKE3 hex of the bytes — also the integrity check
	Size uint64 `cbor:"size"` // byte length
	W    uint32 `cbor:"w"`    // image only
	H    uint32 `cbor:"h"`    // image only
}

// Envelope is the on-the-wire message (PROTOCOL §1). Encoded as CBOR. Unknown
// fields are ignored on decode, so a newer field (e.g. filename) stays wire
// back-compatible with an older peer.
type Envelope struct {
	V          uint16 `cbor:"v"`
	MsgID      string `cbor:"msg_id"` // ULID — the dedupe key
	Type       string `cbor:"type"`   // "text" | "image" | "file"
	Mime       string `cbor:"mime"`
	Sender     string `cbor:"sender"` // stable peer identity: SSH public-key fingerprint
	DeviceName string `cbor:"device_name"`
	TS         uint64 `cbor:"ts"`                 // unix milliseconds at the sender
	Filename   string `cbor:"filename,omitempty"` // image/file: original name, advisory (folder/save sinks)

	Text string `cbor:"text,omitempty"` // type=text: inline UTF-8 payload
	Blob *Blob  `cbor:"blob,omitempty"` // type=image|file: content-addressed reference

	// BlobData carries the image/file bytes inline. This is a documented Phase 0
	// simplification (SPEC §7 / task brief allow inline relay of small test
	// blobs). PNG stays the canonical wire format for images (PROTOCOL §1).
	BlobData []byte `cbor:"blob_data,omitempty"`
}

const (
	TypeText  = "text"
	TypeImage = "image"
	TypeFile  = "file"

	MimeText  = "text/plain; charset=utf-8"
	MimePNG   = "image/png"
	MimeJPEG  = "image/jpeg"
	MimeOctet = "application/octet-stream"
)

// Marshal encodes an Envelope to CBOR.
func Marshal(e *Envelope) ([]byte, error) { return cbor.Marshal(e) }

// Unmarshal decodes CBOR into an Envelope.
func Unmarshal(b []byte) (*Envelope, error) {
	var e Envelope
	if err := cbor.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// WriteFrame writes a length-prefixed (4-byte big-endian) payload.
func WriteFrame(w io.Writer, p []byte) error {
	if len(p) > MaxFrame {
		return fmt.Errorf("frame too large: %d > %d", len(p), MaxFrame)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(p)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

// ReadFrame reads one length-prefixed payload written by WriteFrame.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, fmt.Errorf("frame too large: %d > %d", n, MaxFrame)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return nil, err
	}
	return p, nil
}

var (
	pngMagic  = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
)

// Kind is the sniffed content class.
type Kind int

const (
	KindText Kind = iota
	KindPNG
	KindJPEG
)

// Sniff classifies raw stdin bytes (PROTOCOL §2). PNG/JPEG magic → image;
// else valid UTF-8 → text; else an error (caller maps to file, or exit 2).
func Sniff(b []byte) (Kind, error) {
	if bytes.HasPrefix(b, pngMagic) {
		return KindPNG, nil
	}
	if bytes.HasPrefix(b, jpegMagic) {
		return KindJPEG, nil
	}
	if utf8.Valid(b) {
		return KindText, nil
	}
	return 0, errors.New("unrecognized content: not PNG, JPEG, or valid UTF-8")
}

// Classify decides the wire Type for raw bytes under the `--auto` rules
// (PROTOCOL §2): PNG/JPEG magic → image; else `--file` (forceFile) → file;
// else valid UTF-8 → text; else (binary) → file. It never errors — arbitrary
// binary is a valid file.
func Classify(b []byte, forceFile bool) string {
	if bytes.HasPrefix(b, pngMagic) || bytes.HasPrefix(b, jpegMagic) {
		return TypeImage
	}
	if forceFile {
		return TypeFile
	}
	if utf8.Valid(b) {
		return TypeText
	}
	return TypeFile
}

// MimeForFilename returns a best-effort MIME type for a file, guessed from the
// filename extension (PROTOCOL §1: "best-effort for files"), falling back to
// application/octet-stream when unknown.
func MimeForFilename(name string) string {
	if ext := filepath.Ext(name); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			// mime.TypeByExtension may append "; charset=utf-8"; keep it as-is,
			// it is advisory. Trim any trailing whitespace only.
			return strings.TrimSpace(t)
		}
	}
	return MimeOctet
}

// ToPNG normalizes image bytes to the canonical PNG wire format. PNG passes
// through unchanged (byte-for-byte, so the BLAKE3 hash is preserved); JPEG is
// transcoded (PROTOCOL §2: "canonical wire format is PNG").
func ToPNG(kind Kind, b []byte) ([]byte, error) {
	switch kind {
	case KindPNG:
		return b, nil
	case KindJPEG:
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("decode jpeg: %w", err)
		}
		var out bytes.Buffer
		if err := png.Encode(&out, img); err != nil {
			return nil, fmt.Errorf("encode png: %w", err)
		}
		return out.Bytes(), nil
	default:
		return nil, errors.New("not an image")
	}
}

// PNGDims returns the pixel dimensions of PNG bytes without full decode.
func PNGDims(b []byte) (w, h uint32, err error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0, err
	}
	return uint32(cfg.Width), uint32(cfg.Height), nil
}

// HashHex returns the BLAKE3-256 hex digest of b (PROTOCOL §1 integrity check).
func HashHex(b []byte) string {
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}
