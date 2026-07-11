#!/usr/bin/env python3
"""Generate shared test assets (pure stdlib, no PIL) for the bake-off harness.

Writes testdata/{small,large,screenshot}.png and sample.txt. PNGs are 8-bit RGBA
with a deterministic pattern so round-trip tests can compare BLAKE3 hashes.
"""
import os
import struct
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
TESTDATA = os.path.normpath(os.path.join(HERE, "..", "testdata"))


def write_png(path, width, height, rgba_fn):
    def chunk(typ, data):
        body = typ + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    raw = bytearray()
    for y in range(height):
        raw.append(0)  # filter type 0 (None) per scanline
        for x in range(width):
            r, g, b, a = rgba_fn(x, y)
            raw += bytes((r & 255, g & 255, b & 255, a & 255))

    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)  # 8-bit, color type 6 = RGBA
    idat = zlib.compress(bytes(raw), 9)
    with open(path, "wb") as f:
        f.write(sig)
        f.write(chunk(b"IHDR", ihdr))
        f.write(chunk(b"IDAT", idat))
        f.write(chunk(b"IEND", b""))
    print(f"wrote {path} ({width}x{height}, {os.path.getsize(path)} bytes)")


def main():
    os.makedirs(TESTDATA, exist_ok=True)

    # small: 64x64 diagonal gradient
    write_png(os.path.join(TESTDATA, "small.png"), 64, 64,
              lambda x, y: (x * 4, y * 4, (x + y) * 2, 255))

    # large: 256x256 concentric-ish pattern
    write_png(os.path.join(TESTDATA, "large.png"), 256, 256,
              lambda x, y: ((x ^ y) & 255, (x * 3) & 255, (y * 3) & 255, 255))

    # screenshot: 320x200 "window" — border + fill, mimics a captured region
    def shot(x, y):
        if x < 2 or y < 2 or x >= 318 or y >= 198:
            return (40, 44, 52, 255)          # dark border
        if y < 22:
            return (60, 66, 78, 255)          # title bar
        return (245, 246, 248, 255)           # content

    write_png(os.path.join(TESTDATA, "screenshot.png"), 320, 200, shot)

    with open(os.path.join(TESTDATA, "sample.txt"), "w", encoding="utf-8") as f:
        f.write("hello from cross-platform-copy — 跨平台剪貼簿 hand-off 測試 🚀\n")
    print(f"wrote {os.path.join(TESTDATA, 'sample.txt')}")


if __name__ == "__main__":
    main()
