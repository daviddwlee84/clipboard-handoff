// Package cli is the shared command-line surface (SPEC §2) for every
// experiments-track probe. Both probes have an identical CLI and thin-client
// IPC layer; only the transport differs, so a probe's main() just supplies an
// App{BinName, NewTransport}. The daemon command wires the probe's transport
// into the shared daemon; every other command is a transport-agnostic IPC
// client. Discovery is automatic (mDNS), so `pair` is a documented no-op.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/daemon"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/ipc"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/transport"
)

// App parameterizes the shared CLI for one probe.
type App struct {
	BinName      string // "lan" | "libp2p-mesh"
	Tagline      string // one-line description for usage
	TransportTag string // shown in `status` (e.g. "quic+mdns")
	// NewTransport builds the probe's transport for the daemon, given the loaded
	// config store, the effective room and the device name.
	NewTransport func(cfg *config.Store, room, deviceName string) (transport.Transport, error)
}

// Globals are the shared flags (SPEC §2) that may precede the subcommand.
type Globals struct {
	app       App
	ConfigDir string
	Socket    string
	Room      string
	JSON      bool
	Quiet     bool
	Verbose   bool
}

// Main is the probe entrypoint. It returns a process exit code.
func (a App) Main() int {
	g, rest := a.parseGlobals(os.Args[1:])
	if len(rest) == 0 {
		a.usage()
		return 2
	}
	sub, args := rest[0], rest[1:]
	return g.dispatch(sub, args)
}

func (g Globals) dispatch(sub string, args []string) int {
	switch sub {
	case "daemon":
		return g.cmdDaemon(args)
	case "send":
		return g.cmdSend(args)
	case "recv":
		return g.cmdRecv(args)
	case "paste":
		return g.cmdPaste(args)
	case "clear":
		return g.cmdClear(args)
	case "tui":
		return g.cmdTUI(args)
	case "status":
		return g.cmdStatus(args)
	case "peers":
		return g.cmdPeers(args)
	case "config":
		return g.cmdConfig(args)
	case "pair", "join":
		return g.cmdPair(args)
	case "help", "-h", "--help":
		g.app.usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n", g.app.BinName, sub)
		g.app.usage()
		return 2
	}
}

// parseGlobals pulls recognized global flags out of args wherever they appear
// (before or after the subcommand), returning the remainder. Only known global
// flag names are consumed, so subcommand-specific flags are untouched.
func (a App) parseGlobals(args []string) (Globals, []string) {
	g := Globals{app: a}
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inlineVal, hasInline := arg, "", false
		if eq := strings.Index(arg, "="); strings.HasPrefix(arg, "-") && eq >= 0 {
			name, inlineVal, hasInline = arg[:eq], arg[eq+1:], true
		}
		next := func() string {
			if hasInline {
				return inlineVal
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "--config-dir":
			g.ConfigDir = next()
		case "--socket":
			g.Socket = next()
		case "--room":
			g.Room = next()
		case "--json":
			g.JSON = true
		case "-q", "--quiet":
			g.Quiet = true
		case "-v", "--verbose":
			g.Verbose = true
		default:
			rest = append(rest, arg)
		}
	}
	return g, rest
}

// spawnArgs reconstructs the global flags so an auto-spawned daemon inherits
// this client's config dir / socket / room.
func (g Globals) spawnArgs() []string {
	var a []string
	if g.ConfigDir != "" {
		a = append(a, "--config-dir", g.ConfigDir)
	}
	if g.Socket != "" {
		a = append(a, "--socket", g.Socket)
	}
	if g.Room != "" {
		a = append(a, "--room", g.Room)
	}
	return a
}

func (g Globals) socketPath() (string, error) { return ipc.SocketPath(g.Socket, g.app.BinName) }

func (g Globals) openConfig() (*config.Store, error) {
	return config.Open(g.ConfigDir, g.app.BinName)
}

func (g Globals) effectiveRoom(cfg *config.Store) string {
	if g.Room != "" {
		return g.Room
	}
	if r := cfg.Get().Room; r != "" {
		return r
	}
	return "default"
}

// ---- daemon ----------------------------------------------------------------

