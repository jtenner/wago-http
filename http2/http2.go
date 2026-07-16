// Package http2 provides a strict bounded incremental HTTP/2 frame parser and
// HPACK decoder, and selectively registers the TCP-backed Wago HTTP/2 plugin.
// The native protocol core is implemented; guest-visible exchanges remain
// introspection-only until lifecycle binding lands.
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
		"inspect the HTTP/2 capability; native bounded frames and HPACK are implemented and guest exchanges are pending",
		plugin.NewTransport(plugin.TransportTCP, func(network *wagonet.Network) error { return nettcp.Register(network) }),
		plugin.Binding{Name: "abi_version", Func: stubabi.ABIVersion, Results: []wago.ValType{wago.ValI32}, Docs: "return the HTTP/2 scaffold ABI version"},
		plugin.Binding{Name: "feature_flags", Func: stubabi.FeatureFlags, Results: []wago.ValType{wago.ValI64}, Docs: "return guest-visible HTTP/2 feature bits; zero means lifecycle binding is not implemented"},
	)
	return network.RegisterModule(module)
}
