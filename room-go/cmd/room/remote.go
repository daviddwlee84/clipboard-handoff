package main

// `room remote <ssh-host>` is a VSCode-Remote-SSH-style one-command connect: it
// points at an SSH host (already configured in ~/.ssh/config with key auth) and
//   (a) auto-installs the `room` binary there if missing,
//   (b) starts a room server on the remote bound to loopback,
//   (c) opens an SSH tunnel to it, and
//   (d) connects the local client daemon through the tunnel.
//
// It shells out to the local `ssh`/`scp` binaries (via os/exec) so the user's
// ~/.ssh/config, keys, and agent Just Work — we never reimplement SSH. Every
// step is idempotent: a re-run reuses an installed binary, a running server,
// and a live tunnel.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/daviddwlee84/cross-platform-copy/room-go/internal/ipc"
)

const (
	defaultRemotePort = 2299
	remoteBinDisplay  = "~/.local/bin/room"
	remoteServerLog   = "/tmp/room-remote-server.log"
)

// remoteState is the per-host state file (under <config-dir>/remote/<host>.json)
// so `--stop` can find and tear down the tunnel / server we created.
type remoteState struct {
	Host          string `json:"host"`
	Room          string `json:"room"`
	RPort         int    `json:"rport"`          // remote server loopback port
	LPort         int    `json:"lport"`          // local tunnel port
	TunnelPID     int    `json:"tunnel_pid"`     // pid of the background `ssh -N -L …`
	ServerStarted bool   `json:"server_started"` // true if *we* spawned the remote server
	RemoteBin     string `json:"remote_bin"`
	Target        string `json:"target"` // room@127.0.0.1:<lport>
}

func cmdRemote(g Globals, args []string) int {
	fs := newFlagSet("remote")
	rport := fs.int("rport", defaultRemotePort, "remote room-server port (bound to loopback)")
	lport := fs.int("lport", defaultRemotePort, "local tunnel port (auto-bumped if busy)")
	stop := fs.bool("stop", false, "tear down the tunnel (and any server we started) for this host")
	pos, code := fs.parseInterspersed(args)
	if code != 0 {
		return code
	}
	if len(pos) < 1 {
		return fail(2, errors.New("usage: room remote <ssh-host> [--room NAME] [--rport N] [--lport N] [--stop]"))
	}
	host := pos[0]

	// The room name rides the global --room flag (parsed before the subcommand),
	// defaulting to "default" like everywhere else.
	room := g.Room
	if room == "" {
		room = "default"
	}

	sp, err := remoteStatePath(g, host)
	if err != nil {
		return fail(1, err)
	}

	if *stop {
		return remoteStop(g, host, sp)
	}
	return remoteConnect(g, host, room, sp, *rport, *lport)
}

