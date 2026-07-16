# Wago HTTP/2 ABI v1

Module: `wago_http2`  
Capability: `http.http2`  
Transport capability: `wago_net_tcp`

The ABI is a bounded HTTP/2 protocol engine over caller-supplied bytes. It does not dial, own a TCP handle, configure TLS, or perform ALPN. A guest obtains a TCP resource from `wago-org/net`, copies bytes returned by `session_output` to that resource, feeds received bytes to `session_feed`, and uses the network plugin's readiness operations for polling. TLS and ALPN belong in `github.com/wago-org/net`.

All pointers and lengths are guest-memory `i32` values. Multi-byte structures use little-endian integers. Wire bytes remain HTTP/2 network byte order. Every memory range is checked before use. A status is one of the shared `wago-org/net` statuses. In particular, `OK` means progress, `AGAIN` means output/events or flow-control credit are not currently available, `BAD_HANDLE` identifies an unknown session/stream, `RESOURCE_LIMIT` identifies a configured finite quota, and `INVALID_ARGUMENT`/`INVALID_STATE` identify caller errors.

Each Wago instance owns an isolated session-handle table. `WithMaxSessions` bounds it (default 8), `WithSessionLimits` bounds every protocol-controlled resource, instance close releases all sessions, runtime close releases all instances, and the module requires reinstantiation to prevent state reuse across guest instances.

## Feature flags

`feature_flags() -> i64` returns:

| Bit | Constant | Meaning |
|---:|---|---|
| 0 | `FeatureSessionEngine` | Stateful client/server connection engine |
| 1 | `FeatureMultiplexedStreams` | Concurrent HTTP/2 streams |
| 2 | `FeatureServer` | Server-role sessions and responses |
| 3 | `FeaturePush` | PUSH_PROMISE and pushed responses |
| 4 | `FeatureExtendedConnect` | RFC 8441 extended CONNECT |
| 5 | `FeaturePriorityUpdate` | RFC 9218 PRIORITY_UPDATE |

## Guest structures

### Header entry (`ABIHeaderV1Size == 24`)

| Offset | Type | Field |
|---:|---|---|
| 0 | `u32` | name pointer |
| 4 | `u32` | name length |
| 8 | `u32` | value pointer |
| 12 | `u32` | value length |
| 16 | `u32` | flags; bit 0 is `ABIFlagSensitive` |
| 20 | `u32` | reserved; write zero |

Names and values are copied into host-owned bounded field strings before the host call returns. Header counts, individual fields, and RFC header-list accounting are checked against `HeaderLimits`.

### Event (`ABIEventV1Size == 32`)

| Offset | Type | Field |
|---:|---|---|
| 0 | `u32` | `EventType` |
| 4 | `u32` | stream ID, or zero for a connection event |
| 8 | `u32` | flags: bit 0 `END_STREAM`, bit 1 trailers |
| 12 | `u32` | HTTP/2 error code |
| 16 | `u32` | GOAWAY last-stream ID, or WINDOW_UPDATE increment |
| 20 | `u32` | data length; PING/PING_ACK is 8 |
| 24 | `u32` | header count |
| 28 | `u32` | setting count |

`event_next` commits one current event. Its data, headers, and settings remain readable through `event_data`, `event_header`, and `event_setting` until the next successful `event_next` on that session.

### Setting (`ABISettingV1Size == 8`)

| Offset | Type | Field |
|---:|---|---|
| 0 | `u32` | setting identifier |
| 4 | `u32` | setting value |

## Imports

Signatures below use `u32` for guest `i32` values and `status` for the returned shared status code.

- `abi_version() -> i32`
- `feature_flags() -> i64`
- `session_open(role, out_handle_ptr) -> status`
- `session_close(handle) -> status`
- `session_feed(handle, src_ptr, src_len, out_consumed_ptr) -> status`
- `session_output(handle, dst_ptr, dst_cap, out_written_ptr) -> status`
- `stream_open(handle, headers_ptr, header_count, flags, out_stream_id_ptr) -> status`
- `stream_headers(handle, stream_id, headers_ptr, header_count, flags) -> status`
- `stream_data(handle, stream_id, src_ptr, src_len, flags, out_consumed_ptr) -> status`
- `stream_push(handle, parent_stream_id, headers_ptr, header_count, out_promised_stream_id_ptr) -> status`
- `stream_reset(handle, stream_id, error_code) -> status`
- `stream_priority_update(handle, stream_id, value_ptr, value_len) -> status`
- `session_settings(handle, settings_ptr, setting_count) -> status`
- `session_ping(handle, eight_byte_ptr) -> status`
- `session_goaway(handle, error_code, debug_ptr, debug_len) -> status`
- `event_next(handle, event_ptr) -> status`
- `event_data(handle, dst_ptr, dst_cap, out_written_ptr) -> status`
- `event_header(handle, index, name_ptr, name_cap, out_name_len_ptr, value_ptr, value_cap, out_value_len_ptr) -> status`
- `event_setting(handle, index, setting_ptr) -> status`

Only `ABIFlagEndStream` is accepted by stream-opening, header, and data operations. `stream_data` reports partial consumption when peer flow control or the bounded output queue prevents a complete write. The guest must drain output, wait for and feed transport input, process WINDOW_UPDATE events, and retry the unsent suffix.

## Connection sequence

1. Open a client or server session. The client immediately queues the connection preface and initial SETTINGS; the server queues initial SETTINGS.
2. Repeatedly drain `session_output` into the selected TCP resource until `AGAIN`.
3. Feed each received TCP span with `session_feed`, respecting `out_consumed`.
4. Drain and dispatch events with `event_next`.
5. Open/send streams, drain newly queued output, and repeat.
6. Use GOAWAY for graceful shutdown and `session_close` for deterministic release.

A server session validates the exact client connection preface before frames. A client session requires the peer's initial SETTINGS before non-SETTINGS traffic. Protocol failures are sticky, queue a bounded GOAWAY when possible, and cause subsequent mutating calls to return an invalid-state or I/O status.
