# Wago HTTP Protocols

Capability-gated HTTP-family plugins for the [Wago](https://github.com/wago-org/wago) WebAssembly runtime, layered on [wago-org/net](https://github.com/wago-org/net).

> [!WARNING]
> This repository is experimental. HTTP/2 now includes a bounded RFC 9113 client/server session engine, persistent multiplexed client transport, non-TLS server endpoint, and guest-visible Wago ABI. HTTP/1 parsing and native exchanges are implemented, but its guest exchange ABI remains pending. QPACK, WebSocket, HTTP/3, and QUIC data paths are not yet implemented. TLS and HTTP/2 ALPN are deliberately owned by `github.com/wago-org/net`, not this repository.

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

The `http2` package includes an incremental, allocation-free frame parser for DATA, HEADERS, PRIORITY, RST_STREAM, SETTINGS, PUSH_PROMISE, PING, GOAWAY, WINDOW_UPDATE, CONTINUATION, and extension frames. It validates frame sizes, stream identifiers, padding, fixed fields, SETTINGS values, priority dependencies, continuation sequencing, and bounded cumulative header-block work under arbitrary input fragmentation. Payload callbacks borrow cap-limited input spans. Above it, `http2.Session` implements persistent client and server connections with SETTINGS negotiation, stream-state enforcement, multiplexing, bidirectional flow control, persistent HPACK contexts, informational responses, trailers, push, CONNECT and extended CONNECT, GOAWAY, cancellation, and RFC 9218 priority updates.

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

## HTTP/2 clients and servers

The `http2/request` package retains a small one-shot stream-1 client and adds `Transport`, a persistent concurrent client over a caller-owned byte stream. `Transport` reuses HPACK state, allocates odd stream IDs, multiplexes concurrent `Do` calls, streams request bodies and trailers under peer flow control, supports cancellation, bounds response bodies and event queues, and optionally accepts server push. The `http2/server` package provides the corresponding non-TLS connection runner, concurrent-safe response writers, streaming responses, push, resets, and writable notifications after WINDOW_UPDATE.

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

The packages own neither dialing nor TLS. Callers provide an established byte stream selected through cleartext HTTP/2 prior knowledge or TLS/ALPN in `github.com/wago-org/net`; HTTP/1.1 `Upgrade: h2c` is intentionally not exposed because capability selection happens before the HTTP/2 session starts. The legacy one-shot client remains limited to the initial 65,535-byte send window; `Transport` and `server.Conn` perform concurrent input/output and support bodies and responses larger than the initial window. `request.Pool` lazily dials persistent transports and retries replayable idempotent requests refused before response headers on a fresh connection.

## Wago HTTP/2 ABI

Selecting `http2.Register` installs 19 imports in `wago_http2`, reports real feature flags, requires the shared TCP capability, and creates isolated bounded session handles per Wago instance. Guests shuttle wire bytes between `session_output`/`session_feed` and `wago_net_tcp`, use network readiness for polling, and consume typed protocol events for headers, data, settings, resets, GOAWAY, push, and flow-control updates. Instance and runtime lifecycle hooks deterministically release all sessions. The complete v1 memory layouts, signatures, status behavior, and connection sequence are documented in [`docs/http2-abi-v1.md`](docs/http2-abi-v1.md).

## Architecture direction

The structure follows `wago-org/net`'s selective plugin model:

- the root owns one shared builder and one underlying `wago-org/net` extension;
- protocol packages contribute opaque descriptors through `internal/plugin`;
- registration freezes before Wago sees the extension;
- required TCP and UDP transports are installed once regardless of protocol order;
- unselected HTTP protocol modules are absent from the guest import surface;
- guest imports use fixed memory layouts, checked ranges, bounded handle tables, and deterministic lifecycle cleanup;
- the HTTP/1 and HTTP/2 parsers use finite counters, borrowed spans, sticky failures, and no parser-owned payload buffers;
- the HTTP/2 session, HPACK, client, server, and ABI layers impose explicit stream, dynamic-table, header-list, body, frame, continuation, output, event, and control-frame bounds.

The remaining protocol work is the HTTP/1 guest exchange ABI and the independently selectable WebSocket and HTTP/3 paths. TLS sockets, certificate policy, and ALPN negotiation remain a separate `wago-org/net` responsibility.

## Conformance corpora

Pinned opportunities for HTTP/1.1, HTTP/2 and HPACK, WebSocket, HTTP/3, QPACK, and QUIC interoperability are documented in [`docs/test-corpora.md`](docs/test-corpora.md). Fetch them into ignored local storage with:

```sh
scripts/fetch-test-corpora.sh
```

No third-party corpus is vendored by default. When the pinned checkouts are present, `go test ./http` automatically runs RFC-audited adapters for llhttp, picohttpparser, and httparse—each fixture at every input split point. `go test ./http2` additionally runs all 47,142 encoded cases from the pinned HPACK interoperability corpus under contiguous, byte-at-a-time, and patterned fragmentation. Repository-owned HTTP/2 tests cover every frame family, connection and stream state, SETTINGS, padding, stream identifiers, resource limits, lifecycle, multiplexed client/server exchanges, large flow-controlled bodies, Wago ABI cleanup, fuzzing, benchmarks, and allocation behavior. `scripts/run-h2spec.sh http2` starts the included prior-knowledge endpoint and runs the pinned strict h2spec suite; TLS is not involved.

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
