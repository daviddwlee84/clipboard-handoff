package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// cmdRemote implements `<bin> remote <host> …` by locating the shared engine
// (scripts/remote.sh) and exec'ing it with this tool's name. All the ssh/scp
// bootstrap + start-remote-daemon + connect logic lives in that one place, so
// every tool shares it (SPEC: "common place").
func (g Globals) cmdRemote(args []string) int {
	bin := g.app.BinName
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s remote <ssh-host> [up|down] [--room R]\n", bin)
		return 2
	}
	helper := findRemoteHelper()
	if helper == "" {
		fmt.Fprintf(os.Stderr, "%s remote: could not locate remote.sh — set CPC_REMOTE_HELPER, "+
			"run scripts/install.sh (installs it to ~/.local/libexec/cpc/), or run from the repo\n", bin)
		return 1
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		bash = "/bin/bash"
	}
	// The global --room was already consumed by parseGlobals, so forward it
	// explicitly to the engine (both daemons must share the room).
	room := g.Room
	if room == "" {
		room = "default"
	}
	argv := append([]string{bash, helper, bin}, args...)
	argv = append(argv, "--room", room)
	// exec replaces this process; only returns on error.
	if err := syscall.Exec(bash, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "%s remote: exec failed: %v\n", bin, err)
		return 1
	}
	return 0
}

// findRemoteHelper resolves scripts/remote.sh: an env override, next to the
// installed binary, ~/.local/libexec/cpc/, or by walking up for the repo.
func findRemoteHelper() string {
	if p := os.Getenv("CPC_REMOTE_HELPER"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(dir, "remote.sh"),
			filepath.Join(dir, "..", "libexec", "cpc", "remote.sh"),
		} {
			if fi, e := os.Stat(c); e == nil && !fi.IsDir() {
				return c
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		c := filepath.Join(home, ".local", "libexec", "cpc", "remote.sh")
		if fi, e := os.Stat(c); e == nil && !fi.IsDir() {
			return c
		}
	}
	if wd, err := os.Getwd(); err == nil {
		for d := wd; ; {
			c := filepath.Join(d, "scripts", "remote.sh")
			if fi, e := os.Stat(c); e == nil && !fi.IsDir() {
				return c
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return ""
}