func (g Globals) cmdDaemon(args []string) int {
	if len(args) > 0 && args[0] == "stop" {
		return g.cmdDaemonStop(args[1:])
	}

	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	_ = fs.Bool("foreground", false, "run in the foreground (default when launched directly)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := g.openConfig()
	if err != nil {
		return g.fail(1, err)
	}
	room := g.effectiveRoom(cfg)
	deviceName := cfg.Get().DeviceName

	tr, err := g.app.NewTransport(cfg, room, deviceName)
	if err != nil {
		return g.fail(1, fmt.Errorf("init transport: %w", err))
	}

	sock, err := g.socketPath()
	if err != nil {
		return g.fail(1, err)
	}

	d := daemon.New(cfg, sock, room, g.app.TransportTag, tr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Run(ctx); err != nil {
		return g.fail(1, err)
	}
	return 0
}

// cmdDaemonStop shuts down the resident daemon, applying clear_on_exit (SPEC §8).
// It resolves the interactive "ask" policy client-side (prompt on a TTY → t/a/n,
// else transient) and hands the daemon the concrete policy. It never auto-spawns
// a daemon just to stop it.
func (g Globals) cmdDaemonStop(args []string) int {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sock, err := g.socketPath()
	if err != nil {
		return g.fail(1, err)
	}
	conn, err := ipc.Dial(sock, false, nil) // do NOT spawn a daemon just to stop it
	if err != nil {
		if !g.Quiet {
			fmt.Fprintf(os.Stderr, "%s: no daemon running\n", g.app.BinName)
		}
		return 3
	}
	defer conn.Close()

	policy := g.resolveExitPolicy()
	if err := ipc.WriteReq(conn, &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpDaemonStop, Value: policy}); err != nil {
		return g.fail(3, err)
	}
	resp, err := ipc.ReadResp(conn)
	if err != nil {
		return g.fail(3, err)
	}
	if resp.Kind == ipc.RespErr {
		return g.fail(resp.Code, errors.New(resp.Message))
	}
	if !g.Quiet {
		fmt.Printf("daemon stopping (clear_on_exit: %s)\n", policy)
	}
	return 0
}

// resolveExitPolicy reads clear_on_exit and resolves the interactive "ask" to a
// concrete scope: a TTY prompt (t/a/n) when attached, else "transient".
func (g Globals) resolveExitPolicy() string {
	policy := "ask"
	if cfg, err := g.openConfig(); err == nil {
		if p := cfg.Get().ClearOnExit; p != "" {
			policy = p
		}
	}
	if policy != "ask" {
		return policy
	}
	if !stdinIsTTY() {
		return "transient"
	}
	switch promptChoice("clear this session on exit? [t]ransient / [a]ll incl sinks / [n]o: ") {
	case "a":
		return "all"
	case "n":
		return "never"
	default:
		return "transient"
	}
}

// ---- send ------------------------------------------------------------------

func (g Globals) cmdSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	asText := fs.Bool("text", false, "force text")
	asImage := fs.Bool("image", false, "force image (PNG/JPEG)")
	asFile := fs.Bool("file", false, "force file (arbitrary bytes)")
	_ = fs.Bool("auto", false, "sniff type (default)")
	name := fs.String("name", "", "filename for image/file items (advisory)")
	// Parse flags even when they follow the positional PATH (SPEC signature
	// `send [PATH] [--file] [--name NAME]`); the stdlib flag package otherwise
	// stops at the first non-flag, so parse in a loop collecting positionals.
	rest := args
	var positionals []string
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}

	// Read PATH if given, else stdin (SPEC §2). A PATH (or --name) sets filename.
	var data []byte
	var err error
	filename := *name
	if len(positionals) > 0 {
		path := positionals[0]
		data, err = os.ReadFile(path)
		if err != nil {
			return g.fail(1, err)
		}
		if filename == "" {
			filename = filepath.Base(path)
		}
	} else {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return g.fail(1, err)
		}
	}
	if len(data) == 0 {
		return g.fail(2, errors.New("send: empty input"))
	}

	force := ""
	switch {
	case *asText:
		force = "text"
	case *asImage:
		force = "image"
	case *asFile:
		force = "file"
	}

	resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpSend, Bytes: data, Force: force, Name: filename})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return g.fail(resp.Code, errors.New(resp.Message))
	}
	if resp.Code == 4 && !g.Quiet {
		fmt.Fprintf(os.Stderr, "%s: warning: %s\n", g.app.BinName, resp.Message)
	}
	return 0
}

// ---- recv ------------------------------------------------------------------

func (g Globals) cmdRecv(args []string) int {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "stream until interrupted")
	latestImage := fs.Bool("latest-image", false, "return the latest image")
	emitPath := fs.Bool("emit-path", false, "print the image file path")
	out := fs.String("out", "", "write image to this path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	kind := "any"
	if *latestImage {
		kind = "image"
	}
	req := &ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpRecv, Kind: kind, Follow: *follow}

	sock, err := g.socketPath()
	if err != nil {
		return g.fail(1, err)
	}
	conn, err := ipc.Dial(sock, true, g.spawnArgs())
	if err != nil {
		return g.fail(3, err)
	}
	defer conn.Close()
	if err := ipc.WriteReq(conn, req); err != nil {
		return g.fail(3, err)
	}

	if *follow {
		for {
			resp, err := ipc.ReadResp(conn)
			if err != nil {
				return 0
			}
			printItem(resp, *emitPath || *latestImage, *out)
		}
	}

	resp, err := ipc.ReadResp(conn)
	if err != nil {
		return g.fail(3, err)
	}
	if resp.Kind == ipc.RespErr {
		return g.fail(resp.Code, errors.New(resp.Message))
	}
	printItem(resp, *emitPath || *latestImage, *out)
	return 0
}

