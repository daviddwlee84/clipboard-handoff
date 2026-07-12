//! Gated PTY end-to-end smoke for `clip tui`: launch the built binary under a real
//! pseudo-terminal, wait for the header to render, quit with Ctrl-C, and confirm the
//! process exits cleanly.
//!
//! `#[ignore]`d — it spawns a background daemon and needs a PTY (the graphics-protocol
//! probe also adds a ~1-2s settle), so it stays out of the default `cargo test`. Run it
//! explicitly:
//!
//! ```sh
//! cargo test --test tui_e2e -- --ignored --nocapture
//! ```

use std::time::Duration;

use expectrl::Expect; // brings the `expect` method into scope

#[test]
#[ignore = "spawns a daemon + needs a real PTY; run with `--ignored`"]
fn tui_starts_renders_header_and_quits() {
    let pid = std::process::id();
    let tmp = std::env::temp_dir().join(format!("clip-tui-e2e-{pid}"));
    let _ = std::fs::remove_dir_all(&tmp);
    std::fs::create_dir_all(&tmp).unwrap();
    let cfg = tmp.display().to_string();
    let sock = tmp.join("d.sock").display().to_string();
    let room = format!("tuie2e-{pid}");
    let bin = env!("CARGO_BIN_EXE_clip");

    // Isolated config/socket/room so this never touches a real daemon.
    let cmd = format!("{bin} --config-dir {cfg} --socket {sock} --room {room} tui");
    let mut p = expectrl::spawn(&cmd).expect("spawn `clip tui` under a PTY");
    p.set_expect_timeout(Some(Duration::from_secs(20)));

    // The header title is " clip · messenger " — its presence proves the daemon was
    // reached, the terminal was taken over, and the first frame rendered.
    p.expect("messenger")
        .expect("the TUI header should render within 20s");

    // Ctrl-C quits from any focus (checked before the focus match); nothing was received,
    // so it exits straight away without the clear-on-quit prompt.
    p.send("\x03").expect("send Ctrl-C");

    // A clean quit closes the PTY (EOF) within the timeout.
    p.expect(expectrl::Eof)
        .expect("the TUI should exit and close the PTY after Ctrl-C");

    // Best-effort cleanup: stop the auto-spawned daemon and drop the temp dir.
    let _ = std::process::Command::new(bin)
        .args(["--socket", &sock, "daemon", "stop"])
        .status();
    let _ = std::fs::remove_dir_all(&tmp);
}
