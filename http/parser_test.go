package http

import (
	"bytes"
	"strings"
	"testing"
)

type recordedMessage struct {
	method    []byte
	target    []byte
	reason    []byte
	headers   [][2][]byte
	body      []byte
	status    uint16
	major     uint16
	minor     uint16
	keepAlive bool
}

type recorder struct {
	messages []recordedMessage
	current  recordedMessage
	name     []byte
	value    []byte
	trailers int
	chunks   []uint64
}

func (r *recorder) callbacks() *Callbacks {
	return &Callbacks{
		Method: func(value []byte) {
			r.current.method = append(r.current.method, value...)
		},
		Target: func(value []byte) {
			r.current.target = append(r.current.target, value...)
		},
		Reason: func(value []byte) {
			r.current.reason = append(r.current.reason, value...)
		},
		StartLine: func(message Message) {
			r.current.status = message.Status
			r.current.major, r.current.minor = message.Major, message.Minor
		},
		HeaderName: func(value []byte) {
			r.name = append(r.name, value...)
		},
		HeaderValue: func(value []byte) {
			r.value = append(r.value, value...)
		},
		HeaderEnd: func(trailer bool) {
			pair := [2][]byte{append([]byte(nil), r.name...), append([]byte(nil), r.value...)}
			r.current.headers = append(r.current.headers, pair)
			r.name = r.name[:0]
			r.value = r.value[:0]
			if trailer {
				r.trailers++
			}
		},
		ChunkHeader: func(size uint64) {
			r.chunks = append(r.chunks, size)
		},
		Body: func(value []byte) {
			r.current.body = append(r.current.body, value...)
		},
		MessageComplete: func(message Message) {
			r.current.keepAlive = message.KeepAlive
			r.messages = append(r.messages, r.current)
			r.current = recordedMessage{}
		},
	}
}

func TestParserRequestPipelining(t *testing.T) {
	input := []byte("POST /upload HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\nConnection: keep-alive\r\n\r\nhelloGET /next HTTP/1.1\r\nHost: example.test\r\n\r\n")
	var record recorder
	parser := NewParser(Request, record.callbacks(), Limits{})
	consumed, code := parser.Parse(input)
	if consumed != len(input) || code != CodeNone {
		t.Fatalf("Parse = (%d, %v), want (%d, ok)", consumed, code, len(input))
	}
	if parser.MessageNumber() != 2 || len(record.messages) != 2 {
		t.Fatalf("messages = %d/%d, want 2", parser.MessageNumber(), len(record.messages))
	}
	first := record.messages[0]
	if string(first.method) != "POST" || string(first.target) != "/upload" || string(first.body) != "hello" {
		t.Fatalf("first message = method %q target %q body %q", first.method, first.target, first.body)
	}
	if first.major != 1 || first.minor != 1 {
		t.Fatalf("first version = %d.%d", first.major, first.minor)
	}
	second := record.messages[1]
	if string(second.method) != "GET" || string(second.target) != "/next" || len(second.body) != 0 {
		t.Fatalf("second message = method %q target %q body %q", second.method, second.target, second.body)
	}
}

