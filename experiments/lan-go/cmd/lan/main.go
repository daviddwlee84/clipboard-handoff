// Command lan is the lan-go experiments-track probe: the shared clipboard
// hand-off CLI (docs/SPEC.md) over a hand-rolled LAN mesh — quic-go direct
// connections + mDNS/DNS-SD discovery (see internal/lantransport). The CLI,
// IPC, ring buffer, auto-copy and clipboard all come from shared-go; this
// binary only plugs in the transport. See README.md for the exact commands the
// bake-off harness uses.
package main

import (
	"os"

	"github.com/daviddwlee84/cross-platform-copy/experiments/lan-go/internal/lantransport"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/cli"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/transport"
)

func main() {
	app := cli.App{
		BinName:      "lan",
		Tagline:      "clipboard hand-off over quic-go + mDNS",
		TransportTag: "quic+mdns",
		NewTransport: func(cfg *config.Store, room, deviceName string) (transport.Transport, error) {
			return lantransport.New(cfg, room, deviceName)
		},
	}
	os.Exit(app.Main())
}
