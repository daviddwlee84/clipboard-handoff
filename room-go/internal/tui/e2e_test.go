//go:build e2e

// Layer 2 — a PTY end-to-end smoke for `room tui`. Unlike the teatest render
// tests (render_test.go), which drive the model in-process, this launches the
// *built* binary under a real pseudo-terminal against a live server + client
// daemon, then reads the rendered header off the PTY and quits it.
//
// It is gated behind the `e2e` build tag (and also skips under `-short`) so it
// does NOT run in the default `go test ./...`. Run it with:
//
//	go test -tags e2e ./internal/tui/ -run TestE2E -v
package tui

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestE2E_TUIOverPTY(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped under -short")
	}

	bin := buildRoom(t)
	dir := t.TempDir()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	target := "room@" + addr
	room := "e2e"
	hostKey := filepath.Join(dir, "host_key")
	cfgDir := filepath.Join(dir, "client")
	sock := filepath.Join(dir, "d.sock")

	// 1) start the SSH room server on an isolated loopback port.
	var srvLog syncBuf
	srv := exec.Command(bin, "server", "--addr", addr, "--host-key", hostKey)
	srv.Stdout, srv.Stderr = &srvLog, &srvLog
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	})
	waitForTCP(t, addr, 5*time.Second)

	// 2) join the client daemon to the server (auto-spawns the detached daemon
	//    with these isolated --config-dir/--socket/--room flags).
	global := []string{"--config-dir", cfgDir, "--socket", sock, "--room", room}
	join := exec.Command(bin, append(append([]string{}, global...), "join", target)...)
	if out, err := join.CombinedOutput(); err != nil {
		t.Fatalf("join failed: %v\n--- join output ---\n%s\n--- server log ---\n%s", err, out, srvLog.String())
	}
	t.Cleanup(func() {
		// Stop the detached daemon this test spawned.
		stop := exec.Command(bin, append(append([]string{}, global...), "daemon", "stop")...)
		_ = stop.Run()
	})

	// 3) run `room tui` under a PTY at a fixed 80x24 size.
	cmd := exec.Command(bin, append(append([]string{}, global...), "tui")...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("pty start tui: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	// Drain the PTY continuously into a buffer so the child never blocks writing.
	var mu sync.Mutex
	var buf bytes.Buffer
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf.Write(chunk[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	// 4) read until the TUI header renders (contains "room:e2e").
	want := "room:" + room
	if !eventually(8*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Contains(buf.Bytes(), []byte(want))
	}) {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		_ = cmd.Process.Kill()
		t.Fatalf("TUI header %q not seen on PTY within timeout.\n--- pty output ---\n%q\n--- server log ---\n%s", want, got, srvLog.String())
	}

	// 5) quit: Esc switches the composer to browse mode, then `q` quits (nothing
	//    was received this session, so it exits without a clear prompt).
	_, _ = ptmx.Write([]byte{0x1b}) // Esc
	time.Sleep(250 * time.Millisecond)
	_, _ = ptmx.Write([]byte("q"))

	// 6) assert a clean exit (exit code 0).
	if err := waitTimeout(cmd, 8*time.Second); err != nil {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		t.Fatalf("TUI did not exit cleanly: %v\n--- pty output ---\n%q", err, got)
	}
}

// buildRoom compiles the room binary into a temp path and returns it.
func buildRoom(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "room")
	build := exec.Command("go", "build", "-o", bin, "./cmd/room")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build room: %v\n%s", err, out)
	}
	return bin
}

// moduleRoot returns the room-go module root (two levels up from this file).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForTCP(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	if !eventually(d, func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}) {
		t.Fatalf("server did not accept on %s within %s", addr, d)
	}
}

// waitTimeout waits for cmd to exit, returning its error (nil == clean exit),
// or a timeout error after killing it.
func waitTimeout(cmd *exec.Cmd, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s", d)
	}
}

// syncBuf is a tiny concurrency-safe buffer for capturing subprocess logs.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
