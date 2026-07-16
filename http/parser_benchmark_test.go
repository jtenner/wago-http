package http

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

type parserBenchmarkCase struct {
	name      string
	kind      Kind
	input     []byte
	limits    Limits
	callbacks *Callbacks
	finish    bool
	wantCode  Code
	messages  uint64
}

var benchmarkHEADResponseCallbacks = Callbacks{
	ResponseContext: func(uint64) (bool, bool) { return true, false },
}

var benchmarkCONNECTResponseCallbacks = Callbacks{
	ResponseContext: func(uint64) (bool, bool) { return false, true },
}

func BenchmarkParserEdgeMatrix(b *testing.B) {
	manyHeaders := buildBenchmarkHeaders(64)
	fixed1KiB := buildFixedRequest(1024)
	fixed64KiBResponse := buildFixedResponse(64 << 10)
	manyChunks := buildChunkedRequest(128, 8, false)
	chunkedTrailers := buildChunkedRequest(4, 32, true)
	requestPipeline := bytes.Repeat([]byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"), 16)
	responsePipeline := bytes.Repeat([]byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata"), 16)

	cases := []parserBenchmarkCase{
		{name: "request/minimal-http10", kind: Request, input: []byte("GET / HTTP/1.0\r\n\r\n"), messages: 1},
		{name: "request/typical-get", kind: Request, input: []byte("GET /api/items?q=parser HTTP/1.1\r\nHost: example.test\r\nUser-Agent: benchmark\r\nAccept: application/json\r\nConnection: keep-alive\r\n\r\n"), messages: 1},
		{name: "request/many-headers-64", kind: Request, input: manyHeaders, messages: 1},
		{name: "request/fixed-body-1KiB", kind: Request, input: fixed1KiB, messages: 1},
		{name: "request/chunked-extensions-trailers", kind: Request, input: chunkedTrailers, messages: 1},
		{name: "request/chunked-128-small-chunks", kind: Request, input: manyChunks, messages: 1},
		{name: "request/absolute-form", kind: Request, input: []byte("GET https://example.test/a%20b?q=x HTTP/1.1\r\nHost: example.test\r\n\r\n"), messages: 1},
		{name: "request/connect-ipv6-zone", kind: Request, input: []byte("CONNECT [fe80::1%25eth0]:443 HTTP/1.1\r\nHost: [fe80::1%25eth0]:443\r\n\r\n"), messages: 1},
		{name: "request/upgrade-negotiation", kind: Request, input: []byte("GET /chat HTTP/1.1\r\nHost: example.test\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket/13, h2c\r\n\r\n"), messages: 1},
		{name: "request/pipeline-16", kind: Request, input: requestPipeline, messages: 16},
		{name: "request/malformed-framing-late", kind: Request, input: []byte("POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\ndata"), wantCode: CodeContentLengthConflict},
		{name: "request/malformed-chunk-late", kind: Request, input: []byte("POST / HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n\r\n4;name=\"value\"\r\ndata\rX"), wantCode: CodeInvalidLineEnding},

		{name: "response/fixed-body", kind: Response, input: []byte("HTTP/1.1 200 OK\r\nContent-Length: 16\r\nContent-Type: text/plain\r\n\r\n0123456789abcdef"), messages: 1},
		{name: "response/fixed-body-64KiB", kind: Response, input: fixed64KiBResponse, limits: Limits{MaxBodyBytes: 128 << 10}, messages: 1},
		{name: "response/chunked-trailers", kind: Response, input: bytes.Replace(chunkedTrailers, []byte("POST / HTTP/1.1\r\nHost: example.test\r\n"), []byte("HTTP/1.1 200 OK\r\n"), 1), messages: 1},
		{name: "response/close-delimited", kind: Response, input: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nbody until eof"), finish: true, messages: 1},
		{name: "response/informational-then-head", kind: Response, input: []byte("HTTP/1.1 103 Early Hints\r\nLink: </style.css>; rel=preload\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\n"), callbacks: &benchmarkHEADResponseCallbacks, messages: 2},
		{name: "response/upgrade-101", kind: Response, input: []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nopaque-protocol-bytes"), wantCode: CodeUpgrade, messages: 1},
		{name: "response/connect-success", kind: Response, input: []byte("HTTP/1.1 200 Connection Established\r\n\r\ntunnel-bytes"), callbacks: &benchmarkCONNECTResponseCallbacks, wantCode: CodeUpgrade, messages: 1},
		{name: "response/no-content-204", kind: Response, input: []byte("HTTP/1.1 204 No Content\r\nDate: Thu, 01 Jan 1970 00:00:00 GMT\r\n\r\n"), messages: 1},
		{name: "response/pipeline-16", kind: Response, input: responsePipeline, messages: 16},
		{name: "response/non-final-transfer-coding", kind: Response, input: []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip; q=\"1\"\r\n\r\ncompressed-stream"), finish: true, messages: 1},
	}

	fragmentSizes := []struct {
		name string
		size int
	}{
		{name: "contiguous", size: 0},
		{name: "fragment-64", size: 64},
		{name: "fragment-16", size: 16},
		{name: "byte-at-a-time", size: 1},
	}

	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			for _, fragmentation := range fragmentSizes {
				// Byte-at-a-time throughput for the 64 KiB body is dominated by
				// call overhead and makes the complete matrix unnecessarily slow.
				if fragmentation.size == 1 && len(test.input) > 8<<10 {
					continue
				}
				b.Run(fragmentation.name, func(b *testing.B) {
					benchmarkParserCase(b, test, fragmentation.size)
				})
			}
		})
	}
}