// printItem renders one item to stdout: text inline; image/file as a file path
// (copied to --out first if requested).
func printItem(resp *ipc.Response, emitPath bool, out string) {
	if resp.Kind != ipc.RespItem || resp.Item == nil {
		return
	}
	env := &resp.Item.Envelope
	if env.Type == "text" {
		fmt.Println(env.Text)
		return
	}
	path := resp.Item.LocalPath
	if out != "" && path != "" {
		if err := copyFile(path, out); err == nil {
			path = out
		}
	}
	if emitPath || out != "" {
		fmt.Println(path)
		return
	}
	if env.Type == "file" {
		name := env.Filename
		if name == "" && env.Blob != nil {
			name = short(env.Blob.Hash)
		}
		size := uint64(0)
		if env.Blob != nil {
			size = env.Blob.Size
		}
		fmt.Printf("[file %s %dB %s]\n", name, size, path)
		return
	}
	fmt.Printf("[image %dx%d %s]\n", env.Blob.W, env.Blob.H, path)
}

// ---- paste -----------------------------------------------------------------

func (g Globals) cmdPaste(args []string) int {
	resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpPaste})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return g.fail(resp.Code, errors.New(resp.Message))
	}
	if !g.Quiet {
		fmt.Println("pasted latest item to clipboard")
	}
	return 0
}

// ---- clear -----------------------------------------------------------------

// cmdClear reverts this session's received data (SPEC §8). Default: transient
// only. --all also reverts session sink writes (truncate text_file, delete
// save_dir files this session wrote) — prompts unless --yes / not a TTY.
func (g Globals) cmdClear(args []string) int {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	all := fs.Bool("all", false, "also revert this session's sink writes (text_file + save_dir)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt for --all")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	reqAll := false
	if *all {
		switch {
		case *yes:
			reqAll = true
		case stdinIsTTY():
			reqAll = promptYesNo("revert this session's sink writes (truncate text_file, delete save_dir files written this session)? [y/N] ")
			if !reqAll && !g.Quiet {
				fmt.Fprintf(os.Stderr, "%s: sinks kept; clearing transient only\n", g.app.BinName)
			}
		default:
			fmt.Fprintf(os.Stderr, "%s: --all needs --yes without a TTY; clearing transient only\n", g.app.BinName)
		}
	}

	resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpClear, All: reqAll})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr {
		return g.fail(resp.Code, errors.New(resp.Message))
	}
	if !g.Quiet {
		if reqAll {
			fmt.Println("cleared this session (transient + sink writes)")
		} else {
			fmt.Println("cleared this session (transient)")
		}
	}
	return 0
}

// ---- status / peers --------------------------------------------------------

func (g Globals) cmdStatus(args []string) int {
	resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpStatus})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr || resp.Status == nil {
		return g.fail(1, errors.New("status unavailable"))
	}
	s := resp.Status
	if g.JSON {
		b, _ := json.Marshal(s)
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("identity:   %s\n", s.Identity)
	fmt.Printf("device:     %s\n", s.DeviceName)
	fmt.Printf("room:       %s\n", s.Room)
	fmt.Printf("transport:  %s\n", s.Transport)
	fmt.Printf("auto_copy:  %s\n", s.AutoCopy)
	if s.Clipboard != "" {
		fmt.Printf("clipboard:  %s\n", s.Clipboard)
	}
	fmt.Printf("peers:      %d connected\n", len(s.Peers))
	for _, p := range s.Peers {
		fmt.Printf("  - %s  %s  %s\n", p.Name, short(p.ID), p.Addr)
	}
	fmt.Printf("buffer:     %d item(s)\n", s.Buffer)
	return 0
}

func (g Globals) cmdPeers(args []string) int {
	resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpPeers})
	if code != 0 {
		return code
	}
	if resp.Kind == ipc.RespErr || resp.Status == nil {
		return g.fail(1, errors.New("peers unavailable"))
	}
	if g.JSON {
		b, _ := json.Marshal(resp.Status.Peers)
		fmt.Println(string(b))
		return 0
	}
	if len(resp.Status.Peers) == 0 {
		fmt.Println("no peers connected")
		return 0
	}
	for _, p := range resp.Status.Peers {
		fmt.Printf("%s\t%s\t%s\n", p.Name, p.ID, p.Addr)
	}
	return 0
}