func TestParserEverySplitPoint(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		input string
		body  string
	}{
		{
			name:  "request-content-length",
			kind:  Request,
			input: "POST /alpha HTTP/1.1\r\nHost: example.test\r\nContent-Length: 11\r\nX-Test: some value  \r\n\r\nhello world",
			body:  "hello world",
		},
		{
			name:  "request-chunked",
			kind:  Request,
			input: "POST /alpha HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: gzip; level=\"1\", chunked\r\n\r\n5;foo=bar\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: yes\r\n\r\n",
			body:  "hello world",
		},
		{
			name:  "request-ipv6-host",
			kind:  Request,
			input: "GET / HTTP/1.1\r\nHost: [fe80::1%25eth0]:8080\r\n\r\n",
		},
		{
			name:  "request-connect-ipv6",
			kind:  Request,
			input: "CONNECT [2001:db8::1]:443 HTTP/1.1\r\nHost: [2001:db8::1]\r\n\r\n",
		},
		{
			name:  "request-absolute-form",
			kind:  Request,
			input: "GET http://example.test/path?q=yes HTTP/1.1\r\nHost: example.test\r\n\r\n",
		},
		{
			name:  "request-asterisk-form",
			kind:  Request,
			input: "OPTIONS * HTTP/1.1\r\nHost: example.test\r\n\r\n",
		},
		{
			name:  "response-fixed",
			kind:  Response,
			input: "HTTP/1.0 200 Everything is fine\r\nContent-Length: 2\r\n\r\nok",
			body:  "ok",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(test.input)
			for split := 0; split <= len(input); split++ {
				var record recorder
				parser := NewParser(test.kind, record.callbacks(), Limits{})
				first, code := parser.Parse(input[:split])
				if first != split || code != CodeNone {
					t.Fatalf("split %d first Parse = (%d, %v)", split, first, code)
				}
				second, code := parser.Parse(input[split:])
				if second != len(input)-split || code != CodeNone {
					t.Fatalf("split %d second Parse = (%d, %v), want (%d, ok)", split, second, code, len(input)-split)
				}
				if len(record.messages) != 1 || string(record.messages[0].body) != test.body {
					t.Fatalf("split %d messages/body = %d/%q", split, len(record.messages), messageBody(record.messages))
				}
			}
		})
	}
}

func TestParserByteAtATime(t *testing.T) {
	input := []byte("POST /chunk HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n\r\na;name=\"quoted; value\"\r\n0123456789\r\n0\r\n\r\n")
	var record recorder
	parser := NewParser(Request, record.callbacks(), Limits{})
	for offset := range input {
		consumed, code := parser.Parse(input[offset : offset+1])
		if consumed != 1 || code != CodeNone {
			t.Fatalf("byte %d Parse = (%d, %v)", offset, consumed, code)
		}
	}
	if len(record.messages) != 1 || string(record.messages[0].body) != "0123456789" {
		t.Fatalf("messages/body = %d/%q", len(record.messages), messageBody(record.messages))
	}
	if got, want := record.chunks, []uint64{10, 0}; !equalUint64s(got, want) {
		t.Fatalf("chunks = %v, want %v", got, want)
	}
}

func TestParserPipelinedResponseContexts(t *testing.T) {
	input := []byte("HTTP/1.1 103 Early Hints\r\nLink: </style.css>\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	var bodies [3][]byte
	var exchanges [3]uint64
	current := 0
	callbacks := Callbacks{
		ResponseContext: func(exchangeNumber uint64) (bool, bool) {
			return exchangeNumber == 0, false
		},
		Body: func(value []byte) {
			bodies[current] = append(bodies[current], value...)
		},
		MessageComplete: func(message Message) {
			exchanges[current] = message.ExchangeNumber
			current++
		},
	}
	parser := NewParser(Response, &callbacks, Limits{})
	consumed, code := parser.Parse(input)
	if consumed != len(input) || code != CodeNone || current != 3 {
		t.Fatalf("Parse = (%d, %v), complete=%d", consumed, code, current)
	}
	if len(bodies[0]) != 0 || len(bodies[1]) != 0 || string(bodies[2]) != "ok" {
		t.Fatalf("bodies = %q, %q, %q", bodies[0], bodies[1], bodies[2])
	}
	if exchanges != [3]uint64{0, 0, 1} || parser.ExchangeNumber() != 2 {
		t.Fatalf("exchange numbers = %v, next=%d", exchanges, parser.ExchangeNumber())
	}
}

func TestParserResponseCloseDelimited(t *testing.T) {
	var record recorder
	callbacks := record.callbacks()
	headerKeepAlive := true
	callbacks.HeadersComplete = func(message Message) { headerKeepAlive = message.KeepAlive }
	parser := NewParser(Response, callbacks, Limits{})
	input := []byte("HTTP/1.1 200 OK\r\nServer: test\r\n\r\nbody until eof")
	consumed, code := parser.Parse(input)
	if consumed != len(input) || code != CodeNone {
		t.Fatalf("Parse = (%d, %v)", consumed, code)
	}
	if len(record.messages) != 0 {
		t.Fatal("close-delimited response completed before EOF")
	}
	if code := parser.Finish(); code != CodeNone {
		t.Fatalf("Finish = %v", code)
	}
	if len(record.messages) != 1 || string(record.messages[0].body) != "body until eof" {
		t.Fatalf("messages/body = %d/%q", len(record.messages), messageBody(record.messages))
	}
	if headerKeepAlive || record.messages[0].keepAlive || parser.KeepAlive() {
		t.Fatal("close-delimited response reported keep-alive")
	}
}

func TestParserResponseBodySemantics(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		head    bool
		connect bool
		upgrade bool
	}{
		{name: "informational", input: "HTTP/1.1 103 Early Hints\r\nLink: </x>\r\n\r\n"},
		{name: "no-content", input: "HTTP/1.1 204 No Content\r\n\r\n"},
		{name: "not-modified", input: "HTTP/1.1 304 Not Modified\r\nContent-Length: 99\r\n\r\n"},
		{name: "head", input: "HTTP/1.1 200 OK\r\nContent-Length: 99\r\n\r\n", head: true},
		{name: "connect", input: "HTTP/1.1 200 Connection Established\r\n\r\ntunnel", connect: true, upgrade: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var completed int
			parser := NewParser(Response, &Callbacks{MessageComplete: func(Message) { completed++ }}, Limits{})
			parser.SetResponseContext(test.head, test.connect)
			consumed, code := parser.Parse([]byte(test.input))
			if test.upgrade {
				want := strings.Index(test.input, "tunnel")
				if consumed != want || code != CodeUpgrade || completed != 1 {
					t.Fatalf("Parse = (%d, %v), completed=%d; want (%d, upgrade), 1", consumed, code, completed, want)
				}
				return
			}
			if consumed != len(test.input) || code != CodeNone || completed != 1 {
				t.Fatalf("Parse = (%d, %v), completed=%d", consumed, code, completed)
			}
		})
	}
}

