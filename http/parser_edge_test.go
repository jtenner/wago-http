package http

import (
	"strings"
	"testing"
)

func TestParserResultAndMethodStrings(t *testing.T) {
	codes := []struct {
		code Code
		want string
	}{
		{CodeNone, "ok"},
		{CodeUpgrade, "protocol upgrade"},
		{CodeReentrantCall, "reentrant parser call"},
		{CodeInvalidKind, "invalid parser kind"},
		{CodeInvalidStartLine, "invalid start line"},
		{CodeInvalidMethod, "invalid method"},
		{CodeInvalidTarget, "invalid request target"},
		{CodeInvalidVersion, "invalid HTTP version"},
		{CodeInvalidStatus, "invalid response status"},
		{CodeInvalidHeaderName, "invalid header name"},
		{CodeInvalidHeaderValue, "invalid header value"},
		{CodeInvalidLineEnding, "invalid line ending"},
		{CodeStartLineTooLarge, "start line too large"},
		{CodeHeadersTooLarge, "headers too large"},
		{CodeHeaderNameTooLarge, "header name too large"},
		{CodeChunkLineTooLarge, "chunk line too large"},
		{CodeChunkMetadataTooLarge, "chunk metadata too large"},
		{CodeTooManyChunks, "too many chunks"},
		{CodeTooManyHeaders, "too many headers"},
		{CodeInvalidContentLength, "invalid Content-Length"},
		{CodeContentLengthConflict, "conflicting message framing"},
		{CodeInvalidTransferEncoding, "invalid Transfer-Encoding"},
		{CodeMissingHost, "missing Host header"},
		{CodeDuplicateHost, "duplicate Host header"},
		{CodeBodyTooLarge, "body too large"},
		{CodeInvalidChunkSize, "invalid chunk size"},
		{CodeInvalidChunkExtension, "invalid chunk extension"},
		{CodeUnexpectedEOF, "unexpected EOF"},
		{Code(255), "unknown HTTP parser result"},
	}
	for _, test := range codes {
		if got := test.code.String(); got != test.want {
			t.Errorf("Code(%d).String() = %q, want %q", test.code, got, test.want)
		}
	}

	methods := []struct {
		method Method
		want   string
	}{
		{MethodOther, "OTHER"},
		{MethodGET, "GET"},
		{MethodHEAD, "HEAD"},
		{MethodPOST, "POST"},
		{MethodPUT, "PUT"},
		{MethodDELETE, "DELETE"},
		{MethodCONNECT, "CONNECT"},
		{MethodOPTIONS, "OPTIONS"},
		{MethodTRACE, "TRACE"},
		{MethodPATCH, "PATCH"},
		{Method(255), "OTHER"},
	}
	for _, test := range methods {
		if got := test.method.String(); got != test.want {
			t.Errorf("Method(%d).String() = %q, want %q", test.method, got, test.want)
		}
	}
}

