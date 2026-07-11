// Command libp2p-mesh is the libp2p-mesh experiments-track probe: the shared
// clipboard hand-off CLI (docs/SPEC.md) over a go-libp2p gossipsub mesh with
// libp2p mDNS LAN discovery (see internal/meshtransport). The CLI, IPC, ring
// buffer, auto-copy and clipboard all come from shared-go; this binary only
// plugs in the transport. See README.md for the exact bake-off commands.
package main

import (
	"os"

	"github.com/daviddwlee84/cross-platform-copy/experiments/libp2p-mesh/internal/meshtransport"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/cli"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/config"
	"github.com/daviddwlee84/cross-platform-copy/experiments/shared-go/transport"
)

func main() {
	app := cli.App{
		BinName:      "libp2p-mesh",
		Tagline:      "clipboard hand-off over go-libp2p gossipsub + mDNS",
		TransportTag: "gossipsub+mdns",
		NewTransport: func(cfg *config.Store, room, deviceName string) (transport.Transport, error) {
			return meshtransport.New(cfg, room, deviceName)
		},
	}
	os.Exit(app.Main())
}
