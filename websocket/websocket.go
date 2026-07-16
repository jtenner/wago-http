// Package websocket selectively registers the WebSocket Wago plugin scaffold.
// The selected module reserves a TCP-backed capability and exposes only
// allocation-free ABI/feature introspection until handshakes and frames land.
package websocket

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/internal/plugin"
	"github.com/wago-org/http/internal/stubabi"
	wagonet "github.com/wago-org/net"
	nettcp "github.com/wago-org/net/tcp"
	wago "github.com/wago-org/wago"
)

const (
	Module                       = "wago_websocket"
	CapWebSocket wago.Capability = "http.websocket"
	ABIVersion1                  = stubabi.ABIVersion1
)

// Register selects the WebSocket capability and its TCP transport requirement.
func Register(network *wagohttp.Network) error {
	module := plugin.NewModule(
		plugin.KeyWebSocket,
		Module,
		CapWebSocket,
		"inspect and, when implemented, use bounded WebSocket connections",
		plugin.NewTransport(plugin.TransportTCP, func(network *wagonet.Network) error { return nettcp.Register(network) }),
		plugin.Binding{Name: "abi_version", Func: stubabi.ABIVersion, Results: []wago.ValType{wago.ValI32}, Docs: "return the WebSocket scaffold ABI version"},
		plugin.Binding{Name: "feature_flags", Func: stubabi.FeatureFlags, Results: []wago.ValType{wago.ValI64}, Docs: "return implemented WebSocket feature bits; zero means the data path is not implemented"},
	)
	return network.RegisterModule(module)
}