func BenchmarkParserCallbacks(b *testing.B) {
	inputs := []struct {
		name  string
		kind  Kind
		input []byte
	}{
		{name: "request-fixed", kind: Request, input: benchmarkRequest},
		{name: "request-chunked", kind: Request, input: benchmarkChunkedRequest},
		{name: "response-fixed", kind: Response, input: []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata")},
	}
	for _, input := range inputs {
		b.Run(input.name, func(b *testing.B) {
			b.Run("none", func(b *testing.B) {
				benchmarkParserCase(b, parserBenchmarkCase{kind: input.kind, input: input.input, messages: 1}, 0)
			})
			b.Run("noop-all", func(b *testing.B) {
				benchmarkParserCase(b, parserBenchmarkCase{kind: input.kind, input: input.input, callbacks: &noopCallbacks, messages: 1}, 0)
			})
		})
	}
}

func BenchmarkParserLimitsAndFailures(b *testing.B) {
	cases := []parserBenchmarkCase{
		{name: "start-line-limit", kind: Request, input: []byte("GET /path HTTP/1.1\r\nHost: e\r\n\r\n"), limits: Limits{MaxStartLineBytes: 8}, wantCode: CodeStartLineTooLarge},
		{name: "header-byte-limit", kind: Request, input: []byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"), limits: Limits{MaxHeaderBytes: 8}, wantCode: CodeHeadersTooLarge},
		{name: "body-limit-fixed", kind: Request, input: []byte("POST / HTTP/1.1\r\nHost: e\r\nContent-Length: 8\r\n\r\n12345678"), limits: Limits{MaxBodyBytes: 4}, wantCode: CodeBodyTooLarge},
		{name: "chunk-count-limit", kind: Request, input: buildChunkedRequest(8, 1, false), limits: Limits{MaxChunks: 4}, wantCode: CodeTooManyChunks},
		{name: "smuggling-cl-te", kind: Request, input: []byte("POST / HTTP/1.1\r\nHost: e\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"), wantCode: CodeContentLengthConflict},
		{name: "truncated-fixed", kind: Request, input: []byte("POST / HTTP/1.1\r\nHost: e\r\nContent-Length: 8\r\n\r\n1234"), finish: true, wantCode: CodeUnexpectedEOF},
	}
	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) { benchmarkParserCase(b, test, 0) })
	}
}

