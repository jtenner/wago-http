// Package http2 selectively registers the HTTP/2 Wago plugin scaffold.
// The selected module reserves a TCP-backed capability and exposes only
// allocation-free ABI/feature introspection until frames and HPACK land.
package http2

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/internal/plugin"
	"github.com/wago-org/http/internal/stubabi"
	wagonet "github.com/wago-org/net"
	nettcp "github.com/wago-org/net/tcp"
	wago "github.com/wago-org/wago"
)

const (
	Module                      = "wago_http2"
	CapHTTP2    wago.Capability = "http.http2"
	ABIVersion1                 = stubabi.ABIVersion1
)

// Register selects the HTTP/2 capability and its TCP transport requirement.
func Register(network *wagohttp.Network) error {
	module := plugin.NewModule(
		plugin.KeyHTTP2,
		Module,
		CapHTTP2,
		"inspect and, when implemented, use bounded HTTP/2 streams",
		plugin.NewTransport(plugin.TransportTCP, func(network *wagonet.Network) error { return nettcp.Register(network) }),
		plugin.Binding{Name: "abi_version", Func: stubabi.ABIVersion, Results: []wago.ValType{wago.ValI32}, Docs: "return the HTTP/2 scaffold ABI version"},
		plugin.Binding{Name: "feature_flags", Func: stubabi.FeatureFlags, Results: []wago.ValType{wago.ValI64}, Docs: "return implemented HTTP/2 feature bits; zero means the data path is not implemented"},
	)
	return network.RegisterModule(module)
}
