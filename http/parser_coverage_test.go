package http

import (
	"strings"
	"testing"
)

func TestParserStartLineLimitAtEveryState(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		limit uint32
		input string
	}{
		{name: "leading empty line CR", kind: Request, limit: 2, input: "\r\n\r"},
		{name: "leading empty line LF", kind: Request, limit: 1, input: "\r\n"},
		{name: "method token", kind: Request, limit: 1, input: "GG"},
		{name: "method separator", kind: Request, limit: 1, input: "G "},
		{name: "target separator", kind: Request, limit: 3, input: "G / "},
		{name: "HTTP literal", kind: Request, limit: 4, input: "G / H"},
		{name: "version major", kind: Request, limit: 9, input: "G / HTTP/1"},
		{name: "version dot", kind: Request, limit: 10, input: "G / HTTP/1."},
		{name: "version minor", kind: Request, limit: 11, input: "G / HTTP/1.1"},
		{name: "request CR", kind: Request, limit: 12, input: "G / HTTP/1.1\r"},
		{name: "request LF", kind: Request, limit: 13, input: "G / HTTP/1.1\r\n"},
		{name: "response status separator", kind: Response, limit: 8, input: "HTTP/1.1 "},
		{name: "response status digit", kind: Response, limit: 9, input: "HTTP/1.1 2"},
		{name: "response reason separator", kind: Response, limit: 12, input: "HTTP/1.1 200 "},
		{name: "response reason byte", kind: Response, limit: 13, input: "HTTP/1.1 200 O"},
		{name: "response reason CR", kind: Response, limit: 14, input: "HTTP/1.1 200 O\r"},
		{name: "response LF", kind: Response, limit: 15, input: "HTTP/1.1 200 O\r\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(test.kind, nil, Limits{MaxStartLineBytes: test.limit})
			_, code := parser.Parse([]byte(test.input))
			if code != CodeStartLineTooLarge {
				t.Fatalf("Parse = %v, want start line too large", code)
			}
		})
	}
}

func TestParserHeaderAndChunkLineLimitAtEveryState(t *testing.T) {
	const request = "GET / HTTP/1.1\r\n"
	headerTests := []struct {
		name  string
		limit uint32
		input string
	}{
		{name: "header name", limit: 1, input: "Ho"},
		{name: "header colon", limit: 4, input: "Host:"},
		{name: "leading value whitespace", limit: 5, input: "Host: "},
		{name: "value CR", limit: 6, input: "Host:a\r"},
		{name: "value LF", limit: 7, input: "Host:a\r\n"},
		{name: "terminating CR", limit: 8, input: "Host:a\r\n\r"},
		{name: "terminating LF", limit: 9, input: "Host:a\r\n\r\n"},
	}
	for _, test := range headerTests {
		t.Run("header/"+test.name, func(t *testing.T) {
			parser := NewParser(Request, nil, Limits{MaxHeaderBytes: test.limit})
			_, code := parser.Parse([]byte(request + test.input))
			if code != CodeHeadersTooLarge {
				t.Fatalf("Parse = %v, want headers too large", code)
			}
		})
	}

	const chunked = "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n"
	chunkTests := []struct {
		name  string
		limit uint32
		input string
	}{
		{name: "size digit", limit: 1, input: "12"},
		{name: "extension semicolon", limit: 1, input: "1;"},
		{name: "size CR", limit: 1, input: "1\r"},
		{name: "extension CR", limit: 3, input: "1;x\r"},
	}
	for _, test := range chunkTests {
		t.Run("chunk/"+test.name, func(t *testing.T) {
			parser := NewParser(Request, nil, Limits{MaxChunkLineBytes: test.limit})
			_, code := parser.Parse([]byte(chunked + test.input))
			if code != CodeChunkLineTooLarge {
				t.Fatalf("Parse = %v, want chunk line too large", code)
			}
		})
	}
}

