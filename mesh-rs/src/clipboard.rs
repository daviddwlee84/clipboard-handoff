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
}