func TestParserUpgradeRequestDoesNotSwitchProtocols(t *testing.T) {
	input := []byte("GET /chat HTTP/1.1\r\nHost: example.test\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n\r\n")
	var message Message
	callbacks := Callbacks{MessageComplete: func(completed Message) { message = completed }}
	parser := NewParser(Request, &callbacks, Limits{})
	consumed, code := parser.Parse(input)
	if consumed != len(input) || code != CodeNone || parser.Upgraded() {
		t.Fatalf("Parse = (%d, %v), upgraded=%v", consumed, code, parser.Upgraded())
	}
	if !message.UpgradeRequested || message.ConnectRequested {
		t.Fatalf("message upgrade/connect = %v/%v", message.UpgradeRequested, message.ConnectRequested)
	}
}

func TestParserResponseUpgradeRequires101(t *testing.T) {
	input := []byte("HTTP/1.1 200 OK\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nContent-Length: 0\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n")
	var completed int
	callbacks := Callbacks{MessageComplete: func(Message) { completed++ }}
	parser := NewParser(Response, &callbacks, Limits{})
	consumed, code := parser.Parse(input)
	if consumed != len(input) || code != CodeNone || completed != 2 || parser.Upgraded() {
		t.Fatalf("Parse = (%d, %v), completed=%d upgraded=%v", consumed, code, completed, parser.Upgraded())
	}

	upgrade := []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nwebsocket bytes")
	parser.Reset()
	consumed, code = parser.Parse(upgrade)
	want := bytes.Index(upgrade, []byte("websocket bytes"))
	if consumed != want || code != CodeUpgrade || !parser.Upgraded() {
		t.Fatalf("101 Parse = (%d, %v), upgraded=%v; want (%d, upgrade), true", consumed, code, parser.Upgraded(), want)
	}
}