func TestParserLifecycleAndAccessors(t *testing.T) {
	var invalid Parser
	invalid.Init(Kind(255), nil, Limits{})
	if invalid.Code() != CodeInvalidKind {
		t.Fatalf("invalid Init = %v", invalid.Code())
	}
	if consumed, code := invalid.Parse([]byte("GET / HTTP/1.1\r\n")); consumed != 0 || code != CodeInvalidKind {
		t.Fatalf("sticky invalid parser = (%d, %v)", consumed, code)
	}

	parser := NewParser(Request, nil, Limits{})
	input := []byte("POST /data HTTP/1.1\r\nHost: example.test\r\nContent-Length: 4\r\n\r\nda")
	if consumed, code := parser.Parse(input); consumed != len(input) || code != CodeNone {
		t.Fatalf("partial Parse = (%d, %v)", consumed, code)
	}
	if parser.Kind() != Request || parser.Code() != CodeNone || parser.Method() != MethodPOST {
		t.Fatalf("identity accessors = kind %v code %v method %v", parser.Kind(), parser.Code(), parser.Method())
	}
	major, minor := parser.Version()
	if parser.Status() != 0 || major != 1 || minor != 1 {
		t.Fatalf("start-line accessors = status %d version %d.%d", parser.Status(), major, minor)
	}
	if length, present := parser.ContentLength(); !present || length != 4 {
		t.Fatalf("ContentLength = (%d, %v)", length, present)
	}
	if parser.BodyBytes() != 2 || parser.MessageNumber() != 0 || parser.ExchangeNumber() != 0 {
		t.Fatalf("progress accessors = body %d message %d exchange %d", parser.BodyBytes(), parser.MessageNumber(), parser.ExchangeNumber())
	}
	if parser.Trailers() || parser.Upgraded() || !parser.KeepAlive() {
		t.Fatalf("flags = trailers %v upgraded %v keepalive %v", parser.Trailers(), parser.Upgraded(), parser.KeepAlive())
	}
	parser.SetResponseContext(true, true)
	parser.Reset()
	if parser.Kind() != Request || parser.Code() != CodeNone || parser.MessageNumber() != 0 {
		t.Fatalf("Reset did not restore parser: kind %v code %v messages %d", parser.Kind(), parser.Code(), parser.MessageNumber())
	}

	chunked := NewParser(Request, nil, Limits{})
	prefix := []byte("POST / HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n")
	if consumed, code := chunked.Parse(prefix); consumed != len(prefix) || code != CodeNone {
		t.Fatalf("trailer prefix Parse = (%d, %v)", consumed, code)
	}
	if !chunked.Trailers() {
		t.Fatal("Trailers = false while parsing trailer section")
	}
}

func TestParserRejectsReentrancyFromEveryCallback(t *testing.T) {
	type callbackCase struct {
		name    string
		kind    Kind
		input   string
		install func(*Callbacks, func())
	}
	request := "POST /path HTTP/1.1\r\nHost: example.test\r\nContent-Length: 1\r\n\r\nx"
	chunked := "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n"
	response := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	tests := []callbackCase{
		{name: "response context", kind: Response, input: response, install: func(c *Callbacks, fail func()) {
			c.ResponseContext = func(uint64) (bool, bool) { fail(); return false, false }
		}},
		{name: "message begin", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.MessageBegin = fail }},
		{name: "method", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.Method = func([]byte) { fail() } }},
		{name: "target", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.Target = func([]byte) { fail() } }},
		{name: "reason", kind: Response, input: response, install: func(c *Callbacks, fail func()) { c.Reason = func([]byte) { fail() } }},
		{name: "start line", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.StartLine = func(Message) { fail() } }},
		{name: "header name", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.HeaderName = func([]byte) { fail() } }},
		{name: "header value", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.HeaderValue = func([]byte) { fail() } }},
		{name: "header end", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.HeaderEnd = func(bool) { fail() } }},
		{name: "headers complete", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.HeadersComplete = func(Message) { fail() } }},
		{name: "chunk header", kind: Request, input: chunked, install: func(c *Callbacks, fail func()) { c.ChunkHeader = func(uint64) { fail() } }},
		{name: "body", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.Body = func([]byte) { fail() } }},
		{name: "chunk complete", kind: Request, input: chunked, install: func(c *Callbacks, fail func()) { c.ChunkComplete = fail }},
		{name: "message complete", kind: Request, input: request, install: func(c *Callbacks, fail func()) { c.MessageComplete = func(Message) { fail() } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var parser Parser
			callbacks := Callbacks{}
			test.install(&callbacks, func() { parser.Reset() })
			parser.Init(test.kind, &callbacks, Limits{})
			_, code := parser.Parse([]byte(test.input))
			if code != CodeReentrantCall || parser.Code() != CodeReentrantCall {
				t.Fatalf("result = %v/%v, want reentrant call", code, parser.Code())
			}
		})
	}

	var closeParser Parser
	closeCallbacks := Callbacks{MessageComplete: func(Message) { closeParser.Reset() }}
	closeParser.Init(Response, &closeCallbacks, Limits{})
	if _, code := closeParser.Parse([]byte("HTTP/1.1 200 OK\r\n\r\nbody")); code != CodeNone {
		t.Fatalf("close-delimited Parse = %v", code)
	}
	if code := closeParser.Finish(); code != CodeReentrantCall {
		t.Fatalf("reentrant Finish completion = %v", code)
	}
}