// remoteConnect runs the full detect → install → server → tunnel → join flow.
func remoteConnect(g Globals, host, room, sp string, rport, lport int) int {
	// 1) Detect the remote OS/arch so we can install the right binary.
	fmt.Printf("remote %s: detecting architecture…\n", host)
	uname, err := runSSH(host, "uname -sm")
	if err != nil {
		return fail(1, fmt.Errorf("cannot reach %q over ssh (check ~/.ssh/config + key auth): %w", host, err))
	}
	goos, goarch, err := mapUname(uname)
	if err != nil {
		return fail(1, err)
	}
	fmt.Printf("remote %s: %s → %s/%s\n", host, uname, goos, goarch)

	// 2) Ensure ~/.local/bin/room exists on the remote, bootstrapping if absent.
	present, err := remoteBinExists(host)
	if err != nil {
		return fail(1, err)
	}
	if present {
		fmt.Printf("remote %s: room binary already present at %s\n", host, remoteBinDisplay)
	} else {
		fmt.Printf("remote %s: room binary missing — bootstrapping…\n", host)
		bin, how, berr := produceBinary(goos, goarch)
		if berr != nil {
			return fail(1, berr)
		}
		fmt.Printf("remote %s: %s → %s\n", host, how, bin)
		if _, err := runSSH(host, `mkdir -p "$HOME/.local/bin"`); err != nil {
			return fail(1, fmt.Errorf("mkdir ~/.local/bin: %w", err))
		}
		if err := scpTo(host, bin); err != nil {
			return fail(1, fmt.Errorf("scp binary to %s: %w", host, err))
		}
		if _, err := runSSH(host, `chmod +x "$HOME/.local/bin/room"`); err != nil {
			return fail(1, fmt.Errorf("chmod remote binary: %w", err))
		}
		sz := int64(0)
		if info, e := os.Stat(bin); e == nil {
			sz = info.Size()
		}
		fmt.Printf("remote %s: installed room → %s (%s)\n", host, remoteBinDisplay, humanBytes(sz))
	}

	// 3) Ensure a room server is listening on the remote loopback:<rport>.
	fmt.Printf("remote %s: ensuring room server on 127.0.0.1:%d…\n", host, rport)
	started, err := ensureRemoteServer(host, rport)
	if err != nil {
		return fail(1, err)
	}
	if started {
		fmt.Printf("remote %s: started room server on 127.0.0.1:%d (log %s)\n", host, rport, remoteServerLog)
	} else {
		fmt.Printf("remote %s: reusing running room server on 127.0.0.1:%d\n", host, rport)
	}

	// 4) Open (or reuse) the SSH tunnel local:<lport> → remote:127.0.0.1:<rport>.
	lp := lport
	if st, e := loadRemoteState(sp); e == nil && st.RPort == rport &&
		st.TunnelPID > 0 && pidAlive(st.TunnelPID) && localReachable(st.LPort) {
		lp = st.LPort
		fmt.Printf("remote %s: reusing live SSH tunnel localhost:%d (pid %d)\n", host, lp, st.TunnelPID)
		st.Room, st.ServerStarted = room, st.ServerStarted || started
		st.Target = fmt.Sprintf("room@127.0.0.1:%d", lp)
		_ = saveRemoteState(sp, st)
	} else {
		lp = pickLocalPort(lport)
		if lp == 0 {
			return fail(1, fmt.Errorf("no free local port in [%d,%d) for the tunnel", lport, lport+20))
		}
		tlog := filepath.Join(filepath.Dir(sp), sanitizeHost(host)+"-tunnel.log")
		pid, terr := openTunnel(host, lp, rport, tlog)
		if terr != nil {
			return fail(1, terr)
		}
		fmt.Printf("remote %s: tunnel up  localhost:%d → %s:127.0.0.1:%d (pid %d)\n", host, lp, host, rport, pid)
		st := &remoteState{
			Host: host, Room: room, RPort: rport, LPort: lp, TunnelPID: pid,
			ServerStarted: started, RemoteBin: remoteBinDisplay,
			Target: fmt.Sprintf("room@127.0.0.1:%d", lp),
		}
		if err := saveRemoteState(sp, st); err != nil {
			return fail(1, err)
		}
	}

	// 5) Connect the local client daemon (auto-spawned) through the tunnel.
	target := fmt.Sprintf("room@127.0.0.1:%d", lp)
	resp, code := roundtrip(g, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpJoin, Target: target})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return fail(resp.Code, errors.New(resp.Message))
	}

	fmt.Printf("connected to %s via SSH tunnel (room %q) — send/recv/tui now reach it\n", host, room)
	return 0
}

// remoteStop tears down the tunnel (and the remote server if we started it) and
// removes the state file. Idempotent: a missing state file is a no-op success.
func remoteStop(g Globals, host, sp string) int {
	st, err := loadRemoteState(sp)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("remote %s: no active tunnel (nothing to stop)\n", host)
			return 0
		}
		return fail(1, err)
	}

	if st.TunnelPID > 0 && pidAlive(st.TunnelPID) {
		// The tunnel was started with Setsid, so it leads its own process group;
		// kill the whole group (negative pid) to also reap any ssh children.
		if kerr := syscall.Kill(-st.TunnelPID, syscall.SIGTERM); kerr != nil {
			_ = syscall.Kill(st.TunnelPID, syscall.SIGTERM)
		}
		fmt.Printf("remote %s: stopped SSH tunnel (pid %d)\n", host, st.TunnelPID)
	} else {
		fmt.Printf("remote %s: tunnel already stopped\n", host)
	}

	if st.ServerStarted {
		// Only kill the exact server we spawned (matched by its loopback addr).
		// The `[r]oom` bracket keeps the pattern from matching pkill's own command
		// line (which would otherwise self-terminate the ssh session).
		kill := fmt.Sprintf(`pkill -f "[r]oom server --addr 127.0.0.1:%d" >/dev/null 2>&1; echo done`, st.RPort)
		if _, err := runSSH(host, kill); err != nil {
			fmt.Printf("remote %s: warning: could not stop remote server: %v\n", host, err)
		} else {
			fmt.Printf("remote %s: stopped remote server on 127.0.0.1:%d\n", host, st.RPort)
		}
	}

	_ = os.Remove(sp)
	fmt.Printf("remote %s: state cleared\n", host)
	return 0
}