func TestParserRejectsAmbiguousAndMalformedMessages(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		input string
		want  Code
	}{
		{name: "missing-host", kind: Request, input: "GET / HTTP/1.1\r\n\r\n", want: CodeMissingHost},
		{name: "duplicate-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n", want: CodeDuplicateHost},
		{name: "empty-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: \t\r\n\r\n", want: CodeMissingHost},
		{name: "space-in-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: example test\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "unterminated-ip-literal-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: [::1\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "reg-name-in-ip-literal-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: [not-an-ip]\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "malformed-ip-literal-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: [:::]\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "raw-zone-ip-literal-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: [fe80::1%eth0]\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "bracketed-ipv4-host", kind: Request, input: "GET / HTTP/1.1\r\nHost: [192.0.2.1]\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "reg-name-in-connect-ip-literal", kind: Request, input: "CONNECT [example]:443 HTTP/1.1\r\nHost: example\r\n\r\n", want: CodeInvalidTarget},
		{name: "nondigit-host-port", kind: Request, input: "GET / HTTP/1.1\r\nHost: example.test:http\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "bad-host-percent-encoding", kind: Request, input: "GET / HTTP/1.1\r\nHost: example%zz.test\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "empty-upgrade", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nConnection: upgrade\r\nUpgrade:\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "malformed-upgrade", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nConnection: upgrade\r\nUpgrade: websocket bad\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "empty-upgrade-version", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nConnection: upgrade\r\nUpgrade: h2c/\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "content-length-and-transfer-encoding", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", want: CodeContentLengthConflict},
		{name: "duplicate-content-length", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\nx", want: CodeInvalidContentLength},
		{name: "content-length-overflow", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 18446744073709551616\r\n\r\n", want: CodeInvalidContentLength},
		{name: "content-length-internal-space", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 1 1\r\n\r\n", want: CodeInvalidContentLength},
		{name: "transfer-encoding-not-final-chunked", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked, gzip\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer-encoding-with-chunk-parameter", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked;bad=yes\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer-encoding-malformed-parameter", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: gzip;level, chunked\r\n\r\n0\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer-encoding-on-http-1-0", kind: Request, input: "POST / HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "duplicate-chunked-response-coding", kind: Response, input: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked, chunked\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "content-length-on-204", kind: Response, input: "HTTP/1.1 204 No Content\r\nContent-Length: 1\r\n\r\n", want: CodeContentLengthConflict},
		{name: "switching-protocols-without-upgrade", kind: Response, input: "HTTP/1.1 101 Switching Protocols\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "bare-lf", kind: Request, input: "GET / HTTP/1.1\nHost: a\n\n", want: CodeInvalidLineEnding},
		{name: "obs-fold", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nX: one\r\n two\r\n\r\n", want: CodeInvalidHeaderName},
		{name: "bad-header-name", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nBad Name: x\r\n\r\n", want: CodeInvalidHeaderName},
		{name: "bad-chunk-size", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\nz\r\n", want: CodeInvalidChunkSize},
		{name: "del-in-chunk-extension", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1;x=\"a\x7fb\"\r\na\r\n0\r\n\r\n", want: CodeInvalidChunkExtension},
		{name: "escaped-nul-in-chunk-extension", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1;x=\"a\\\x00b\"\r\na\r\n0\r\n\r\n", want: CodeInvalidChunkExtension},
		{name: "forbidden-trailer", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nContent-Length: 1\r\n\r\n", want: CodeInvalidHeaderName},
		{name: "connection-trailer", kind: Request, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nConnection: close\r\n\r\n", want: CodeInvalidHeaderName},
		{name: "empty-target", kind: Request, input: "GET  HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeInvalidTarget},
		{name: "unsupported-version", kind: Request, input: "GET / HTTP/2.0\r\nHost: a\r\n\r\n", want: CodeInvalidVersion},
		{name: "leading-zero-version", kind: Response, input: "HTTP/01.1 200 OK\r\n\r\n", want: CodeInvalidVersion},
		{name: "multi-digit-minor-version", kind: Response, input: "HTTP/1.01 200 OK\r\n\r\n", want: CodeInvalidStartLine},
		{name: "non-ascii-target", kind: Request, input: "GET /\xce\xb4 HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeInvalidTarget},
		{name: "fragment-in-target", kind: Request, input: "GET /path#fragment HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeInvalidTarget},
		{name: "backslash-in-target", kind: Request, input: "GET /path\\evil HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeInvalidTarget},
		{name: "bad-target-percent-encoding", kind: Request, input: "GET /path%zz HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeInvalidTarget},
		{name: "bare-target", kind: Request, input: "GET relative HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeInvalidTarget},
		{name: "connect-target-without-port", kind: Request, input: "CONNECT example.test HTTP/1.1\r\nHost: example.test\r\n\r\n", want: CodeInvalidTarget},
		{name: "connect-target-with-path", kind: Request, input: "CONNECT example.test:443/path HTTP/1.1\r\nHost: example.test\r\n\r\n", want: CodeInvalidTarget},
		{name: "del-in-header", kind: Request, input: "GET / HTTP/1.1\r\nHost: a\r\nX: a\x7fb\r\n\r\n", want: CodeInvalidHeaderValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(test.kind, nil, Limits{})
			consumed, code := parser.Parse([]byte(test.input))
			if code != test.want {
				t.Fatalf("Parse consumed %d and returned %v, want %v", consumed, code, test.want)
			}
			if consumed, again := parser.Parse([]byte("anything")); consumed != 0 || again != test.want {
				t.Fatalf("sticky Parse = (%d, %v), want (0, %v)", consumed, again, test.want)
			}
		})
	}
}

func TestParserLimits(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		input  string
		want   Code
	}{
		{name: "start-line", limits: Limits{MaxStartLineBytes: 8}, input: "GET /long HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeStartLineTooLarge},
		{name: "headers", limits: Limits{MaxHeaderBytes: 8}, input: "GET / HTTP/1.1\r\nHost: abc\r\n\r\n", want: CodeHeadersTooLarge},
		{name: "header-name", limits: Limits{MaxHeaderNameBytes: 3}, input: "GET / HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeHeaderNameTooLarge},
		{name: "leading-empty-lines", limits: Limits{MaxStartLineBytes: 3}, input: "\r\n\r\nGET / HTTP/1.1\r\nHost: a\r\n\r\n", want: CodeStartLineTooLarge},
		{name: "chunk-extension", limits: Limits{MaxChunkLineBytes: 8}, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1;long=yes\r\nx\r\n0\r\n\r\n", want: CodeChunkLineTooLarge},
		{name: "chunk-count", limits: Limits{MaxChunks: 1}, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n1\r\nb\r\n0\r\n\r\n", want: CodeTooManyChunks},
		{name: "chunk-metadata", limits: Limits{MaxChunkMetadataBytes: 5}, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1\r\na\r\n1\r\nb\r\n0\r\n\r\n", want: CodeChunkMetadataTooLarge},
		{name: "header-count", limits: Limits{MaxHeaders: 1}, input: "GET / HTTP/1.1\r\nHost: a\r\nX: y\r\n\r\n", want: CodeTooManyHeaders},
		{name: "fixed-body", limits: Limits{MaxBodyBytes: 3}, input: "POST / HTTP/1.1\r\nHost: a\r\nContent-Length: 4\r\n\r\ndata", want: CodeBodyTooLarge},
		{name: "chunked-body", limits: Limits{MaxBodyBytes: 3}, input: "POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n4\r\ndata\r\n0\r\n\r\n", want: CodeBodyTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(Request, nil, test.limits)
			_, code := parser.Parse([]byte(test.input))
			if code != test.want {
				t.Fatalf("Parse = %v, want %v", code, test.want)
			}
		})
	}
}

func TestParserMaximumHeaderCountDoesNotWrap(t *testing.T) {
	const maxHeaders = ^uint16(0)
	var input strings.Builder
	input.Grow(16 + int(maxHeaders)*6)
	input.WriteString("GET / HTTP/1.1\r\nHost: a\r\n")
	for index := 1; index < int(maxHeaders); index++ {
		input.WriteString("X: y\r\n")
	}
	input.WriteString("X: overflow\r\n\r\n")
	parser := NewParser(Request, nil, Limits{MaxHeaders: maxHeaders, MaxHeaderBytes: 1 << 20})
	if _, code := parser.Parse([]byte(input.String())); code != CodeTooManyHeaders {
		t.Fatalf("Parse = %v, want too many headers", code)
	}
}

func TestParserFinishRejectsTruncation(t *testing.T) {
	inputs := []string{
		"GET / HTTP/1.1\r",
		"GET / HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r",
	}
	for _, input := range inputs {
		parser := NewParser(Request, nil, Limits{})
		if _, code := parser.Parse([]byte(input)); code != CodeNone {
			t.Fatalf("Parse(%q) = %v", input, code)
		}
		if code := parser.Finish(); code != CodeUnexpectedEOF {
			t.Fatalf("Finish(%q) = %v, want unexpected EOF", input, code)
		}
	}
}

var benchmarkRequest = []byte("POST /api/v1/resource HTTP/1.1\r\nHost: example.test\r\nUser-Agent: wago-benchmark\r\nContent-Type: application/octet-stream\r\nContent-Length: 16\r\n\r\n0123456789abcdef")
var benchmarkPipeline = bytes.Repeat(benchmarkRequest, 64)
var benchmarkChunkedRequest = []byte("POST / HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n\r\n4;name=value\r\ndata\r\n0\r\n\r\n")

func noopMessage()                            {}
func noopSpan([]byte)                         {}
func noopMetadata(Message)                    {}
func noopHeaderEnd(bool)                      {}
func noopChunkHeader(uint64)                  {}
func noopResponseContext(uint64) (bool, bool) { return false, false }

var noopCallbacks = Callbacks{
	ResponseContext: noopResponseContext,
	MessageBegin:    noopMessage, Method: noopSpan, Target: noopSpan, Reason: noopSpan,
	StartLine: noopMetadata, HeaderName: noopSpan, HeaderValue: noopSpan,
	HeaderEnd: noopHeaderEnd, HeadersComplete: noopMetadata, ChunkHeader: noopChunkHeader,
	Body: noopSpan, ChunkComplete: noopMessage, MessageComplete: noopMetadata,
}

func TestParserRejectsReentrantCallbacks(t *testing.T) {
	input := []byte("POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 4\r\n\r\ndata")
	tests := []struct {
		name   string
		mutate func(*Parser)
	}{
		{name: "reset", mutate: func(parser *Parser) { parser.Reset() }},
		{name: "nested-parse", mutate: func(parser *Parser) { _, _ = parser.Parse(nil) }},
		{name: "finish", mutate: func(parser *Parser) { _ = parser.Finish() }},
		{name: "init", mutate: func(parser *Parser) { parser.Init(Request, nil, Limits{}) }},
		{name: "response-context", mutate: func(parser *Parser) { parser.SetResponseContext(true, false) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var parser Parser
			callbacks := Callbacks{Body: func([]byte) { test.mutate(&parser) }}
			parser.Init(Request, &callbacks, Limits{})
			consumed, code := parser.Parse(input)
			if code != CodeReentrantCall {
				t.Fatalf("Parse consumed %d and returned %v", consumed, code)
			}
			if parser.Code() != CodeReentrantCall {
				t.Fatalf("sticky code = %v", parser.Code())
			}
		})
	}
}

func TestParserCallbackSpansAreCapacityLimited(t *testing.T) {
	input := []byte("POST /path HTTP/1.1\r\nHost: example.test\r\nX-Test: value\r\nContent-Length: 4\r\n\r\ndata")
	var spans int
	check := func(value []byte) {
		spans++
		if cap(value) != len(value) {
			t.Fatalf("span len/cap = %d/%d", len(value), cap(value))
		}
	}
	callbacks := Callbacks{Method: check, Target: check, HeaderName: check, HeaderValue: check, Body: check}
	parser := NewParser(Request, &callbacks, Limits{})
	if consumed, code := parser.Parse(input); consumed != len(input) || code != CodeNone {
		t.Fatalf("Parse = (%d, %v)", consumed, code)
	}
	if spans == 0 {
		t.Fatal("no spans observed")
	}
}

func TestParserChunkedZeroAllocations(t *testing.T) {
	allocs := testing.AllocsPerRun(1000, func() {
		parser := NewParser(Request, nil, Limits{})
		consumed, code := parser.Parse(benchmarkChunkedRequest)
		if consumed != len(benchmarkChunkedRequest) || code != CodeNone {
			panic("unexpected chunked parse result")
		}
	})
	if allocs != 0 {
		t.Fatalf("chunked allocations per parse = %v, want 0", allocs)
	}
}

func TestParserNoopCallbacksZeroAllocations(t *testing.T) {
	allocs := testing.AllocsPerRun(1000, func() {
		parser := NewParser(Request, &noopCallbacks, Limits{})
		consumed, code := parser.Parse(benchmarkRequest)
		if consumed != len(benchmarkRequest) || code != CodeNone {
			panic("unexpected callback parse result")
		}
	})
	if allocs != 0 {
		t.Fatalf("no-op callback allocations per parse = %v, want 0", allocs)
	}
}

func TestParserIPLiteralAllocationBudget(t *testing.T) {
	input := []byte("GET / HTTP/1.1\r\nHost: [2001:db8::1]\r\n\r\n")
	allocs := testing.AllocsPerRun(1000, func() {
		parser := NewParser(Request, nil, Limits{})
		consumed, code := parser.Parse(input)
		if consumed != len(input) || code != CodeNone {
			panic("unexpected IP literal parse result")
		}
	})
	if allocs > 1 {
		t.Fatalf("IP literal allocations per parse = %v, want at most 1 from netip validation", allocs)
	}
}

func TestParserZeroAllocations(t *testing.T) {
	allocs := testing.AllocsPerRun(1000, func() {
		parser := NewParser(Request, nil, Limits{})
		consumed, code := parser.Parse(benchmarkRequest)
		if consumed != len(benchmarkRequest) || code != CodeNone {
			panic("unexpected parse result")
		}
	})
	if allocs != 0 {
		t.Fatalf("allocations per parse = %v, want 0", allocs)
	}
}

func BenchmarkParserContiguous(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkRequest)))
	for b.Loop() {
		parser := NewParser(Request, nil, Limits{})
		consumed, code := parser.Parse(benchmarkRequest)
		if consumed != len(benchmarkRequest) || code != CodeNone {
			b.Fatal(consumed, code)
		}
	}
}

func BenchmarkParserPipelined(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkPipeline)))
	for b.Loop() {
		parser := NewParser(Request, nil, Limits{})
		consumed, code := parser.Parse(benchmarkPipeline)
		if consumed != len(benchmarkPipeline) || code != CodeNone || parser.MessageNumber() != 64 {
			b.Fatal(consumed, code, parser.MessageNumber())
		}
	}
}

func BenchmarkParserByteAtATime(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkRequest)))
	for b.Loop() {
		parser := NewParser(Request, nil, Limits{})
		for offset := range benchmarkRequest {
			consumed, code := parser.Parse(benchmarkRequest[offset : offset+1])
			if consumed != 1 || code != CodeNone {
				b.Fatal(offset, consumed, code)
			}
		}
	}
}

func FuzzParserNoPanic(f *testing.F) {
	f.Add(uint8(Request), benchmarkRequest, []byte{1, 2, 3, 5, 8})
	f.Add(uint8(Response), []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"), []byte{7, 1})
	f.Fuzz(func(t *testing.T, rawKind uint8, input, segmentation []byte) {
		kind := Kind(rawKind%2 + 1)
		limits := Limits{MaxStartLineBytes: 1024, MaxHeaderBytes: 4096, MaxBodyBytes: 8192}

		contiguous := NewParser(kind, nil, limits)
		_, contiguousCode := contiguous.Parse(input)
		if contiguousCode == CodeNone {
			contiguousCode = contiguous.Finish()
		}

		segmented := NewParser(kind, nil, limits)
		offset := 0
		segment := 0
		segmentedCode := CodeNone
		for offset < len(input) && segmentedCode == CodeNone {
			step := 1
			if len(segmentation) != 0 {
				step += int(segmentation[segment%len(segmentation)]) % 31
			}
			if step > len(input)-offset {
				step = len(input) - offset
			}
			consumed, code := segmented.Parse(input[offset : offset+step])
			if consumed < 0 || consumed > step {
				t.Fatalf("invalid consumption %d of %d", consumed, step)
			}
			if code == CodeNone && consumed != step {
				t.Fatalf("successful parse consumed %d of %d", consumed, step)
			}
			offset += consumed
			segmentedCode = code
			segment++
		}
		if segmentedCode == CodeNone {
			segmentedCode = segmented.Finish()
		}
		if segmentedCode != contiguousCode || segmented.MessageNumber() != contiguous.MessageNumber() {
			t.Fatalf("segmented result = %v/%d, contiguous = %v/%d", segmentedCode, segmented.MessageNumber(), contiguousCode, contiguous.MessageNumber())
		}
	})
}

func messageBody(messages []recordedMessage) string {
	if len(messages) == 0 {
		return ""
	}
	return string(messages[0].body)
}

func equalUint64s(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
