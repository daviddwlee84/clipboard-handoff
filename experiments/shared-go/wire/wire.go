// Package wire is the transport-agnostic message model shared by the
// experiments-track probes: the Envelope (PROTOCOL §1), a CBOR codec,
// length-prefixed framing (reused on the local IPC socket), type sniffing
// (PROTOCOL §2) and BLAKE3 hashing. It is deliberately identical in shape to
// room-go's wire package so the bake-off compares like with like, but lives in
// its own module (probes may not import room-go's internal packages).
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
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
	"lukechampine.com/blake3"
)

// Version is the current protocol version (PROTOCOL §1: currently 1).
const Version uint16 = 1

// MaxFrame caps a single framed payload. Phase 0 rides image PNG bytes inline
// in Envelope.BlobData, so this must comfortably exceed testdata/large.png
// (~210 KiB). 32 MiB leaves headroom without inviting abuse.
const MaxFrame = 32 << 20

// Blob is the content-addressed reference for an image (PROTOCOL §1). In
// Phase 0 the PNG bytes ride inline in Envelope.BlobData; announce+pull is the
// Phase 1 target.
type Blob struct {
	Hash string `cbor:"hash"` // BLAKE3 hex of the PNG bytes — also the integrity check
	Size uint64 `cbor:"size"` // PNG byte length
	W    uint32 `cbor:"w"`
	H    uint32 `cbor:"h"`
}

// Envelope is the on-the-wire message (PROTOCOL §1). Encoded as CBOR.
type Envelope struct {
	V          uint16 `cbor:"v"`
	MsgID      string `cbor:"msg_id"` // ULID — the dedupe key
	Type       string `cbor:"type"`   // "text" | "image"
	Mime       string `cbor:"mime"`
	Sender     string `cbor:"sender"` // stable peer identity: TLS cert fp / libp2p PeerId
	DeviceName string `cbor:"device_name"`
	TS         uint64 `cbor:"ts"` // unix milliseconds at the sender

	Text string `cbor:"text,omitempty"` // type=text: inline UTF-8 payload
	Blob *Blob  `cbor:"blob,omitempty"` // type=image: content-addressed reference

	// BlobData carries the PNG bytes inline. This is the documented Phase 0
	// inline extension (PROTOCOL §1 "blob_data"). PNG is the canonical wire
	// format regardless and blob.hash is still verified on receipt.
	BlobData []byte `cbor:"blob_data,omitempty"`
}

const (
	TypeText  = "text"
	TypeImage = "image"

	MimeText = "text/plain; charset=utf-8"
	MimePNG  = "image/png"
	MimeJPEG = "image/jpeg"
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
// else valid UTF-8 → text; else an error (caller maps to exit code 2).
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

// PNGDims returns the pixel dimensions of PNG bytes without a full decode.
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
