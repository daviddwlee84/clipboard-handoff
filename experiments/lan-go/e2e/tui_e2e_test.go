//go:build e2e

// Layer-2 end-to-end smoke for `lan tui`, gated behind the `e2e` build tag so it
// is excluded from the default `go test ./...`. Run it with:
//
//	cd experiments/lan-go && go test -tags e2e ./e2e/ -run TestTUIPTYSmoke -v
//
// It builds the `lan` binary, boots `lan tui` under a real PTY (the daemon is
// auto-spawned by the first IPC call; no peer or mDNS discovery is required —
// only that the TUI comes up and renders its header), quits the TUI, asserts a
// clean exit, then stops the auto-spawned daemon and removes the temp dir.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// ansiRE strips CSI/OSC-ish escape sequences so header matching is robust
// whether or not the PTY's TERM makes lipgloss emit color codes.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// lanRoot returns the absolute lan-go module root (this file lives in lan-go/e2e).
func lanRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source path")
	}
	return filepath.Dir(filepath.Dir(file)) // e2e/ -> lan-go/
}

// buildLan compiles the probe binary (mirrors `go build -o bin/lan ./cmd/lan`)
// into a hermetic path so the smoke test never depends on prior build state.
func buildLan(t *testing.T, root, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, "./cmd/lan")
	cmd.Dir = root
	cmd.Env = os.Environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build lan failed: %v\n%s", err, b)
	}
}

func TestTUIPTYSmoke(t *testing.T) {
	root := lanRoot(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lan")
	buildLan(t, root, bin)

	sock := filepath.Join(tmp, "d.sock")
	room := fmt.Sprintf("e2e-%d", os.Getpid())

	// Best-effort teardown: stop the daemon the TUI auto-spawned (does not
	// itself spawn one). Runs before t.TempDir's own cleanup removes the dir.
	defer func() {
		stop := exec.Command(bin, "-q", "--config-dir", tmp, "--socket", sock, "daemon", "stop")
		_ = stop.Run()
	}()

	cmd := exec.Command(bin, "--config-dir", tmp, "--socket", sock, "--room", room, "tui")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 100})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	// Drain the PTY continuously into a mutex-guarded buffer.
	var mu sync.Mutex
	var buf bytes.Buffer
	go func() {
		b := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return ansiRE.ReplaceAllString(buf.String(), "")
	}

	// Wait for the header to render (covers daemon auto-spawn + program boot).
	deadline := time.Now().Add(15 * time.Second)
	for {
		s := snapshot()
		if strings.Contains(s, "lan tui") && strings.Contains(s, room) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TUI header did not appear within 15s; captured:\n%s", s)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The TUI starts focused on the composer, where `q` is literal text; switch
	// to browse mode (Esc) first, then press `q`. Nothing was received, so it
	// exits immediately with no clear-on-quit prompt.
	_, _ = ptmx.Write([]byte{0x1b}) // Esc -> browse mode
	time.Sleep(250 * time.Millisecond)
	_, _ = ptmx.Write([]byte("q")) // quit

	// Assert a clean exit, with a Ctrl-C fallback if `q` didn't take.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("lan tui exited with error after q: %v\ncaptured:\n%s", err, snapshot())
		}
	case <-time.After(5 * time.Second):
		_, _ = ptmx.Write([]byte{0x03}) // Ctrl-C fallback
		select {
		case err := <-waitErr:
			if err != nil {
				t.Fatalf("lan tui exited with error after Ctrl-C: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("lan tui did not exit after q + Ctrl-C; captured:\n%s", snapshot())
		}
	}

	t.Logf("PTY smoke OK — header rendered and `lan tui` exited cleanly.\nFinal screen:\n%s", snapshot())
}
