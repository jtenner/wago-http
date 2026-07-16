# HTTP/2 Performance Baseline

This document records the first comprehensive performance baseline for the native HTTP/2 frame parser, HPACK decoder, and `http2/request` stream-1 client. The machine-readable Go benchmark output is committed at [`benchmarks/http2-baseline-2026-07-16.txt`](benchmarks/http2-baseline-2026-07-16.txt).

## Baseline identity

| Property | Value |
|---|---|
| Date | July 16, 2026 |
| Base Git HEAD | `fc0f31c` plus the uncommitted HTTP/2 implementation represented by source fingerprint below |
| HTTP/2 source fingerprint | `7858f45de6a076170b423313e0dfc0e452ed5461a1eff932932ec58bcba6f27d` |
| Raw result SHA-256 | `74699dc9da22eb5d7299fd4382f65eef1fcdfaecb48d66fc01224da1bb33e33f` |
| OS | Debian GNU/Linux, Linux `6.12.95+deb13-amd64` |
| Architecture | `linux/amd64`, `GOAMD64=v1`, CGO enabled |
| CPU | AMD Ryzen 7 8845HS, 8 cores / 16 threads, up to 5.1 GHz |
| Cache | 256 KiB L1d, 256 KiB L1i, 8 MiB L2, 16 MiB L3 |
| Go | `go1.24.4 linux/amd64` |
| Scheduling | CPU affinity fixed to logical CPU 4; `GOMAXPROCS=1` |
| Samples | 10 per benchmark |
| Target duration | 250 ms per sample |
| Race/checkptr | Disabled for performance run |

Command:

```sh
taskset -c 4 env GOMAXPROCS=1 \
  go test ./http2 ./http2/request \
  -run '^$' -bench '^Benchmark' -benchmem -benchtime=250ms -count=10 \
  | tee benchmarks/http2-baseline-2026-07-16.txt
```

The tables report the median of ten samples. The range column preserves the observed minimum and maximum so future comparisons can distinguish likely signal from machine noise. `MB/s` is emitted by Go from each benchmark's declared wire-byte count.

## Frame parser, frame writer, and HPACK

