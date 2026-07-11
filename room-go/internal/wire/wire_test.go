package wire

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestEnvelopeCBORRoundTrip_Text(t *testing.T) {
	in := &Envelope{
		V:          Version,
		MsgID:      "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Type:       TypeText,
		Mime:       MimeText,
		Sender:     "SHA256:abc",
		DeviceName: "laptop",
		TS:         1720000000000,
		Text:       "hello, 世界",
	}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Text != in.Text || out.MsgID != in.MsgID || out.Type != in.Type || out.V != in.V {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
	if out.Blob != nil {
		t.Errorf("text envelope decoded with a Blob: %+v", out.Blob)
	}
}

func TestEnvelopeCBORRoundTrip_Image(t *testing.T) {
	png := makePNG(t, 3, 2)
	in := &Envelope{
		V:        Version,
		MsgID:    "01ARZ3NDEKTSV4RRFFQ69G5FB0",
		Type:     TypeImage,
		Mime:     MimePNG,
		Sender:   "SHA256:def",
		TS:       1720000000001,
		Blob:     &Blob{Hash: HashHex(png), Size: uint64(len(png)), W: 3, H: 2},
		BlobData: png,
	}
	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Blob == nil || out.Blob.Hash != in.Blob.Hash || out.Blob.W != 3 || out.Blob.H != 2 {
		t.Fatalf("blob mismatch: %+v", out.Blob)
	}
	if !bytes.Equal(out.BlobData, in.BlobData) {
		t.Fatalf("blob data not preserved across CBOR round-trip")
	}
	// Integrity: hash of decoded bytes must equal the announced hash.
	if HashHex(out.BlobData) != out.Blob.Hash {
		t.Fatalf("integrity check would fail after round-trip")
	}
}

func TestSniff(t *testing.T) {
	pngBytes := makePNG(t, 2, 2)
	jpegBytes := makeJPEG(t, 2, 2)

	cases := []struct {
		name string
		in   []byte
		want Kind
		err  bool
	}{
		{"png", pngBytes, KindPNG, false},
		{"jpeg", jpegBytes, KindJPEG, false},
		{"utf8", []byte("hello world"), KindText, false},
		{"utf8-multibyte", []byte("héllo 世界"), KindText, false},
		{"empty", []byte(""), KindText, false}, // empty is valid UTF-8
		{"invalid", []byte{0xff, 0xfe, 0x00, 0x01}, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Sniff(c.in)
			if c.err {
				if err == nil {
					t.Fatalf("Sniff(%s) = %v, want error", c.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sniff(%s) unexpected error: %v", c.name, err)
			}
			if got != c.want {
				t.Fatalf("Sniff(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestToPNG_PassthroughPreservesBytes(t *testing.T) {
	pngBytes := makePNG(t, 4, 4)
	out, err := ToPNG(KindPNG, pngBytes)
	if err != nil {
		t.Fatalf("ToPNG: %v", err)
	}
	if !bytes.Equal(out, pngBytes) {
		t.Fatalf("PNG passthrough altered bytes (hash would change)")
	}
}

func TestToPNG_TranscodeJPEG(t *testing.T) {
	jpegBytes := makeJPEG(t, 8, 8)
	out, err := ToPNG(KindJPEG, jpegBytes)
	if err != nil {
		t.Fatalf("ToPNG(jpeg): %v", err)
	}
	kind, err := Sniff(out)
	if err != nil || kind != KindPNG {
		t.Fatalf("transcoded output is not PNG: kind=%v err=%v", kind, err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{[]byte("a"), []byte(""), bytes.Repeat([]byte("x"), 5000)}
	for _, p := range payloads {
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	for i, want := range payloads {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame[%d]: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame[%d] = %q, want %q", i, got, want)
		}
	}
}

// helpers

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 30), uint8(y * 30), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}