// ---- config ----------------------------------------------------------------

func (g Globals) cmdConfig(args []string) int {
	if len(args) < 2 {
		return g.fail(2, fmt.Errorf("usage: %s config set KEY VALUE | %s config get KEY", g.app.BinName, g.app.BinName))
	}
	switch args[0] {
	case "set":
		if len(args) < 3 {
			return g.fail(2, errors.New("usage: config set KEY VALUE"))
		}
		resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpConfigSet, Key: args[1], Value: args[2]})
		if code != 0 {
			return code
		}
		if resp.Kind == ipc.RespErr {
			return g.fail(resp.Code, errors.New(resp.Message))
		}
		if !g.Quiet {
			fmt.Printf("%s = %s\n", args[1], args[2])
		}
		return 0
	case "get":
		resp, code := g.roundtrip(&ipc.Request{IPCVersion: ipc.IPCVersion, Op: ipc.OpConfigGet, Key: args[1]})
		if code != 0 {
			return code
		}
		if resp.Kind == ipc.RespErr {
			return g.fail(resp.Code, errors.New(resp.Message))
		}
		fmt.Println(resp.Value)
		return 0
	default:
		return g.fail(2, fmt.Errorf("config: unknown action %q", args[0]))
	}
}

// ---- pair (no-op) ----------------------------------------------------------

// cmdPair is a documented no-op: these probes discover peers automatically over
// mDNS in the same room, so no ticket/join step is required. It prints an
// explanatory note to stderr and exits 0 so the shared bake-off harness's
// pairing hook is satisfied without emitting a ticket.
func (g Globals) cmdPair(args []string) int {
	if !g.Quiet {
		fmt.Fprintf(os.Stderr, "%s: discovery is automatic (mDNS in room %q); no pairing needed.\n", g.app.BinName, g.Room)
	}
	return 0
}

// ---- shared client plumbing ------------------------------------------------

func (g Globals) roundtrip(req *ipc.Request) (*ipc.Response, int) {
	sock, err := g.socketPath()
	if err != nil {
		return nil, g.fail(1, err)
	}
	conn, err := ipc.Dial(sock, true, g.spawnArgs())
	if err != nil {
		return nil, g.fail(3, err)
	}
	defer conn.Close()
	if err := ipc.WriteReq(conn, req); err != nil {
		return nil, g.fail(3, err)
	}
	resp, err := ipc.ReadResp(conn)
	if err != nil {
		return nil, g.fail(3, err)
	}
	return resp, 0
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func (g Globals) fail(code int, err error) int {
	fmt.Fprintf(os.Stderr, "%s: %v\n", g.app.BinName, err)
	return code
}

// stdinIsTTY reports whether stdin is an interactive terminal (so we may prompt).
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// promptLine writes a prompt to stderr and reads one trimmed, lowercased line.
func promptLine(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}

// promptYesNo returns true only for an affirmative (y/yes) answer.
func promptYesNo(prompt string) bool {
	switch promptLine(prompt) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// promptChoice returns the first letter of the answer ("t"/"a"/"n"/…).
func promptChoice(prompt string) string {
	ans := promptLine(prompt)
	if ans == "" {
		return ""
	}
	return ans[:1]
}

func short(s string) string {
	if len(s) <= 20 {
		return s
	}
	return s[:20] + "…"
}

func (a App) usage() {
	fmt.Fprintf(os.Stderr, `%s — %s (experiments track, Phase 0)

Usage:
  %s [global flags] <command> [command flags]

Global flags:
  --config-dir PATH   config/identity directory
  --socket PATH       IPC socket path
  --room NAME         room to join (default "default")
  --json              machine-readable output (status/peers)
  -q, --quiet         quieter output
  -v, --verbose       debug logging

Commands:
  daemon   [--foreground] | daemon stop     run/stop the resident daemon (auto-spawned)
  send     [PATH] [--text|--image|--file|--auto] [--name NAME]
  recv     [--follow] [--latest-image --emit-path] [--out PATH]
  paste                                   write latest received item to OS clipboard
  clear    [--all] [--yes]                clear this session (transient; --all also reverts sinks)
  tui                                     launch the messenger-style chat TUI (SPEC §4)
  status   [--json]
  peers    [--json]
  config   set KEY VALUE | get KEY        keys: auto_copy, save_dir, text_file, clear_on_exit, …
  pair                                    no-op: discovery is automatic (mDNS)
`, a.BinName, a.Tagline, a.BinName)
}