| Benchmark | Median | Observed range | Median MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| `AppendFrame/payload-0` | 6.40 ns | 6.30 ns–6.49 ns | 1,405.4 | 0 | 0 |
| `AppendFrame/payload-1024` | 14.61 ns | 13.79 ns–23.06 ns | 70,847 | 0 | 0 |
| `AppendFrame/payload-16384` | 148.35 ns | 144.30 ns–235.70 ns | 110,491 | 0 | 0 |
| `AppendFrame/payload-64` | 7.24 ns | 7.15 ns–7.38 ns | 10,083 | 0 | 0 |
| `HeaderDecoder/chunk-1` | 275.44 µs | 271.55 µs–294.78 µs | 31.55 | 12,811 | 200 |
| `HeaderDecoder/chunk-1073741824` | 81.89 µs | 79.75 µs–85.38 µs | 106.1 | 12,803 | 200 |
| `HeaderDecoder/chunk-64` | 84.07 µs | 72.52 µs–87.01 µs | 103.4 | 12,803 | 200 |
| `HeaderDecoder/chunk-8` | 104.31 µs | 101.43 µs–106.35 µs | 83.31 | 12,804 | 200 |
| `Parser/data-16k/chunk-1` | 150.13 µs | 148.67 µs–150.75 µs | 109.2 | 0 | 0 |
| `Parser/data-16k/chunk-1024` | 168.90 ns | 167.60 ns–171.80 ns | 97,065 | 0 | 0 |
| `Parser/data-16k/chunk-1073741824` | 25.97 ns | 25.84 ns–26.21 ns | 631,190 | 0 | 0 |
| `Parser/data-16k/chunk-16` | 9.27 µs | 9.14 µs–9.91 µs | 1,768.8 | 0 | 0 |
| `Parser/data-16k/chunk-64` | 2.32 µs | 2.31 µs–2.45 µs | 7,055.5 | 0 | 0 |
| `Parser/headers-continuations-12k/chunk-1` | 119.00 µs | 113.43 µs–126.90 µs | 103.5 | 0 | 0 |
| `Parser/headers-continuations-12k/chunk-1024` | 181.90 ns | 178.70 ns–215.50 ns | 67,709 | 0 | 0 |
| `Parser/headers-continuations-12k/chunk-1073741824` | 67.50 ns | 64.65 ns–76.60 ns | 182,476 | 0 | 0 |
| `Parser/headers-continuations-12k/chunk-16` | 7.46 µs | 7.21 µs–8.35 µs | 1,649.8 | 0 | 0 |
| `Parser/headers-continuations-12k/chunk-64` | 1.90 µs | 1.85 µs–3.27 µs | 6,493.4 | 0 | 0 |
| `Parser/padded-data-16k/chunk-1` | 150.32 µs | 147.35 µs–151.94 µs | 109.0 | 0 | 0 |
| `Parser/padded-data-16k/chunk-1024` | 284.80 ns | 174.30 ns–300.80 ns | 57,669 | 0 | 0 |
| `Parser/padded-data-16k/chunk-1073741824` | 27.32 ns | 26.49 ns–27.71 ns | 600,104 | 0 | 0 |
| `Parser/padded-data-16k/chunk-16` | 16.19 µs | 9.19 µs–16.29 µs | 1,012.6 | 0 | 0 |
| `Parser/padded-data-16k/chunk-64` | 4.14 µs | 4.09 µs–4.17 µs | 3,964.2 | 0 | 0 |
| `Parser/ping-pipeline-100/chunk-1` | 13.23 µs | 13.18 µs–13.43 µs | 128.5 | 0 | 0 |
| `Parser/ping-pipeline-100/chunk-1024` | 1.70 µs | 1.69 µs–1.72 µs | 998.5 | 0 | 0 |
| `Parser/ping-pipeline-100/chunk-1073741824` | 1.68 µs | 1.66 µs–1.78 µs | 1,014.3 | 0 | 0 |
| `Parser/ping-pipeline-100/chunk-16` | 2.44 µs | 2.42 µs–2.45 µs | 696.9 | 0 | 0 |
| `Parser/ping-pipeline-100/chunk-64` | 1.88 µs | 1.87 µs–1.90 µs | 904.2 | 0 | 0 |
| `Parser/settings-100/chunk-1` | 6.87 µs | 6.78 µs–6.98 µs | 88.66 | 0 | 0 |
| `Parser/settings-100/chunk-1024` | 402.40 ns | 399.30 ns–414.90 ns | 1,513.5 | 0 | 0 |
| `Parser/settings-100/chunk-1073741824` | 414.10 ns | 405.10 ns–426.30 ns | 1,470.8 | 0 | 0 |
| `Parser/settings-100/chunk-16` | 819.00 ns | 812.80 ns–842.30 ns | 743.6 | 0 | 0 |
| `Parser/settings-100/chunk-64` | 503.85 ns | 495.20 ns–514.80 ns | 1,208.7 | 0 | 0 |
| `Parser/unknown-16k/chunk-1` | 114.64 µs | 113.03 µs–116.07 µs | 143.0 | 0 | 0 |
| `Parser/unknown-16k/chunk-1024` | 137.60 ns | 133.70 ns–138.90 ns | 119,121 | 0 | 0 |
| `Parser/unknown-16k/chunk-1073741824` | 22.77 ns | 22.57 ns–23.83 ns | 719,879 | 0 | 0 |
| `Parser/unknown-16k/chunk-16` | 7.21 µs | 7.08 µs–7.37 µs | 2,274.4 | 0 | 0 |
| `Parser/unknown-16k/chunk-64` | 1.81 µs | 1.80 µs–1.82 µs | 9,064.1 | 0 | 0 |
| `ParserCallbacks/chunk-1` | 165.52 µs | 162.17 µs–218.76 µs | 99.04 | 0 | 0 |
| `ParserCallbacks/chunk-1073741824` | 27.15 ns | 27.00 ns–27.36 ns | 603,824 | 0 | 0 |
| `ParserCallbacks/chunk-64` | 2.59 µs | 2.55 µs–3.63 µs | 6,324.0 | 0 | 0 |
| `ParserErrors/header-block-limit` | 22.40 ns | 22.33 ns–27.41 ns | 46,115 | 0 | 0 |
| `ParserErrors/header-stream-zero` | 19.02 ns | 15.48 ns–24.98 ns | 475.5 | 0 | 0 |
| `ParserErrors/invalid-setting-late` | 27.99 ns | 27.69 ns–28.45 ns | 750.3 | 0 | 0 |
| `ParserErrors/late-continuation` | 35.21 ns | 34.85 ns–41.11 ns | 766.8 | 0 | 0 |
| `ParserErrors/payload-padding` | 20.30 ns | 19.19 ns–21.46 ns | 492.8 | 0 | 0 |
| `ParserParseOnePipeline` | 2.21 µs | 2.10 µs–2.66 µs | 770.5 | 0 | 0 |

