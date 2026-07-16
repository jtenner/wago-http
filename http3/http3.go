// Package http3 selectively registers the HTTP/3 Wago plugin scaffold.
// The selected module reserves a UDP-backed capability and exposes only
// allocation-free ABI/feature introspection until QUIC, frames, and QPACK land.
package http3

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/internal/plugin"
	"github.com/wago-org/http/internal/stubabi"
	wagonet "github.com/wago-org/net"
	netudp "github.com/wago-org/net/udp"
	wago "github.com/wago-org/wago"
)

const (
	Module                      = "wago_http3"
	CapHTTP3    wago.Capability = "http.http3"
	ABIVersion1                 = stubabi.ABIVersion1
)

// Register selects the HTTP/3 capability and its UDP transport requirement.
func Register(network *wagohttp.Network) error {
	module := plugin.NewModule(
		plugin.KeyHTTP3,
		Module,
		CapHTTP3,
		"inspect and, when implemented, use bounded HTTP/3 streams",
		plugin.NewTransport(plugin.TransportUDP, func(network *wagonet.Network) error { return netudp.Register(network) }),
		plugin.Binding{Name: "abi_version", Func: stubabi.ABIVersion, Results: []wago.ValType{wago.ValI32}, Docs: "return the HTTP/3 scaffold ABI version"},
		plugin.Binding{Name: "feature_flags", Func: stubabi.FeatureFlags, Results: []wago.ValType{wago.ValI64}, Docs: "return implemented HTTP/3 feature bits; zero means the data path is not implemented"},
	)
	return network.RegisterModule(module)
}
