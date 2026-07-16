// Package http provides a strict, bounded HTTP/1.0 and HTTP/1.1 parser with a
// zero-allocation ordinary hot path, and selectively registers the TCP-backed
// Wago HTTP/1 capability. The parser core is implemented; the guest-visible
// network exchange ABI remains introspection-only until lifecycle binding lands.
package http

import (
	wagohttp "github.com/wago-org/http"
	"github.com/wago-org/http/internal/plugin"
	"github.com/wago-org/http/internal/stubabi"
	wagonet "github.com/wago-org/net"
	nettcp "github.com/wago-org/net/tcp"
	wago "github.com/wago-org/wago"
)

const (
	Module                      = "wago_http1"
	CapHTTP     wago.Capability = "http.http1"
	ABIVersion1                 = stubabi.ABIVersion1
)

// Register selects the HTTP/1.1 capability and its TCP transport requirement.
func Register(network *wagohttp.Network) error {
	module := plugin.NewModule(
		plugin.KeyHTTP1,
		Module,
		CapHTTP,
		"inspect the HTTP/1 capability; native bounded parsing is implemented and guest exchanges are pending",
		plugin.NewTransport(plugin.TransportTCP, func(network *wagonet.Network) error { return nettcp.Register(network) }),
		plugin.Binding{Name: "abi_version", Func: stubabi.ABIVersion, Results: []wago.ValType{wago.ValI32}, Docs: "return the HTTP/1 ABI version"},
		plugin.Binding{Name: "feature_flags", Func: stubabi.FeatureFlags, Results: []wago.ValType{wago.ValI64}, Docs: "return implemented HTTP/1.1 feature bits; zero means the data path is not implemented"},
	)
	return network.RegisterModule(module)
}