func benchmarkParserCase(b *testing.B, test parserBenchmarkCase, fragmentSize int) {
	b.Helper()
	b.ReportAllocs()
	parsedBytes := verifyParserBenchmarkCase(b, test, fragmentSize)
	b.SetBytes(int64(parsedBytes))
	b.ResetTimer()
	for b.Loop() {
		parser := NewParser(test.kind, test.callbacks, test.limits)
		offset := 0
		code := CodeNone
		for offset < len(test.input) && code == CodeNone {
			end := len(test.input)
			if fragmentSize > 0 && end-offset > fragmentSize {
				end = offset + fragmentSize
			}
			consumed, nextCode := parser.Parse(test.input[offset:end])
			if consumed < 0 || consumed > end-offset {
				b.Fatalf("invalid consumption %d of %d", consumed, end-offset)
			}
			offset += consumed
			code = nextCode
		}
		if test.finish && code == CodeNone {
			code = parser.Finish()
		}
		if code != test.wantCode || parser.MessageNumber() != test.messages {
			b.Fatalf("result = %v/%d at %d/%d, want %v/%d", code, parser.MessageNumber(), offset, len(test.input), test.wantCode, test.messages)
		}
	}
	b.ReportMetric(float64(parsedBytes), "parsed-B/op")
}

func verifyParserBenchmarkCase(b *testing.B, test parserBenchmarkCase, fragmentSize int) int {
	b.Helper()
	parser := NewParser(test.kind, test.callbacks, test.limits)
	offset := 0
	code := CodeNone
	for offset < len(test.input) && code == CodeNone {
		end := len(test.input)
		if fragmentSize > 0 && end-offset > fragmentSize {
			end = offset + fragmentSize
		}
		consumed, nextCode := parser.Parse(test.input[offset:end])
		if consumed < 0 || consumed > end-offset {
			b.Fatalf("invalid preflight consumption %d of %d", consumed, end-offset)
		}
		offset += consumed
		code = nextCode
	}
	if test.finish && code == CodeNone {
		code = parser.Finish()
	}
	if code != test.wantCode || parser.MessageNumber() != test.messages {
		b.Fatalf("preflight result = %v/%d at %d/%d, want %v/%d", code, parser.MessageNumber(), offset, len(test.input), test.wantCode, test.messages)
	}
	return offset
}

func buildBenchmarkHeaders(count int) []byte {
	var builder strings.Builder
	builder.Grow(32 + count*32)
	builder.WriteString("GET /headers HTTP/1.1\r\nHost: example.test\r\n")
	for index := 1; index < count; index++ {
		fmt.Fprintf(&builder, "X-Header-%02d: value-%02d\r\n", index, index)
	}
	builder.WriteString("\r\n")
	return []byte(builder.String())
}

func buildFixedRequest(size int) []byte {
	prefix := fmt.Sprintf("POST /upload HTTP/1.1\r\nHost: example.test\r\nContent-Length: %d\r\n\r\n", size)
	input := make([]byte, len(prefix)+size)
	copy(input, prefix)
	for index := len(prefix); index < len(input); index++ {
		input[index] = byte(index)
	}
	return input
}

func buildFixedResponse(size int) []byte {
	prefix := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\n\r\n", size)
	input := make([]byte, len(prefix)+size)
	copy(input, prefix)
	for index := len(prefix); index < len(input); index++ {
		input[index] = byte(index)
	}
	return input
}

func buildChunkedRequest(chunks, chunkSize int, trailers bool) []byte {
	var builder strings.Builder
	builder.Grow(96 + chunks*(chunkSize+32))
	builder.WriteString("POST / HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n")
	if trailers {
		builder.WriteString("Trailer: X-Checksum\r\n")
	}
	builder.WriteString("\r\n")
	body := strings.Repeat("x", chunkSize)
	for index := 0; index < chunks; index++ {
		if trailers {
			fmt.Fprintf(&builder, "%x; index=%d; mode=\"fast\"\r\n", chunkSize, index)
		} else {
			fmt.Fprintf(&builder, "%x\r\n", chunkSize)
		}
		builder.WriteString(body)
		builder.WriteString("\r\n")
	}
	builder.WriteString("0\r\n")
	if trailers {
		builder.WriteString("X-Checksum: complete\r\n")
	}
	builder.WriteString("\r\n")
	return []byte(builder.String())
}