## Request encoding and streaming client

| Benchmark | Median | Observed range | Median MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| `AppendRequest/body-16k` | 2.40 µs | 2.27 µs–3.75 µs | 6,908.1 | 1,872 | 28 |
| `AppendRequest/body-64k` | 3.31 µs | 3.09 µs–3.53 µs | 19,864 | 1,872 | 28 |
| `AppendRequest/get` | 625.50 ns | 588.30 ns–690.50 ns | 118.3 | 992 | 13 |
| `AppendRequest/header-16k` | 73.57 µs | 65.14 µs–74.81 µs | 196.0 | 96,712 | 31 |
| `AppendRequest/headers-32` | 13.70 µs | 13.45 µs–14.69 µs | 57.44 | 14,768 | 102 |
| `ClientResponseSequences/informational-trailers` | 2.29 µs | 2.25 µs–2.33 µs | 29.20 | 2,856 | 46 |
| `ClientResponseSequences/malformed-early` | 2.28 µs | 1.32 µs–2.69 µs | 6.14 | 2,112 | 37 |
| `ClientResponseSequences/malformed-late` | 1.80 µs | 1.78 µs–2.92 µs | 17.77 | 2,760 | 42 |
| `ClientStreamingResponse/body-0/read-1` | 2.33 µs | 2.25 µs–4.45 µs | 9.45 | 2,792 | 41 |
| `ClientStreamingResponse/body-0/read-16384` | 1.95 µs | 1.89 µs–2.02 µs | 11.29 | 2,728 | 40 |
| `ClientStreamingResponse/body-1024/read-1` | 21.25 µs | 20.48 µs–36.98 µs | 49.77 | 2,832 | 44 |
| `ClientStreamingResponse/body-1024/read-16384` | 2.46 µs | 2.37 µs–3.07 µs | 429.2 | 2,768 | 43 |
| `ClientStreamingResponse/body-1024/read-64` | 3.00 µs | 2.65 µs–4.12 µs | 352.9 | 2,768 | 43 |
| `ClientStreamingResponse/body-1024/read-64/callback` | 2.66 µs | 2.62 µs–2.77 µs | 397.6 | 2,768 | 43 |
| `ClientStreamingResponse/body-1048576/read-1024` | 42.15 µs | 39.33 µs–67.36 µs | 24,947 | 4,784 | 169 |
| `ClientStreamingResponse/body-1048576/read-16384` | 28.39 µs | 27.93 µs–29.47 µs | 36,961 | 4,784 | 169 |
| `ClientStreamingResponse/body-1048576/read-16384/callback` | 28.63 µs | 27.11 µs–34.57 µs | 36,642 | 4,784 | 169 |
| `ClientStreamingResponse/body-65535/read-1024` | 7.63 µs | 4.37 µs–7.75 µs | 8,592.8 | 2,864 | 49 |
| `ClientStreamingResponse/body-65535/read-1024/callback` | 4.33 µs | 4.17 µs–4.45 µs | 15,137 | 2,864 | 49 |
| `ClientStreamingResponse/body-65535/read-16384` | 3.76 µs | 3.63 µs–3.91 µs | 17,462 | 2,864 | 49 |
| `ClientStreamingResponse/body-65535/read-64` | 21.55 µs | 21.15 µs–22.39 µs | 3,043.2 | 2,864 | 49 |
| `EncodeRequest/body-64k` | 15.21 µs | 14.69 µs–17.02 µs | 4,322.4 | 192,696 | 36 |
| `EncodeRequest/get` | 716.95 ns | 684.80 ns–743.60 ns | 103.2 | 1,160 | 16 |
| `EncodeRequest/headers-32` | 13.89 µs | 13.76 µs–16.08 µs | 56.64 | 15,832 | 106 |
| `ValidateRequest/body-64k` | 87.67 ns | 85.35 ns–102.20 ns | — | 0 | 0 |
| `ValidateRequest/get` | 17.10 ns | 16.93 ns–18.53 ns | — | 0 | 0 |
| `ValidateRequest/headers-32` | 560.35 ns | 534.50 ns–796.40 ns | — | 0 | 0 |
| `ValidateRequest/invalid-header-late` | 539.00 ns | 532.20 ns–587.70 ns | — | 16 | 1 |
| `ValidateRequest/invalid-method-early` | 20.80 ns | 20.38 ns–21.40 ns | — | 16 | 1 |
| `WriteRequest/body-64k` | 15.09 µs | 13.10 µs–16.19 µs | 4,356.6 | 192,696 | 36 |
| `WriteRequest/get` | 709.15 ns | 678.50 ns–738.50 ns | 104.4 | 1,160 | 16 |
| `WriteRequest/headers-32` | 13.37 µs | 13.19 µs–13.80 µs | 58.86 | 15,832 | 106 |
## Interpretation notes

