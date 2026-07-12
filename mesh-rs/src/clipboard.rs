//! A small, **fallible** wrapper around the arboard OS clipboard.
//!
//! On a headless Linux box (no X11/Wayland display) arboard cannot open a display and
//! every clipboard op returns `Err`. This wrapper turns those into ordinary errors so the
//! daemon degrades gracefully instead of panicking: `send`, `recv`/`--emit-path`, gossip,
//! and the daemon itself keep working; only `paste` / auto-copy become clear no-ops.
//!
//! The daemon probes availability **once** at startup (`probe_available`) and gates all
//! clipboard writes on the result; `write` re-checks so nothing can ever panic.

use std::borrow::Cow;

use anyhow::{Context, Result, anyhow, bail};

/// Set (to anything non-empty other than `0`) to simulate a headless host where the OS
/// clipboard is unavailable — the probe reports `unavailable` and every write is a no-op
/// error, exactly as on a real display-less Linux server. Lets us exercise the degradation
/// path on macOS (and in a unit test) without an actual headless box.
pub const FORCE_HEADLESS_ENV: &str = "CLIP_FORCE_HEADLESS";

/// True if the caller asked us to pretend the clipboard is unavailable.
pub fn force_headless() -> bool {
    match std::env::var_os(FORCE_HEADLESS_ENV) {
        Some(v) => !v.is_empty() && v != "0",
        None => false,
    }
}

/// Probe **once** whether the OS clipboard can be opened. Never panics.
///
/// Returns `false` on a headless host (arboard init fails) or when `CLIP_FORCE_HEADLESS`
/// is set. Meant to be run inside `spawn_blocking` at daemon startup.
pub fn probe_available() -> bool {
    if force_headless() {
        return false;
    }
    arboard::Clipboard::new().is_ok()
}

/// What to place on the clipboard.
pub enum Payload {
    Text(String),
    /// Canonical PNG bytes; decoded to RGBA for arboard's `set_image`.
    ImagePng(Vec<u8>),
}

/// Read an image off the OS clipboard as PNG bytes: arboard `get_image()` → RGBA → PNG-encode.
/// Returns `(png, w, h)`, or `None` when there is no image, the host is headless/forced, or any
/// step fails (never panics). Blocking + arboard's `Clipboard` isn't `Send`, so run the whole op
/// inside `spawn_blocking` at the call site.
pub fn read_image() -> Option<(Vec<u8>, u32, u32)> {
    if force_headless() {
        return None;
    }
    let mut cb = arboard::Clipboard::new().ok()?;
    let img = cb.get_image().ok()?;
    let (w, h) = (img.width as u32, img.height as u32);
    let rgba = image::RgbaImage::from_raw(w, h, img.bytes.into_owned())?;
    let mut png = Vec::new();
    image::DynamicImage::ImageRgba8(rgba)
        .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
        .ok()?;
    Some((png, w, h))
}

/// Write `payload` to the OS clipboard. Returns `Err` (never panics) when the clipboard is
/// unavailable (headless / forced) or the op fails. Blocking; call from `spawn_blocking`.
pub fn write(payload: Payload) -> Result<()> {
    if force_headless() {
        bail!("clipboard unavailable (headless)");
    }
    let mut cb =
        arboard::Clipboard::new().map_err(|e| anyhow!("clipboard unavailable: {e}"))?;
    match payload {
        Payload::Text(t) => cb.set_text(t).map_err(|e| anyhow!("set_text: {e}"))?,
        Payload::ImagePng(png) => {
            let img = image::load_from_memory(&png).context("decode png")?.to_rgba8();
            let data = arboard::ImageData {
                width: img.width() as usize,
                height: img.height() as usize,
                bytes: Cow::Owned(img.into_raw()),
            };
            cb.set_image(data).map_err(|e| anyhow!("set_image: {e}"))?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The wrapper must return `Err` (not panic) when the clipboard is unavailable — this is
    /// the exact code path a headless Linux daemon takes for `paste` / auto-copy.
    #[test]
    fn forced_headless_degrades_to_error() {
        // Safety: single-threaded test; we set + unset the override around the assertions.
        unsafe { std::env::set_var(FORCE_HEADLESS_ENV, "1") };
        assert!(!probe_available(), "probe must report unavailable when forced headless");
        let err = write(Payload::Text("hi".into())).unwrap_err();
        assert!(
            err.to_string().contains("clipboard unavailable"),
            "expected a clear 'clipboard unavailable' error, got: {err}"
        );
        unsafe { std::env::remove_var(FORCE_HEADLESS_ENV) };
    }

    /// Round-trips a real image through the OS clipboard: write PNG → read it back with
    /// `read_image` → decode. Touches the live clipboard, so it is `#[ignore]`d (run manually
    /// with `cargo test -- --ignored read_image_round_trips`). Confirms Feature B's daemon-side
    /// read produces a valid PNG of the right dimensions from whatever is on the clipboard.
    #[test]
    #[ignore = "touches the live OS clipboard"]
    fn read_image_round_trips_through_the_clipboard() {
        let mut png = Vec::new();
        let src = image::RgbaImage::from_pixel(37, 19, image::Rgba([12, 200, 90, 255]));
        image::DynamicImage::ImageRgba8(src)
            .write_to(&mut std::io::Cursor::new(&mut png), image::ImageFormat::Png)
            .unwrap();
        write(Payload::ImagePng(png)).expect("write image to clipboard");

        let (out_png, w, h) = read_image().expect("an image must be readable back off the clipboard");
        assert_eq!((w, h), (37, 19), "dimensions must survive the clipboard round-trip");
        let decoded = image::load_from_memory(&out_png).expect("read_image must yield valid PNG");
        assert_eq!(
            (image::GenericImageView::width(&decoded), image::GenericImageView::height(&decoded)),
            (37, 19)
        );
    }
}