// ---- remote binary bootstrap ------------------------------------------------

// produceBinary returns a path to a room binary for goos/goarch to upload. It
// prefers cross-building from the module (located by walking up from CWD for
// go.mod); if there is no module and the local host already matches the remote,
// it falls back to the currently-running executable; else it errors with a
// clear message.
func produceBinary(goos, goarch string) (path, how string, err error) {
	if moduleDir, ok := findModuleDir(); ok {
		out := filepath.Join(os.TempDir(), fmt.Sprintf("room-%s-%s", goos, goarch))
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/room")
		cmd.Dir = moduleDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		var se strings.Builder
		cmd.Stderr = &se
		if e := cmd.Run(); e != nil {
			return "", "", fmt.Errorf("cross-build room for %s/%s failed: %v: %s", goos, goarch, e, strings.TrimSpace(se.String()))
		}
		return out, fmt.Sprintf("cross-built room (%s/%s, CGO_ENABLED=0)", goos, goarch), nil
	}
	if runtime.GOOS == goos && runtime.GOARCH == goarch {
		exe, e := os.Executable()
		if e != nil {
			return "", "", e
		}
		return exe, fmt.Sprintf("copying the running room binary (local host is %s/%s)", goos, goarch), nil
	}
	return "", "", fmt.Errorf(
		"cannot produce a %s/%s room binary: no go.mod found by walking up from the current directory, "+
			"and this host is %s/%s so the running binary can't be reused — run `room remote` from inside the room-go repo (needs the local `go` toolchain)",
		goos, goarch, runtime.GOOS, runtime.GOARCH)
}

// findModuleDir walks up from CWD looking for the room-go module root (a go.mod
// alongside cmd/room).
func findModuleDir() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "cmd", "room")); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func remoteBinExists(host string) (bool, error) {
	out, err := runSSH(host, `test -x "$HOME/.local/bin/room" && echo YES || echo NO`)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "YES"), nil
}

// ---- remote server ----------------------------------------------------------