func TestParserCallbackFailureAtTerminalStates(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		input     string
		callbacks func(*Parser) *Callbacks
	}{
		{
			name: "response start line", kind: Response,
			input: "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{StartLine: func(Message) { parser.Reset() }}
			},
		},
		{
			name: "close-delimited body", kind: Response,
			input: "HTTP/1.1 200 OK\r\n\r\nbody",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{Body: func([]byte) { parser.Reset() }}
			},
		},
		{
			name: "chunk body", kind: Request,
			input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{Body: func([]byte) { parser.Reset() }}
			},
		},
		{
			name: "terminal chunk complete", kind: Request,
			input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{ChunkComplete: func() { parser.Reset() }}
			},
		},
		{
			name: "chunked message complete", kind: Request,
			input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{MessageComplete: func(Message) { parser.Reset() }}
			},
		},
		{
			name: "no-body response complete", kind: Response,
			input: "HTTP/1.1 204 No Content\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{MessageComplete: func(Message) { parser.Reset() }}
			},
		},
		{
			name: "zero-length complete", kind: Response,
			input: "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{MessageComplete: func(Message) { parser.Reset() }}
			},
		},
		{
			name: "request complete", kind: Request,
			input: "GET / HTTP/1.1\r\nHost: e\r\n\r\n",
			callbacks: func(parser *Parser) *Callbacks {
				return &Callbacks{MessageComplete: func(Message) { parser.Reset() }}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var parser Parser
			parser.Init(test.kind, test.callbacks(&parser), Limits{})
			_, code := parser.Parse([]byte(test.input))
			if code != CodeReentrantCall {
				t.Fatalf("Parse = %v, want reentrant call", code)
			}
		})
	}
}

func TestParserUpgradeAndFinishTerminalCalls(t *testing.T) {
	input := []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	parser := NewParser(Response, nil, Limits{})
	if consumed, code := parser.Parse(input); consumed != len(input) || code != CodeUpgrade {
		t.Fatalf("upgrade Parse = (%d, %v)", consumed, code)
	}
	if consumed, code := parser.Parse([]byte("opaque")); consumed != 0 || code != CodeUpgrade {
		t.Fatalf("post-upgrade Parse = (%d, %v)", consumed, code)
	}
	if code := parser.Finish(); code != CodeUpgrade {
		t.Fatalf("post-upgrade Finish = %v", code)
	}

	idle := NewParser(Request, nil, Limits{})
	if code := idle.Finish(); code != CodeNone {
		t.Fatalf("idle Finish = %v", code)
	}

	failed := NewParser(Request, nil, Limits{})
	_, want := failed.Parse([]byte("bad request\r\n"))
	if got := failed.Finish(); got != want {
		t.Fatalf("sticky Finish = %v, want %v", got, want)
	}
}

func TestParserFramingAndMatcherEdgeBranches(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		input string
		want  Code
	}{
		{name: "content length after transfer encoding", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n", want: CodeContentLengthConflict},
		{name: "duplicate transfer encoding", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "request nonchunked transfer encoding", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "duplicate chunked response", kind: Response, input: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked, chunked \r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "bad request line LF", kind: Request, input: "GET / HTTP/1.1\rX", want: CodeInvalidLineEnding},
		{name: "parameter after spaced chunked", kind: Request, input: "POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked ;x=y\r\n\r\n", want: CodeInvalidTransferEncoding},
		{name: "invalid initial target form", kind: Request, input: "GET ! HTTP/1.1\r\n", want: CodeInvalidTarget},
		{name: "overlong CONNECT literal", kind: Request, input: "CONNECT [" + strings.Repeat("a", maxIPLiteralBytes+1) + "]:443 HTTP/1.1\r\n", want: CodeInvalidTarget},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parser := NewParser(test.kind, nil, Limits{})
			_, code := parser.Parse([]byte(test.input))
			if code != test.want {
				t.Fatalf("Parse = %v, want %v", code, test.want)
			}
		})
	}

	valid := []string{
		"POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip , chunked\r\n\r\n0\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;a  =b, chunked\r\n\r\n0\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: gzip;a=b;c=d, chunked\r\n\r\n0\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: e\r\nTransX: y\r\nConneX: y\r\nTraiX: y\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: [::1] \r\n\r\n",
		"GET / HTTP/1.1\r\nHost: example: \r\n\r\n",
		"GET / HTTP/1.1\r\nHost: example:80 \r\n\r\n",
		"POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\n\r\n1;x;y=q;z  =q;w=\"v\"  ;end\r\na\r\n0\r\n\r\n",
	}
	for index, input := range valid {
		t.Run("valid-"+string(rune('a'+index)), func(t *testing.T) {
			parser := NewParser(Request, nil, Limits{})
			if consumed, code := parser.Parse([]byte(input)); consumed != len(input) || code != CodeNone {
				t.Fatalf("Parse = (%d/%d, %v)", consumed, len(input), code)
			}
		})
	}
}

