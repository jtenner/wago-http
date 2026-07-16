# Wago HTTP Protocols

Capability-gated HTTP-family plugins for the [Wago](https://github.com/wago-org/wago) WebAssembly runtime, layered on [wago-org/net](https://github.com/wago-org/net).

> [!WARNING]
> This repository is experimental. The strict, bounded HTTP/1.0 and HTTP/1.1 parser core is implemented and allocation-free on its hot path. Guest-visible network exchanges, HTTP/2, HPACK/QPACK, WebSocket, HTTP/3, and QUIC data paths are not yet implemented. Every protocol's guest `feature_flags` import therefore still returns zero.

## Protocol packages

| Package | Wago import module | Capability | Required net transport |
|---|---|---|---|
| `http` | `wago_http1` | `http.http1` | TCP |
| `http2` | `wago_http2` | `http.http2` | TCP |
| `websocket` | `wago_websocket` | `http.websocket` | TCP |
| `http3` | `wago_http3` | `http.http3` | UDP |

The root builder installs the shared `wago_http.abi_version` import and deduplicates transport selection. For example, selecting HTTP/1.1, HTTP/2, and WebSocket registers `wago_net_tcp` only once.

## Selective composition

```go
package main

import (
    wagohttp "github.com/wago-org/http"
    http1 "github.com/wago-org/http/http"
    "github.com/wago-org/http/http2"
    wago "github.com/wago-org/wago"
)

func runtime() (*wago.Runtime, error) {
    protocols := wagohttp.New()
    if err := http1.Register(protocols); err != nil {
        return nil, err
    }
    if err := http2.Register(protocols); err != nil {
        return nil, err
    }
    runtime := wago.NewRuntime()
    if err := runtime.Use(protocols); err != nil {
        return nil, err
    }
    return runtime, nil
}
```

Protocol-local self-registration packages are available for custom Wago binaries:

```go
import _ "github.com/wago-org/http/http/register"      // extension key: http
import _ "github.com/wago-org/http/http2/register"     // extension key: http2
import _ "github.com/wago-org/http/websocket/register" // extension key: websocket
import _ "github.com/wago-org/http/http3/register"     // extension key: http3
import _ "github.com/wago-org/http/register"           // extension key: http-all
```

## HTTP/1 parser

The `http` protocol package includes an incremental parser that retains no input buffers, rejects ambiguous framing, enforces finite limits, handles fixed-length, chunked, close-delimited, pipelined, CONNECT, and upgrade messages, and supports arbitrary input fragmentation. Request-side CONNECT and Upgrade are reported as metadata requests; `CodeUpgrade` is reserved for a validated `101` response or successful CONNECT response, where following bytes actually belong to the switched protocol.

```go
callbacks := http1.Callbacks{
    Body: func(fragment []byte) {
        // fragment borrows the input passed to Parse.
    },
    MessageComplete: func(message http1.Message) {
        // message is an allocation-free metadata snapshot.
    },
}
parser := http1.NewParser(http1.Request, &callbacks, http1.Limits{})
consumed, code := parser.Parse(input)
if code != http1.CodeNone && code != http1.CodeUpgrade {
    // Reject the message. Parse errors remain sticky until Reset or Init.
}
_ = consumed
```

Zero-valued limits select conservative finite defaults, including cumulative chunk-count and chunk-metadata quotas. Callback spans are read-only and cap-limited; reentrant parser calls are rejected. Because validation is streaming, consumers should defer externally visible side effects until `MessageComplete`. Ordinary parsing, including no-op callbacks and arbitrary fragmentation, performs zero heap allocations. Bracketed IPv6 literals delegate validation to the same `net/netip` representation used by `wago-org/net` and currently cost one allocation per validated literal; callback allocation behavior remains controlled by the caller.

## Architecture direction

The structure follows `wago-org/net`'s selective plugin model:

- the root owns one shared builder and one underlying `wago-org/net` extension;
- protocol packages contribute opaque descriptors through `internal/plugin`;
- registration freezes before Wago sees the extension;
- required TCP and UDP transports are installed once regardless of protocol order;
- unselected HTTP protocol modules are absent from the guest import surface;
- placeholder host calls are fixed-shape and allocation-free;
- the HTTP/1 parser uses finite counters, borrowed spans, sticky failures, and no parser-owned buffers.

The next implementation stages should preserve compile-time protocol isolation and bind the tested HTTP/1 state machine to finite per-instance `wago-org/net` resources before implementing the independently selectable HTTP/2, WebSocket, and HTTP/3 paths.

## Conformance corpora

Pinned opportunities for HTTP/1.1, HTTP/2 and HPACK, WebSocket, HTTP/3, QPACK, and QUIC interoperability are documented in [`docs/test-corpora.md`](docs/test-corpora.md). Fetch them into ignored local storage with:

```sh
scripts/fetch-test-corpora.sh
```

No third-party corpus is vendored by default. When the pinned llhttp checkout is present, `go test ./http` automatically runs the RFC-aligned corpus adapter in addition to repository-owned fragmentation, limits, smuggling, lifecycle, fuzz, benchmark, and allocation tests.

## Development

```sh
go test ./...
go test -shuffle=on -count=1 ./...
go vet ./...
```

The module temporarily replaces unpublished Wago `v0.1.0` with the exact production lifecycle merge selected by the pinned `wago-org/net` source.

## License

Apache-2.0.
