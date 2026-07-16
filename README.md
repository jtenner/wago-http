# Wago HTTP Protocols

Capability-gated HTTP-family plugins for the [Wago](https://github.com/wago-org/wago) WebAssembly runtime, layered on [wago-org/net](https://github.com/wago-org/net).

> [!WARNING]
> This repository is experimental. The strict, bounded HTTP/1.0 and HTTP/1.1 parser, HTTP/2 frame parser, bounded HPACK decoder, and native HTTP/1 and HTTP/2 single-exchange clients are implemented. Guest-visible network exchanges, QPACK, WebSocket, HTTP/3, and QUIC data paths are not yet implemented. Every protocol's guest `feature_flags` import therefore still returns zero.

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

`Parser.ParseOne` stops at exactly one message boundary, leaving pipelined or upgraded-protocol bytes unconsumed. The regular `Parse` method retains its existing pipeline-consuming behavior.

## HTTP/1 requests

The `http/request` package validates and writes fixed-length HTTP/1.1 requests, then can parse one synchronous exchange over any caller-owned connected byte stream. It deliberately does not dial, close, or set deadlines, so the caller retains transport authority and can use a plain TCP connection, test stream, or a future `wago-org/net` stream adapter. This repository does not currently provide TLS, so HTTPS requests are not supported by this submodule.

```go
conn, err := net.DialTimeout("tcp", "example.test:80", 5*time.Second)
if err != nil {
    return err
}
defer conn.Close()

response, err := request.Client{}.Do(conn, request.Request{
    Method: []byte("GET"),
    Target: []byte("/health"),
    Host:   []byte("example.test"),
}, &http1.Callbacks{
    Body: func(fragment []byte) {
        // fragment borrows the client's read buffer.
    },
})
if err != nil {
    return err
}
_ = response.Message.Status
```

`Host`, `Content-Length`, and `Transfer-Encoding` are writer-managed to reject request smuggling and header injection. `Client.DoBuffer` accepts caller-owned scratch memory and returns bytes read beyond the final response boundary, including protocol-upgrade bytes. This is a native Go protocol engine; the guest-visible Wago request exchange ABI remains pending lifecycle binding to `wago-org/net`.

## HTTP/2 frames and HPACK

The `http2` package includes an incremental, allocation-free frame parser for DATA, HEADERS, PRIORITY, RST_STREAM, SETTINGS, PUSH_PROMISE, PING, GOAWAY, WINDOW_UPDATE, CONTINUATION, and extension frames. It validates frame sizes, stream identifiers, padding, fixed fields, SETTINGS values, priority dependencies, continuation sequencing, and bounded cumulative header-block work under arbitrary input fragmentation. Payload callbacks borrow cap-limited input spans.

```go
parser := http2.NewParser(&http2.Callbacks{
    Data: func(streamID uint32, fragment []byte, endStream bool) {
        // fragment borrows the Parse input.
    },
    HeaderBlock: func(streamID uint32, fragment []byte) {
        // Feed the fragment to a persistent HeaderDecoder.
    },
}, http2.Limits{})
consumed, code := parser.Parse(input)
```

`http2.HeaderDecoder` wraps a persistent HPACK compression context with finite dynamic-table, field, header-count, and decoded header-list limits. `BeginBlock`, fragmented `Write` calls, and `EndBlock` preserve dynamic-table state across blocks.

## HTTP/2 requests

The `http2/request` package emits the client connection preface, bounded SETTINGS, HPACK request headers, CONTINUATION frames, and flow-controlled DATA on stream 1. Its synchronous client requires the server's initial SETTINGS frame, acknowledges SETTINGS and PING, replenishes connection and stream receive windows, rejects server push and malformed response field sections, handles informational responses and trailers, and returns exactly at the final stream boundary.

```go
response, err := request.Client{}.Do(conn, request.Request{
    Method:    []byte("GET"),
    Scheme:    []byte("https"),
    Authority: []byte("example.test"),
    Path:      []byte("/health"),
}, &request.Callbacks{
    Body: func(fragment []byte) {
        // fragment borrows the client's read buffer.
    },
})
```

The package owns neither dialing nor TLS. The caller must provide an already connected HTTP/2 transport with appropriate ALPN or prior-knowledge negotiation. Request bodies are currently limited to the 65,535-byte initial peer flow-control window because the one-shot client writes the request before reading peer WINDOW_UPDATE frames.

## Architecture direction

The structure follows `wago-org/net`'s selective plugin model:

- the root owns one shared builder and one underlying `wago-org/net` extension;
- protocol packages contribute opaque descriptors through `internal/plugin`;
- registration freezes before Wago sees the extension;
- required TCP and UDP transports are installed once regardless of protocol order;
- unselected HTTP protocol modules are absent from the guest import surface;
- placeholder host calls are fixed-shape and allocation-free;
- the HTTP/1 and HTTP/2 parsers use finite counters, borrowed spans, sticky failures, and no parser-owned payload buffers;
- the HPACK and request layers impose explicit dynamic-table, header-list, body, frame, continuation, and flow-control bounds.

The next implementation stages should preserve compile-time protocol isolation and bind the tested HTTP/1 and HTTP/2 state machines to finite per-instance `wago-org/net` resources before implementing the independently selectable WebSocket and HTTP/3 paths.

## Conformance corpora

Pinned opportunities for HTTP/1.1, HTTP/2 and HPACK, WebSocket, HTTP/3, QPACK, and QUIC interoperability are documented in [`docs/test-corpora.md`](docs/test-corpora.md). Fetch them into ignored local storage with:

```sh
scripts/fetch-test-corpora.sh
```

No third-party corpus is vendored by default. When the pinned checkouts are present, `go test ./http` automatically runs RFC-audited adapters for llhttp, picohttpparser, and httparse—each fixture at every input split point. `go test ./http2` additionally runs all 47,142 encoded cases from the pinned HPACK interoperability corpus under contiguous, byte-at-a-time, and patterned fragmentation. Repository-owned HTTP/2 tests cover every frame family, continuation sequencing, SETTINGS, padding, stream identifiers, resource limits, truncation, callback safety, lifecycle, request/response streaming, control-frame acknowledgements, flow control, fuzzing, benchmarks, and allocation behavior.

## Development

```sh
go test ./...
go test -shuffle=on -count=1 ./...
go vet ./...
```

The reproducible ten-sample HTTP/2 performance baseline, environment details, median tables, observed ranges, allocation counts, and raw-result comparison procedure are recorded in [`BENCHMARKS.md`](BENCHMARKS.md).

The module temporarily replaces unpublished Wago `v0.1.0` with the exact production lifecycle merge selected by the pinned `wago-org/net` source.

## License

Apache-2.0.
