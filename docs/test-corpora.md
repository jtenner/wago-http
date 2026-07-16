# Test corpus plan

No single external suite is authoritative for every requirement. Imported cases must be mapped to the current RFC text, reviewed for intentional implementation leniency, and supplemented with repository-owned regression, fragmentation, resource-bound, lifecycle, differential, and fuzz tests.

The exact source revisions and licenses are pinned in [`testdata/corpora.lock`](../testdata/corpora.lock). `scripts/fetch-test-corpora.sh` checks them out under ignored `testdata/upstream/`; CI should eventually verify the pins and run adapters without committing third-party trees.

## HTTP/1.1

Primary specifications: RFC 9110 semantics and RFC 9112 message syntax and routing.

Candidate: `nodejs/llhttp` (`test/request`, `test/response`, `test/fixtures`, and `test/fuzzers`; MIT). Its fixtures cover request and response parsing, methods, URI forms, connection handling, content length, transfer encoding, chunking, pipelining, upgrades, invalid syntax, pause/resume, and leniency modes.

Current integration:

1. `http/corpus_test.go` loads the pinned llhttp Markdown fixtures directly when `testdata/upstream/llhttp` is present and validates more than 100 strict, RFC-aligned request and response cases contiguously and at every input split point.
2. Explicit adapter classifications exclude llhttp legacy protocol support, method-whitelist behavior, obsolete line folding, missing HTTP/1.1 Host enforcement, skip-body hooks, and tolerant response separators from the Wago oracle.
3. Repository-owned tests cover framing precedence and request-smuggling regressions, byte-at-a-time feeds, every split point, informational/final response association, enforced callback reentrancy guards, cap-limited spans, cumulative chunk quotas, sticky failures, upgrades, trailers, truncation, independently fuzzed segmentation, and zero-allocation hot paths.
4. `http/parser_benchmark_test.go` provides request and response edge-case matrices across contiguous, 64-byte, 16-byte, and byte-at-a-time delivery, plus callback-overhead, resource-limit, truncation, and malformed-framing benchmarks.

Run the detailed parser benchmark suite with:

```sh
go test ./http -run '^$' -bench 'BenchmarkParser' -benchmem -count=5
```
4. Further differential testing should add another mature parser while keeping current RFC text as the oracle when implementations disagree.

## HTTP/2 and HPACK

Primary specifications: RFC 9113 for HTTP/2 and RFC 7541 for HPACK.

Candidates:

- `summerwind/h2spec` (MIT), a runnable server conformance tool with frame, stream state, flow control, settings, error handling, message exchange, and HPACK cases. It targets RFC 7540, so each test must be audited against RFC 9113 before becoming normative.
- `http2jp/hpack-test-case` (MIT), 478 pinned JSON interoperability stories from multiple HPACK encoders, including Huffman and dynamic-table strategies.

Integration direction:

1. Import HPACK JSON through a streaming test loader and preserve story order because cases share compression context.
2. Build an in-process h2spec adapter once a server endpoint exists; until then, port frame/state-machine cases into deterministic table tests.
3. Add RFC 9113-specific deltas, continuation sequencing, pseudo-header validation, stream-state transitions, flow-control overflow, SETTINGS synchronization, GOAWAY boundaries, and adversarial HPACK integer/Huffman inputs.
4. Run every frame parser under all input split points and enforce finite frame, header-list, dynamic-table, stream, and queued-byte limits.

## WebSocket

Primary specification: RFC 6455; RFC 7692 is relevant only if per-message deflate is implemented.

Candidate: `crossbario/autobahn-testsuite` (Apache-2.0). The pinned suite contains more than 500 client/server conformance, robustness, limits, performance, fragmentation, UTF-8, ping/pong, close-handshake, opcode, reserved-bit, and compression cases. Its recommended execution environment is containerized and intentionally preserves legacy dependencies.

Integration direction:

1. Add small Wago guest testees for client and server modes and run Autobahn out of process.
2. Keep compression groups disabled until RFC 7692 is implemented and separately bounded.
3. Mirror critical framing, masking, close-code, UTF-8, control-frame, fragmentation, and handshake failures as fast in-process regression tests.
4. Add allocation and retained-buffer assertions for fragmented messages, early close, peer failure, and instance teardown.

## HTTP/3, QPACK, and QUIC

Primary specifications: RFC 9114 for HTTP/3, RFC 9204 for QPACK, and RFCs 9000/9001 for QUIC transport and TLS integration.

Candidates:

- `ngtcp2/nghttp3` (MIT), with HTTP/3/QPACK unit tests and fuzz seeds. It is useful for parser/state-machine differential testing even if the Wago implementation does not reuse its C code.
- `quic-interop/quic-interop-runner` (Apache-2.0), which runs endpoint interoperability scenarios across independent QUIC implementations and records logs and packet captures.

Integration direction:

1. Port nghttp3 QPACK vectors and fuzz seeds into Go-native corpus tests; add RFC 9204 appendix vectors and repository-owned blocked-stream/table-eviction cases.
2. Create a runner endpoint only after the UDP-backed QUIC state machine supports deterministic certificates, timers, loss recovery, congestion control, stream limits, and connection teardown.
3. Separate QUIC transport conformance from HTTP/3 mapping tests so failures retain a narrow owner.
4. Add malformed varints, reserved frames/settings, critical-stream closure, stream-type duplication, QPACK blocking limits, 0-RTT policy, key updates, retry/version negotiation, loss/reordering, amplification limits, and timer-injection tests.

## Import policy

- Keep upstream checkouts ignored and pinned by full commit ID.
- Preserve each upstream license and attribution in the fetched tree.
- Do not silently patch imported expectations; adapters must record local overrides with an RFC section and rationale.
- Convert only the minimum stable fixture data needed for fast unit tests. Keep heavyweight network runners as explicit integration jobs.
- Treat corpus success as necessary but insufficient: bounds, cleanup, capability isolation, exact Wago instance identity, and allocation behavior remain repository-owned invariants.