func TestParserValidEdgeGrammar(t *testing.T) {
	requestMethods := "GET / HTTP/1.1\r\nHost: e\r\n\r\n" +
		"HEAD / HTTP/1.1\r\nHost: e\r\n\r\n" +
		"PUT / HTTP/1.1\r\nHost: e\r\nContent-Length: 0\r\n\r\n" +
		"DELETE / HTTP/1.1\r\nHost: e\r\n\r\n" +
		"OPTIONS * HTTP/1.1\r\nHost: e\r\n\r\n" +
		"TRACE / HTTP/1.1\r\nHost: e\r\n\r\n" +
		"PATCH / HTTP/1.1\r\nHost: e\r\nContent-Length: 0\r\n\r\n" +
		"CUSTOM / HTTP/1.1\r\nHost: e\r\n\r\n"
	tests := []struct {
		name      string
		kind      Kind
		input     string
		callbacks *Callbacks
		finish    bool
		wantCode  Code
		messages  uint64
	}{
		{name: "all request methods", kind: Request, input: requestMethods, messages: 8},
		{name: "leading empty line and HTTP 1.0", kind: Request, input: "\r\nOPTIONS * HTTP/1.0\r\nConnection: keep-alive, custom\r\n\r\n", messages: 1},
		{name: "absolute target and escaped host", kind: Request, input: "GET http://example.test/a%20b?q=x HTTP/1.1\r\nHost: ex%61mple.test:8080\r\n\r\n", messages: 1},
		{name: "IPv6 zone and empty port", kind: Request, input: "GET / HTTP/1.1\r\nHost: [fe80::1%25eth0]:\r\n\r\n", messages: 1},
		{name: "transfer parameters", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip; level = \"a\\b\"; flag=token, chunked\r\n\r\n0\r\n\r\n", messages: 1},
		{name: "transfer optional whitespace states", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip \t; level = token \t, chunked\r\n\r\n0\r\n\r\n", messages: 1},
		{name: "chunk extensions and trailers", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\nTrailer: X-End\r\n\r\n1; one ; two = token ; three = \"a\\b\"\r\nx\r\n0; done\r\nX-End: yes\r\n\r\n", messages: 1},
		{name: "upgrade protocol list", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket/13, h2c\r\n\r\n", messages: 1},
		{name: "upgrade optional whitespace states", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: close \t, Upgrade\r\nUpgrade: websocket \t, h2c/1 \t, custom\r\n\r\n", messages: 1},
		{name: "CONNECT IPv6 authority", kind: Request, input: "CONNECT [2001:db8::1]:443 HTTP/1.1\r\nHost: [2001:db8::1]:443\r\n\r\n", messages: 1},
		{name: "response empty reason", kind: Response, input: "HTTP/1.1 204 \r\nServer: e\r\n\r\n", messages: 1},
		{name: "response non-final transfer coding", kind: Response, input: "HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip; q=\"x\"\r\n\r\ncompressed", finish: true, messages: 1},
		{name: "HTTP 1.0 response keepalive", kind: Response, input: "HTTP/1.0 200 OK\r\nContent-Length: 0\r\nConnection: keep-alive\r\n\r\n", messages: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(test.kind, test.callbacks, Limits{})
			input := []byte(test.input)
			consumed, code := parser.Parse(input)
			if consumed != len(input) || code != test.wantCode {
				t.Fatalf("Parse = (%d/%d, %v), want code %v", consumed, len(input), code, test.wantCode)
			}
			if test.finish {
				code = parser.Finish()
				if code != CodeNone {
					t.Fatalf("Finish = %v", code)
				}
			}
			if parser.MessageNumber() != test.messages {
				t.Fatalf("messages = %d, want %d", parser.MessageNumber(), test.messages)
			}
		})
	}
}

func TestIPLiteralValidationEdges(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "::1", want: true},
		{value: "fe80::1%25eth0", want: true},
		{value: "", want: false},
		{value: "127.0.0.1", want: false},
		{value: "%", want: false},
		{value: "%GG", want: false},
		{value: "%00", want: false},
		{value: strings.Repeat("a", maxIPLiteralBytes+1), want: false},
	}
	for _, test := range tests {
		if got := validIPLiteral([]byte(test.value)); got != test.want {
			t.Errorf("validIPLiteral(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestParserRejectsEdgeGrammar(t *testing.T) {
	longLiteral := strings.Repeat("a", maxIPLiteralBytes+1)
	tests := []struct {
		name   string
		kind   Kind
		input  string
		limits Limits
		want   Code
	}{
		{name: "bad leading line", kind: Request, input: "\rX", want: CodeInvalidLineEnding},
		{name: "empty method", kind: Request, input: " / HTTP/1.1\r\n", want: CodeInvalidMethod},
		{name: "method separator", kind: Request, input: "GET\t/ HTTP/1.1\r\n", want: CodeInvalidMethod},
		{name: "bad HTTP literal", kind: Request, input: "GET / HTTX/1.1\r\n", want: CodeInvalidStartLine},
		{name: "bad major", kind: Request, input: "GET / HTTP/x.1\r\n", want: CodeInvalidVersion},
		{name: "bad dot", kind: Request, input: "GET / HTTP/1x1\r\n", want: CodeInvalidVersion},
		{name: "bad minor", kind: Request, input: "GET / HTTP/1.x\r\n", want: CodeInvalidVersion},
		{name: "unsupported version", kind: Request, input: "GET / HTTP/2.0\r\n", want: CodeInvalidVersion},
		{name: "short status", kind: Response, input: "HTTP/1.1 2x0 OK\r\n", want: CodeInvalidStatus},
		{name: "status below 100", kind: Response, input: "HTTP/1.1 099 Nope\r\n", want: CodeInvalidStatus},
		{name: "missing status space", kind: Response, input: "HTTP/1.1x200 OK\r\n", want: CodeInvalidStartLine},
		{name: "missing reason separator", kind: Response, input: "HTTP/1.1 200\r\n\r\n", want: CodeInvalidStatus},
		{name: "bad reason control", kind: Response, input: "HTTP/1.1 200 O\x01K\r\n", want: CodeInvalidStatus},
		{name: "bad response line ending", kind: Response, input: "HTTP/1.1 200 OK\rX", want: CodeInvalidLineEnding},
		{name: "empty header name", kind: Request, input: "GET / HTTP/1.1\r\n: x\r\n", want: CodeInvalidHeaderName},
		{name: "folded header", kind: Request, input: "GET / HTTP/1.1\r\n value\r\n", want: CodeInvalidHeaderName},
		{name: "bad header value control", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\x01\r\n", want: CodeInvalidHeaderValue},
		{name: "bad header ending", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\rX", want: CodeInvalidLineEnding},
		{name: "empty transfer encoding", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: \r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer trailing comma", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip,\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer empty parameter", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer parameter without value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;foo\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer parameter empty value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;foo=\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer unterminated quote", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;foo=\"x\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer escaped control", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;foo=\"x\\\x00\"\r\n", want: CodeInvalidHeaderValue},
		{name: "parameter on chunked", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked;foo=bar\r\n", want: CodeInvalidTransferEncoding},
		{name: "coding after chunked", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked, gzip\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad token delimiter", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip/\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad after token", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip /\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad parameter start", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;=x\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad after parameter name", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;x /\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad before parameter value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;x=(\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad token value delimiter", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;x=y/\r\n", want: CodeInvalidTransferEncoding},
		{name: "transfer bad after value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;x=y /\r\n", want: CodeInvalidTransferEncoding},
		{name: "empty upgrade", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nUpgrade: \r\n", want: CodeInvalidHeaderValue},
		{name: "upgrade missing name", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nUpgrade: /1\r\n", want: CodeInvalidHeaderValue},
		{name: "upgrade missing version", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nUpgrade: websocket/\r\n", want: CodeInvalidHeaderValue},
		{name: "upgrade trailing comma", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nUpgrade: websocket,\r\n", want: CodeInvalidHeaderValue},
		{name: "empty connection", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: \r\n", want: CodeInvalidHeaderValue},
		{name: "connection trailing comma", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: close,\r\n", want: CodeInvalidHeaderValue},
		{name: "connection parameter", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: close;x\r\n", want: CodeInvalidHeaderValue},
		{name: "connection leading comma", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: ,close\r\n", want: CodeInvalidHeaderValue},
		{name: "connection bad after whitespace", kind: Request, input: "GET / HTTP/1.1\r\nHost: e\r\nConnection: close /\r\n", want: CodeInvalidHeaderValue},
		{name: "host bad first escape", kind: Request, input: "GET / HTTP/1.1\r\nHost: %x0\r\n", want: CodeInvalidHeaderValue},
		{name: "host bad second escape", kind: Request, input: "GET / HTTP/1.1\r\nHost: %0x\r\n", want: CodeInvalidHeaderValue},
		{name: "host unfinished escape", kind: Request, input: "GET / HTTP/1.1\r\nHost: x%20%\r\n", want: CodeInvalidHeaderValue},
		{name: "host empty literal", kind: Request, input: "GET / HTTP/1.1\r\nHost: []\r\n", want: CodeInvalidHeaderValue},
		{name: "host IPvFuture unsupported", kind: Request, input: "GET / HTTP/1.1\r\nHost: [v1.example]\r\n", want: CodeInvalidHeaderValue},
		{name: "host unfinished literal", kind: Request, input: "GET / HTTP/1.1\r\nHost: [::1\r\n", want: CodeInvalidHeaderValue},
		{name: "host literal too long", kind: Request, input: "GET / HTTP/1.1\r\nHost: [" + longLiteral + "]\r\n", want: CodeInvalidHeaderValue},
		{name: "host suffix", kind: Request, input: "GET / HTTP/1.1\r\nHost: [::1]x\r\n", want: CodeInvalidHeaderValue},
		{name: "asterisk suffix", kind: Request, input: "OPTIONS *x HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "bad absolute scheme", kind: Request, input: "GET abc!def HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT bad first authority byte", kind: Request, input: "CONNECT :443 HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT path separator", kind: Request, input: "CONNECT host/443 HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT malformed literal", kind: Request, input: "CONNECT [bad]:443 HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT literal suffix", kind: Request, input: "CONNECT [::1]x:443 HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT nonnumeric port", kind: Request, input: "CONNECT host:x HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT missing port", kind: Request, input: "CONNECT host HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "CONNECT empty port", kind: Request, input: "CONNECT host: HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "chunk extension empty", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension missing value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x=\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension unterminated quote", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x=\"a\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension escaped control", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x=\"a\\\x00\"\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension bad name start", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;=x\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension bad name delimiter", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x/\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension bad after name", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x /\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension bad before value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x=(\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension bad token value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x=y/\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk extension bad after value", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x=y /\r\n", want: CodeInvalidChunkExtension},
		{name: "chunk body bad CR", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nxX", want: CodeInvalidLineEnding},
		{name: "chunk body bad LF", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\rX", want: CodeInvalidLineEnding},
		{name: "forbidden trailer", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nContent-Length: 0\r\n", want: CodeInvalidHeaderName},
		{name: "HTTP 1.0 transfer encoding", kind: Request, input: "POST / HTTP/1.0\r\nTransfer-Encoding: chunked\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "101 missing upgrade", kind: Response, input: "HTTP/1.1 101 Switching Protocols\r\n\r\n", want: CodeInvalidHeaderValue},
		{name: "informational framing", kind: Response, input: "HTTP/1.1 100 Continue\r\nContent-Length: 0\r\n\r\n", want: CodeContentLengthConflict},
		{name: "204 transfer framing", kind: Response, input: "HTTP/1.1 204 No Content\r\nTransfer-Encoding: chunked\r\n\r\n", want: CodeContentLengthConflict},
		{name: "close-delimited body limit", kind: Response, input: "HTTP/1.1 200 OK\r\n\r\nbody", limits: Limits{MaxBodyBytes: 3}, want: CodeBodyTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(test.kind, nil, test.limits)
			_, code := parser.Parse([]byte(test.input))
			if code == CodeNone {
				code = parser.Finish()
			}
			if code != test.want {
				t.Fatalf("result = %v, want %v", code, test.want)
			}
		})
	}
}