// ensureRemoteServer makes sure a room server is listening on 127.0.0.1:<rport>.
// It reuses one if already listening, else starts it detached (setsid+nohup) and
// waits for it to come up. Returns whether it started a new one.
func ensureRemoteServer(host string, rport int) (started bool, err error) {
	if remoteListening(host, rport) {
		return false, nil
	}
	start := fmt.Sprintf(
		`mkdir -p "$HOME/.config/room"; `+
			`setsid nohup "$HOME/.local/bin/room" server --addr 127.0.0.1:%d --host-key "$HOME/.config/room/host_key" `+
			`>%s 2>&1 </dev/null & echo spawned`,
		rport, remoteServerLog)
	if _, err := runSSH(host, start); err != nil {
		return false, fmt.Errorf("start remote server: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if remoteListening(host, rport) {
			return true, nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	tail, _ := runSSH(host, "tail -n 20 "+remoteServerLog)
	return true, fmt.Errorf("remote server never listened on 127.0.0.1:%d; log:\n%s", rport, tail)
}

// remoteListening reports whether something is LISTENing on the remote 127.0.0.1
// :port, preferring `ss` and falling back to lsof/netstat. If none of those exist
// it returns false (we then try to start a server; a double-bind would surface
// as a start failure).
func remoteListening(host string, port int) bool {
	chk := fmt.Sprintf(`p=%d; `+
		`if command -v ss >/dev/null 2>&1; then ss -ltn "sport = :$p" 2>/dev/null | grep -q LISTEN && echo YES || echo NO; `+
		`elif command -v lsof >/dev/null 2>&1; then lsof -nP -iTCP:$p -sTCP:LISTEN >/dev/null 2>&1 && echo YES || echo NO; `+
		`elif command -v netstat >/dev/null 2>&1; then netstat -ltn 2>/dev/null | grep -qE "[:.]$p[[:space:]]" && echo YES || echo NO; `+
		`else echo UNKNOWN; fi`, port)
	out, err := runSSH(host, chk)
	if err != nil {
		return false
	}
	return strings.Contains(out, "YES")
}

// ---- ssh tunnel -------------------------------------------------------------

// openTunnel starts a detached `ssh -N -L lport:127.0.0.1:rport host`, returns
// its pid, and waits for the local port to forward. On failure it kills the
// process it started.
func openTunnel(host string, lport, rport int, logPath string) (int, error) {
	spec := fmt.Sprintf("%d:127.0.0.1:%d", lport, rport)
	args := []string{"-N", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3"}
	args = append(args, sshBaseOpts()...)
	args = append(args, "-L", spec, host)

	cmd := exec.Command("ssh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach: survive `room remote` exiting
	cmd.Stdin = nil
	if lf, e := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); e == nil {
		cmd.Stdout, cmd.Stderr = lf, lf
		defer lf.Close()
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start ssh tunnel: %w", err)
	}
	pid := cmd.Process.Pid

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if localReachable(lport) {
			return pid, nil
		}
		if !pidAlive(pid) {
			tail, _ := os.ReadFile(logPath)
			return 0, fmt.Errorf("ssh tunnel exited early: %s", strings.TrimSpace(string(tail)))
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	tail, _ := os.ReadFile(logPath)
	return 0, fmt.Errorf("tunnel opened (pid %d) but localhost:%d never forwarded: %s", pid, lport, strings.TrimSpace(string(tail)))
}

// ---- ssh/scp plumbing -------------------------------------------------------

// sshBaseOpts are the options every ssh/scp invocation shares: non-interactive
// (fail rather than prompt for a password — key auth is assumed) with a bounded
// connect timeout and TOFU host-key acceptance.
func sshBaseOpts() []string {
	return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new"}
}

// runSSH runs one remote command and returns its trimmed stdout (stderr folded
// into the error).
func runSSH(host, remoteCmd string) (string, error) {
	args := append(sshBaseOpts(), host, remoteCmd)
	cmd := exec.Command("ssh", args...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(se.String())
		if msg == "" {
			msg = strings.TrimSpace(so.String())
		}
		if msg != "" {
			return strings.TrimSpace(so.String()), fmt.Errorf("%w: %s", err, msg)
		}
		return strings.TrimSpace(so.String()), err
	}
	return strings.TrimSpace(so.String()), nil
}

// scpTo copies localPath to the remote ~/.local/bin/room (scp resolves the bare
// path relative to the remote home directory).
func scpTo(host, localPath string) error {
	args := append([]string{"-q"}, sshBaseOpts()...)
	args = append(args, localPath, host+":.local/bin/room")
	cmd := exec.Command("scp", args...)
	var se strings.Builder
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(se.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// ---- state file -------------------------------------------------------------

func remoteStatePath(g Globals, host string) (string, error) {
	dir, err := resolveConfigDir(g)
	if err != nil {
		return "", err
	}
	d := filepath.Join(dir, "remote")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(d, sanitizeHost(host)+".json"), nil
}

func loadRemoteState(path string) (*remoteState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s remoteState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveRemoteState(path string, s *remoteState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- small helpers ----------------------------------------------------------

func sanitizeHost(h string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_", " ", "_", "*", "_").Replace(h)
}

func mapUname(unameSM string) (goos, goarch string, err error) {
	f := strings.Fields(unameSM)
	if len(f) < 2 {
		return "", "", fmt.Errorf("unexpected `uname -sm` output %q", unameSM)
	}
	switch f[0] {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported remote OS %q (want Linux/Darwin)", f[0])
	}
	switch f[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported remote arch %q (want x86_64/aarch64)", f[1])
	}
	return goos, goarch, nil
}

func pickLocalPort(start int) int {
	for p := start; p < start+20; p++ {
		if portFreeLocal(p) {
			return p
		}
	}
	return 0
}

func portFreeLocal(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func localReachable(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
