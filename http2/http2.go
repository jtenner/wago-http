// Package http2 provides a strict bounded HTTP/2 frame parser, HPACK codec,
// client/server session engine, and lifecycle-bound Wago guest ABI. Transport
// dialing, TLS, and ALPN remain caller and wago-org/net responsibilities.
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

// Register selects the HTTP/2 capability, bounded guest session engine, and
// TCP transport requirement with finite defaults. TLS remains separate.
func Register(network *wagohttp.Network) error {
	return RegisterWithOptions(network)
}

// RegisterWithOptions selects HTTP/2 with explicit guest session bounds.
func RegisterWithOptions(network *wagohttp.Network, options ...Option) error {
	config := registerConfig{}
	for _, option := range options {
		if option != nil {
			option.applyHTTP2(&config)
		}
	}
	manager := newABIManager(config)
	module := plugin.NewModule(
		plugin.KeyHTTP2,
		Module,
		CapHTTP2,
		"use bounded HTTP/2 client and server sessions over capability-gated TCP bytes",
		plugin.NewTransport(plugin.TransportTCP, func(network *wagonet.Network) error { return nettcp.Register(network) }),
		plugin.Binding{Name: "abi_version", Func: abiVersion, Results: []wago.ValType{wago.ValI32}, Docs: "return the HTTP/2 ABI version"},
		plugin.Binding{Name: "feature_flags", Func: abiFeatureFlags, Results: []wago.ValType{wago.ValI64}, Docs: "return implemented HTTP/2 session feature bits"},
		plugin.Binding{Name: "session_open", Func: manager.sessionOpen, Params: []wago.ValType{wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "create a bounded client or server HTTP/2 session"},
		plugin.Binding{Name: "session_close", Func: manager.sessionClose, Params: []wago.ValType{wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "close an HTTP/2 session handle"},
		plugin.Binding{Name: "session_feed", Func: manager.sessionFeed, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "feed received transport bytes into a session"},
		plugin.Binding{Name: "session_output", Func: manager.sessionOutput, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "drain queued HTTP/2 wire bytes"},
		plugin.Binding{Name: "stream_open", Func: manager.streamOpen, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "open a client stream with initial headers"},
		plugin.Binding{Name: "stream_headers", Func: manager.streamHeaders, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "send response headers or trailers"},
		plugin.Binding{Name: "stream_data", Func: manager.streamData, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "send flow-controlled DATA"},
		plugin.Binding{Name: "stream_push", Func: manager.streamPush, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "reserve a server-pushed stream"},
		plugin.Binding{Name: "stream_reset", Func: manager.streamReset, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "reset one stream"},
		plugin.Binding{Name: "stream_priority_update", Func: manager.streamPriorityUpdate, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "send RFC 9218 priority metadata"},
		plugin.Binding{Name: "session_settings", Func: manager.sessionSettings, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "send a bounded SETTINGS update"},
		plugin.Binding{Name: "session_ping", Func: manager.sessionPing, Params: []wago.ValType{wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "send a PING"},
		plugin.Binding{Name: "session_goaway", Func: manager.sessionGoAway, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "start graceful GOAWAY shutdown"},
		plugin.Binding{Name: "event_next", Func: manager.eventNext, Params: []wago.ValType{wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "read the next committed session event"},
		plugin.Binding{Name: "event_data", Func: manager.eventData, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "copy the current event payload"},
		plugin.Binding{Name: "event_header", Func: manager.eventHeader, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "copy one current-event header field"},
		plugin.Binding{Name: "event_setting", Func: manager.eventSetting, Params: []wago.ValType{wago.ValI32, wago.ValI32, wago.ValI32}, Results: []wago.ValType{wago.ValI32}, Docs: "copy one current-event setting"},
	).WithRegistry(manager.configure)
	return network.RegisterModule(module)
}