func TestParserDefensiveInternalStateFailures(t *testing.T) {
	t.Run("fixed body accounting", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{MaxBodyBytes: 1})
		parser.state = stateFixedBody
		parser.remaining = 1
		parser.bodyBytes = 1
		if _, code := parser.Parse([]byte("x")); code != CodeBodyTooLarge {
			t.Fatalf("Parse = %v", code)
		}
	})
	t.Run("chunk body accounting", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{MaxBodyBytes: 1})
		parser.state = stateChunkBody
		parser.remaining = 1
		parser.bodyBytes = 1
		if _, code := parser.Parse([]byte("x")); code != CodeBodyTooLarge {
			t.Fatalf("Parse = %v", code)
		}
	})
	t.Run("unknown parser state", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.state = parserState(255)
		if _, code := parser.Parse([]byte("x")); code != CodeInvalidStartLine {
			t.Fatalf("Parse = %v", code)
		}
	})
	t.Run("empty transfer token", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.transferState = transferToken
		if parser.consumeTransferValue(';') || parser.Code() != CodeInvalidTransferEncoding {
			t.Fatalf("transfer result = %v", parser.Code())
		}
	})
	t.Run("invalid quoted transfer byte", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.transferState = transferParamQuoted
		if parser.consumeTransferValue(0) || parser.Code() != CodeInvalidTransferEncoding {
			t.Fatalf("transfer result = %v", parser.Code())
		}
	})
	t.Run("invalid quoted transfer escape", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.transferState = transferParamQuotedEscape
		if parser.consumeTransferValue(0) || parser.Code() != CodeInvalidTransferEncoding {
			t.Fatalf("transfer result = %v", parser.Code())
		}
	})
	t.Run("unknown transfer state", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.transferState = transferState(255)
		if parser.consumeTransferValue('x') || parser.Code() != CodeInvalidTransferEncoding {
			t.Fatalf("transfer result = %v", parser.Code())
		}
	})
	t.Run("unknown finished transfer state", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.transferState = transferState(255)
		if parser.finishTransferValue() || parser.Code() != CodeInvalidTransferEncoding {
			t.Fatalf("transfer result = %v", parser.Code())
		}
	})
	t.Run("finished transfer without token", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.transferState = transferAfterToken
		if parser.finishTransferValue() || parser.Code() != CodeInvalidTransferEncoding {
			t.Fatalf("transfer result = %v", parser.Code())
		}
	})
	t.Run("unknown connection state", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.connectionState = connectionState(255)
		if parser.consumeConnectionValue('x') || parser.Code() != CodeInvalidHeaderValue {
			t.Fatalf("connection result = %v", parser.Code())
		}
	})
	t.Run("invalid quoted chunk byte", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.chunkExtState = chunkExtQuotedValue
		if parser.consumeChunkExtension(0) || parser.Code() != CodeInvalidChunkExtension {
			t.Fatalf("chunk result = %v", parser.Code())
		}
	})
	t.Run("invalid quoted chunk escape", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.chunkExtState = chunkExtQuotedEscape
		if parser.consumeChunkExtension(0) || parser.Code() != CodeInvalidChunkExtension {
			t.Fatalf("chunk result = %v", parser.Code())
		}
	})
	t.Run("unknown chunk extension state", func(t *testing.T) {
		parser := NewParser(Request, nil, Limits{})
		parser.chunkExtState = chunkExtState(255)
		if parser.consumeChunkExtension('x') || parser.Code() != CodeInvalidChunkExtension {
			t.Fatalf("chunk result = %v", parser.Code())
		}
	})
}