- The frame parser's no-callback DATA and extension-frame paths validate and advance borrowed payload spans without scanning every payload byte. Their very high reported MB/s is therefore a control-path throughput measurement, not memory-copy bandwidth.
- Frame parsing, malformed-frame rejection, `ParseOne`, and preallocated `AppendFrame` remain at **0 B/op and 0 allocs/op** in the sustained baseline.
- HPACK numbers include decoded string and field emission costs. The benchmark preserves compression context across repeated blocks, matching a long-lived HTTP/2 connection.
- Request `Append`, `Encode`, and `Write` include HPACK encoder construction. `Encode` intentionally creates the complete validated wire image before copying into the caller's destination.
- Streaming-client benchmarks are in-memory end-to-end exchanges. They include request encoding, frame parsing, HPACK response decoding, SETTINGS acknowledgement, and DATA flow-control WINDOW_UPDATE writes, but exclude sockets, TLS, ALPN, kernel scheduling, and real network latency.
- Read sizes of 1, 64, 1,024, and 16,384 bytes model extreme fragmentation through normal buffered delivery. Callback variants include body callback dispatch but do not copy body data.
- Some ranges show frequency or thermal transitions during this approximately five-minute run. Use the committed ten-sample raw file with `benchstat` rather than comparing one isolated sample.

## Future comparison procedure

1. Keep the same machine, CPU affinity, Go version, `GOMAXPROCS`, and benchmark command when possible.
2. Save the new raw output under `benchmarks/` with an ISO date.
3. Compare raw files with:

   ```sh
   benchstat benchmarks/http2-baseline-2026-07-16.txt benchmarks/http2-candidate-YYYY-MM-DD.txt
   ```

4. Treat parser allocation regressions as failures. For request and HPACK paths, review both latency and allocation deltas.
5. Re-run noisy or thermally bimodal cases independently with a longer `-benchtime` before accepting a regression or improvement.
