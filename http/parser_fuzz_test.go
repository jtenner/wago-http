package http

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	stdhttp "net/http"
	"strconv"
	"strings"
	"testing"
)

func FuzzParserCallbackTrace(f *testing.F) {
	f.Add(uint8(Request), benchmarkChunkedRequest, []byte{1, 2, 3}, []byte{}, []byte{})
	f.Add(uint8(Request), []byte("POST / HTTP/1.1\r\nHost: e\r\nTransfer-Encoding: chunked\r\nTrailer: X-End\r\n\r\n1;x=\"y\"\r\na\r\n0\r\nX-End: yes\r\n\r\n"), []byte{1}, []byte{32, 64, 8}, []byte{})
	f.Add(uint8(Response), []byte("HTTP/1.1 103 Early Hints\r\nLink: </a>; rel=preload\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"), []byte{7, 1}, []byte{}, []byte{0x80})
	f.Add(uint8(Response), []byte("HTTP/1.1 200 Connection Established\r\n\r\nopaque"), []byte{1}, []byte{}, []byte{0x82})
	f.Add(uint8(Response), []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nnext"), []byte{3}, []byte{}, []byte{0x81})
	f.Add(uint8(Response), []byte("HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nHTTP/1.1 200 Connection Established\r\n\r\nopaque"), []byte{1, 7}, []byte{}, []byte{0, 1, 2})
	f.Fuzz(func(t *testing.T, rawKind uint8, input, segmentation, configuration, context []byte) {
		kind := Kind(rawKind%2 + 1)
		limits := parserFuzzLimits(configuration)
		contiguous := runParserTraceFuzz(kind, input, nil, false, limits, context)
		if !contiguous.valid {
			t.Fatal("contiguous callback trace violated span or consumption invariants")
		}
		segmented := runParserTraceFuzz(kind, input, segmentation, true, limits, context)
		if !segmented.valid {
			t.Fatal("segmented callback trace violated span or consumption invariants")
		}
		if segmented.outcome != contiguous.outcome {
			t.Fatalf("segmented result = %+v, contiguous = %+v", segmented.outcome, contiguous.outcome)
		}
		if !bytes.Equal(segmented.committedTrace, contiguous.committedTrace) || !bytes.Equal(segmented.committedBody, contiguous.committedBody) {
			t.Fatalf("committed callback output differs across segmentation")
		}
		if contiguous.outcome.code == CodeNone || contiguous.outcome.code == CodeUpgrade {
			if !bytes.Equal(segmented.trace, contiguous.trace) {
				t.Fatalf("callback trace differs across segmentation\nsegmented: %x\ncontiguous: %x", segmented.trace, contiguous.trace)
			}
			if !bytes.Equal(segmented.body, contiguous.body) {
				t.Fatalf("callback body differs across segmentation: %x != %x", segmented.body, contiguous.body)
			}
		}
	})
}

func FuzzParserPairDifferential(f *testing.F) {
	f.Add([]byte("body"), []byte{1, 2, 3}, []byte{1, 7}, uint8(0))
	f.Add([]byte("chunked body"), []byte{1, 3, 5}, []byte{1}, uint8(1))
	f.Add([]byte{}, []byte{}, []byte{7, 1}, uint8(1))
	f.Fuzz(func(t *testing.T, body, chunkPattern, segmentation []byte, framing uint8) {
		if len(body) > 256 {
			body = body[:256]
		}
		chunked := framing&1 != 0

		var request bytes.Buffer
		request.WriteString("POST /pair?x=1 HTTP/1.1\r\nHost: example.test\r\n")
		if chunked {
			request.WriteString("Transfer-Encoding: chunked\r\nTrailer: X-End\r\n\r\n")
			writeDifferentialChunks(&request, body, chunkPattern)
			request.WriteString("X-End: yes\r\n\r\n")
		} else {
			request.WriteString("Content-Length: ")
			request.WriteString(strconv.Itoa(len(body)))
			request.WriteString("\r\n\r\n")
			request.Write(body)
		}
		request.WriteString("GET /sentinel HTTP/1.1\r\nHost: example.test\r\n\r\n")
		requestCase := generatedParserCase{kind: Request, input: request.Bytes(), code: CodeNone, consumed: request.Len(), messages: 2, body: body}
		checkGeneratedParserCase(t, requestCase, segmentation)
		checkStandardLibraryRequests(t, request.Bytes(), body)

		var response bytes.Buffer
		response.WriteString("HTTP/1.1 200 OK\r\n")
		if chunked {
			response.WriteString("Transfer-Encoding: chunked\r\nTrailer: X-End\r\n\r\n")
			writeDifferentialChunks(&response, body, chunkPattern)
			response.WriteString("X-End: yes\r\n\r\n")
		} else {
			response.WriteString("Content-Length: ")
			response.WriteString(strconv.Itoa(len(body)))
			response.WriteString("\r\n\r\n")
			response.Write(body)
		}
		response.WriteString("HTTP/1.1 204 No Content\r\n\r\n")
		responseCase := generatedParserCase{kind: Response, input: response.Bytes(), context: []byte{0x80}, code: CodeNone, consumed: response.Len(), messages: 2, body: body}
		checkGeneratedParserCase(t, responseCase, segmentation)
		checkStandardLibraryResponses(t, response.Bytes(), body)
	})
}

func FuzzParserGeneratedMessages(f *testing.F) {
	f.Add([]byte("fixed request body and headers"), []byte{1, 2, 3, 5})
	f.Add([]byte("chunked request with trailers and quoted extensions"), []byte{1})
	f.Add([]byte("informational response pipeline"), []byte{7, 1})
	f.Add([]byte("successful CONNECT response"), []byte{2, 3})
	f.Fuzz(func(t *testing.T, data, segmentation []byte) {
		if len(data) > 256 {
			data = data[:256]
		}
		request := buildGeneratedRequest(data)
		checkGeneratedParserCase(t, request, segmentation)
		response := buildGeneratedResponse(data)
		checkGeneratedParserCase(t, response, segmentation)
	})
}

func FuzzParserStructuredFraming(f *testing.F) {
	f.Add([]byte("example.test"), []byte("4"), []byte("chunked"), []byte("data"), []byte{1, 2, 3}, uint8(1))
	f.Add([]byte("example.test"), []byte("1"), []byte("chunked"), []byte("0\r\n\r\n"), []byte{1}, uint8(3))
	f.Add([]byte(""), []byte("0, 0"), []byte("gzip, chunked"), []byte("0\r\n\r\n"), []byte{7}, uint8(0x27))
	f.Fuzz(func(t *testing.T, host, contentLength, transferEncoding, wireBody, segmentation []byte, flags uint8) {
		host = sanitizeStructuredValue(host, 96)
		contentLength = sanitizeStructuredValue(contentLength, 96)
		transferEncoding = sanitizeStructuredValue(transferEncoding, 128)
		if len(wireBody) > 256 {
			wireBody = wireBody[:256]
		}

		var request bytes.Buffer
		if flags&0x80 != 0 {
			request.WriteString("POST / HTTP/1.0\r\n")
		} else {
			request.WriteString("POST / HTTP/1.1\r\n")
		}
		if flags&0x40 == 0 {
			writeStructuredHeader(&request, fuzzHeaderCase("Host", flags^0x55), host)
		}
		if flags&0x20 != 0 {
			writeStructuredHeader(&request, fuzzHeaderCase("Host", flags^0xaa), host)
		}
		writeStructuredFramingHeaders(&request, contentLength, transferEncoding, flags)
		request.WriteString("\r\n")
		request.Write(wireBody)
		request.WriteString("GET /sentinel HTTP/1.1\r\nHost: sentinel.test\r\n\r\n")
		assertTraceSegmentationEquivalent(t, Request, request.Bytes(), segmentation, nil)

		var response bytes.Buffer
		response.WriteString("HTTP/1.1 200 OK\r\n")
		writeStructuredFramingHeaders(&response, contentLength, transferEncoding, flags)
		response.WriteString("\r\n")
		response.Write(wireBody)
		response.WriteString("HTTP/1.1 204 No Content\r\n\r\n")
		context := []byte{0x80 | flags&3}
		assertTraceSegmentationEquivalent(t, Response, response.Bytes(), segmentation, context)
	})
}

func FuzzParserStructuredResponseLine(f *testing.F) {
	f.Add([]byte("200"), []byte("OK"), []byte("4"), []byte("body"), []byte{1, 2, 3}, uint8(0))
	f.Add([]byte("103"), []byte("Early Hints"), []byte("0"), []byte{}, []byte{1}, uint8(0))
	f.Add([]byte("101"), []byte("Switching Protocols"), []byte("0"), []byte("opaque"), []byte{7}, uint8(3))
	f.Fuzz(func(t *testing.T, status, reason, contentLength, body, segmentation []byte, contextBits uint8) {
		status = sanitizeStructuredValue(status, 12)
		reason = sanitizeStructuredValue(reason, 128)
		contentLength = sanitizeStructuredValue(contentLength, 96)
		if len(body) > 256 {
			body = body[:256]
		}
		var wire bytes.Buffer
		wire.WriteString("HTTP/1.1 ")
		wire.Write(status)
		wire.WriteByte(' ')
		wire.Write(reason)
		wire.WriteString("\r\nContent-Length: ")
		wire.Write(contentLength)
		wire.WriteString("\r\n")
		if bytes.Equal(status, []byte("101")) {
			wire.WriteString("Connection: Upgrade\r\nUpgrade: websocket\r\n")
		}
		wire.WriteString("\r\n")
		wire.Write(body)
		wire.WriteString("HTTP/1.1 204 No Content\r\n\r\n")
		context := []byte{0x80 | contextBits&3}
		assertTraceSegmentationEquivalent(t, Response, wire.Bytes(), segmentation, context)
	})
}

func FuzzParserStructuredTargetAndHost(f *testing.F) {
	f.Add([]byte("example.test:443"), []byte("example.test:443"), uint8(0), []byte{1})
	f.Add([]byte("[2001:db8::1]:443"), []byte("[2001:db8::1]:443"), uint8(0), []byte{2, 3})
	f.Add([]byte("/path%20with%20escapes?x=y"), []byte("[fe80::1%25eth0]"), uint8(1), []byte{7})
	f.Add([]byte("http://example.test/absolute"), []byte("example.test"), uint8(2), []byte{13, 1})
	f.Fuzz(func(t *testing.T, target, host []byte, methodSelector uint8, segmentation []byte) {
		if len(target) > 192 {
			target = target[:192]
		}
		if len(host) > 128 {
			host = host[:128]
		}
		target = append([]byte(nil), target...)
		for index, b := range target {
			if b == '\r' || b == '\n' {
				target[index] = '!'
			}
		}
		host = sanitizeStructuredValue(host, 128)
		methods := [...]string{"CONNECT", "GET", "OPTIONS", "POST", "TRACE", "PATCH", "extension"}
		method := methods[int(methodSelector)%len(methods)]
		if method == "extension" {
			cursor := fuzzDataCursor{data: target}
			method = cursor.token(24)
		}
		var wire bytes.Buffer
		wire.WriteString(method)
		wire.WriteByte(' ')
		wire.Write(target)
		wire.WriteString(" HTTP/1.1\r\nHost: \t")
		wire.Write(host)
		wire.WriteString(" \t\r\nX-End: yes\r\n\r\n")
		wire.WriteString("GET /sentinel HTTP/1.1\r\nHost: sentinel.test\r\n\r\n")
		assertTraceSegmentationEquivalent(t, Request, wire.Bytes(), segmentation, nil)
	})
}

func FuzzParserStructuredChunked(f *testing.F) {
	f.Add([]byte("4;foo=bar"), []byte("data"), []byte("X-End"), []byte("yes"), []byte{1, 2, 3})
	f.Add([]byte("ffffffffffffffff"), []byte("x"), []byte("X-End"), []byte("yes"), []byte{1})
	f.Add([]byte("1;x=\"unterminated"), []byte("x"), []byte("Content-Length"), []byte("1"), []byte{7})
	f.Fuzz(func(t *testing.T, chunkLine, chunkData, trailerName, trailerValue, segmentation []byte) {
		chunkLine = sanitizeStructuredValue(chunkLine, 128)
		trailerValue = sanitizeStructuredValue(trailerValue, 96)
		if len(chunkData) > 256 {
			chunkData = chunkData[:256]
		}
		cursor := fuzzDataCursor{data: trailerName}
		name := "X-" + cursor.token(24)

		var wire bytes.Buffer
		wire.WriteString("POST /chunk HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\nTrailer: ")
		wire.WriteString(name)
		wire.WriteString("\r\n\r\n")
		wire.Write(chunkLine)
		wire.WriteString("\r\n")
		wire.Write(chunkData)
		wire.WriteString("\r\n0;done=\"yes\"\r\n")
		writeStructuredHeader(&wire, name, trailerValue)
		wire.WriteString("\r\n")
		assertTraceSegmentationEquivalent(t, Request, wire.Bytes(), segmentation, nil)
	})
}

func writeDifferentialChunks(destination *bytes.Buffer, body, pattern []byte) {
	remaining := body
	index := 0
	for len(remaining) != 0 {
		size := 1
		if len(pattern) != 0 {
			size += int(pattern[index%len(pattern)]) % 31
		}
		if size > len(remaining) {
			size = len(remaining)
		}
		destination.WriteString(strconv.FormatInt(int64(size), 16))
		destination.WriteString(";pair=value\r\n")
		destination.Write(remaining[:size])
		destination.WriteString("\r\n")
		remaining = remaining[size:]
		index++
	}
	destination.WriteString("0\r\n")
}

func checkStandardLibraryRequests(t *testing.T, wire, wantBody []byte) {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(wire))
	first, err := stdhttp.ReadRequest(reader)
	if err != nil {
		t.Fatalf("net/http first request: %v", err)
	}
	body, err := io.ReadAll(first.Body)
	first.Body.Close()
	if err != nil || !bytes.Equal(body, wantBody) {
		t.Fatalf("net/http first request body = %x/%v, want %x", body, err, wantBody)
	}
	second, err := stdhttp.ReadRequest(reader)
	if err != nil {
		t.Fatalf("net/http second request: %v", err)
	}
	secondBody, err := io.ReadAll(second.Body)
	second.Body.Close()
	if err != nil || len(secondBody) != 0 || second.Method != "GET" || second.RequestURI != "/sentinel" {
		t.Fatalf("net/http sentinel request = %s %s body %x err %v", second.Method, second.RequestURI, secondBody, err)
	}
	if reader.Buffered() != 0 {
		t.Fatalf("net/http left %d buffered request bytes", reader.Buffered())
	}
}

func checkStandardLibraryResponses(t *testing.T, wire, wantBody []byte) {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(wire))
	first, err := stdhttp.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("net/http first response: %v", err)
	}
	body, err := io.ReadAll(first.Body)
	first.Body.Close()
	if err != nil || !bytes.Equal(body, wantBody) {
		t.Fatalf("net/http first response body = %x/%v, want %x", body, err, wantBody)
	}
	second, err := stdhttp.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("net/http second response: %v", err)
	}
	secondBody, err := io.ReadAll(second.Body)
	second.Body.Close()
	if err != nil || len(secondBody) != 0 || second.StatusCode != 204 {
		t.Fatalf("net/http sentinel response = %d body %x err %v", second.StatusCode, secondBody, err)
	}
	if reader.Buffered() != 0 {
		t.Fatalf("net/http left %d buffered response bytes", reader.Buffered())
	}
}

func FuzzParserStickyAndReset(f *testing.F) {
	f.Add(uint8(Request), benchmarkRequest, []byte{1, 2, 3}, []byte{}, []byte("suffix"))
	f.Add(uint8(Request), []byte("GET / HTTP/1.1\r\nHost: e\rX"), []byte{1}, []byte{8, 8}, []byte("GET / HTTP/1.0\r\n\r\n"))
	f.Add(uint8(Response), []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nopaque"), []byte{4}, []byte{}, []byte("more opaque"))
	f.Fuzz(func(t *testing.T, rawKind uint8, input, segmentation, configuration, suffix []byte) {
		kind := Kind(rawKind%2 + 1)
		limits := parserFuzzLimits(configuration)
		fresh, valid := runParserFuzz(kind, input, segmentation, true, limits)
		if !valid {
			t.Fatal("fresh parser reported invalid consumption")
		}

		parser := NewParser(kind, nil, limits)
		first, valid := driveParserFuzz(&parser, input, segmentation, true)
		if !valid || first != fresh {
			t.Fatalf("first parse = %+v/%v, fresh = %+v", first, valid, fresh)
		}
		if first.code != CodeNone {
			consumed, code := parser.Parse(suffix)
			if consumed != 0 || code != first.code {
				t.Fatalf("terminal Parse = (%d, %v), want (0, %v)", consumed, code, first.code)
			}
			if code := parser.Finish(); code != first.code {
				t.Fatalf("terminal Finish = %v, want %v", code, first.code)
			}
		}

		parser.Reset()
		afterReset, valid := driveParserFuzz(&parser, input, segmentation, true)
		if !valid || afterReset != fresh {
			t.Fatalf("after Reset = %+v/%v, fresh = %+v", afterReset, valid, fresh)
		}
		parser.Init(kind, nil, limits)
		afterInit, valid := driveParserFuzz(&parser, input, segmentation, true)
		if !valid || afterInit != fresh {
			t.Fatalf("after Init = %+v/%v, fresh = %+v", afterInit, valid, fresh)
		}
	})
}

func sanitizeStructuredValue(value []byte, maximum int) []byte {
	if len(value) > maximum {
		value = value[:maximum]
	}
	result := append([]byte(nil), value...)
	for index, b := range result {
		if b == '\r' || b == '\n' {
			result[index] = ' '
		}
	}
	return result
}

func fuzzHeaderCase(name string, selector uint8) string {
	result := []byte(name)
	for index := range result {
		if result[index] >= 'a' && result[index] <= 'z' && selector&(1<<(index%8)) != 0 {
			result[index] -= 'a' - 'A'
		}
	}
	return string(result)
}

func writeStructuredHeader(destination *bytes.Buffer, name string, value []byte) {
	destination.WriteString(name)
	destination.WriteString(": \t")
	destination.Write(value)
	destination.WriteString(" \t\r\n")
}

func writeStructuredFramingHeaders(destination *bytes.Buffer, contentLength, transferEncoding []byte, flags uint8) {
	writeContentLength := func() {
		writeStructuredHeader(destination, fuzzHeaderCase("Content-Length", flags), contentLength)
	}
	writeTransferEncoding := func() {
		writeStructuredHeader(destination, fuzzHeaderCase("Transfer-Encoding", flags^0xff), transferEncoding)
	}
	if flags&0x10 != 0 {
		if flags&2 != 0 || flags&8 != 0 {
			writeTransferEncoding()
		}
		if flags&1 != 0 || flags&4 != 0 {
			writeContentLength()
		}
	} else {
		if flags&1 != 0 || flags&4 != 0 {
			writeContentLength()
		}
		if flags&2 != 0 || flags&8 != 0 {
			writeTransferEncoding()
		}
	}
	if flags&4 != 0 {
		writeContentLength()
	}
	if flags&8 != 0 {
		writeTransferEncoding()
	}
}

func assertTraceSegmentationEquivalent(t *testing.T, kind Kind, input, segmentation, context []byte) {
	t.Helper()
	limits := parserFuzzExpandedLimits()
	contiguous := runParserTraceFuzz(kind, input, nil, false, limits, context)
	randomSegments := runParserTraceFuzz(kind, input, segmentation, true, limits, context)
	byteAtATime := runParserTraceFuzz(kind, input, nil, true, limits, context)
	for name, candidate := range map[string]parserTraceFuzzResult{"random": randomSegments, "byte-at-a-time": byteAtATime} {
		if !contiguous.valid || !candidate.valid {
			t.Fatalf("%s parse violated span or consumption invariants", name)
		}
		if candidate.outcome != contiguous.outcome {
			t.Fatalf("%s outcome = %+v, contiguous = %+v", name, candidate.outcome, contiguous.outcome)
		}
		if !bytes.Equal(candidate.committedTrace, contiguous.committedTrace) || !bytes.Equal(candidate.committedBody, contiguous.committedBody) {
			t.Fatalf("%s committed callback output differs", name)
		}
		if contiguous.outcome.code == CodeNone || contiguous.outcome.code == CodeUpgrade {
			if !bytes.Equal(candidate.trace, contiguous.trace) || !bytes.Equal(candidate.body, contiguous.body) {
				t.Fatalf("%s successful callback output differs", name)
			}
		}
	}
}

type parserTraceRecorder struct {
	events, committedEvents []byte
	method, target, reason  []byte
	headerName, headerValue []byte
	pendingBody, body       []byte
	committedBody           []byte
	invalidSpan             bool
}

func (r *parserTraceRecorder) callbacks(context []byte) (*Callbacks, bool, bool) {
	static := len(context) != 0 && context[0]&0x80 != 0
	staticHead := len(context) != 0 && context[0]&1 != 0
	staticConnect := len(context) != 0 && context[0]&2 != 0
	callbacks := &Callbacks{
		MessageBegin: func() { r.event('B') },
		Method:       func(span []byte) { r.appendSpan(&r.method, span) },
		Target:       func(span []byte) { r.appendSpan(&r.target, span) },
		Reason:       func(span []byte) { r.appendSpan(&r.reason, span) },
		StartLine: func(message Message) {
			r.flushStartLine()
			r.messageEvent('S', message)
		},
		HeaderName:  func(span []byte) { r.appendSpan(&r.headerName, span) },
		HeaderValue: func(span []byte) { r.appendSpan(&r.headerValue, span) },
		HeaderEnd: func(trailer bool) {
			r.flushHeader(trailer)
		},
		HeadersComplete: func(message Message) {
			r.flushBody()
			r.messageEvent('H', message)
		},
		ChunkHeader: func(size uint64) {
			r.flushBody()
			r.uintEvent('C', size)
		},
		Body: func(span []byte) {
			r.appendSpan(&r.pendingBody, span)
			r.body = append(r.body, span...)
		},
		ChunkComplete: func() {
			r.flushBody()
			r.event('K')
		},
		MessageComplete: func(message Message) {
			r.flushBody()
			r.messageEvent('M', message)
			r.committedEvents = append(r.committedEvents, r.events...)
			r.events = r.events[:0]
			r.committedBody = append(r.committedBody, r.body...)
			r.body = r.body[:0]
		},
	}
	if !static {
		callbacks.ResponseContext = func(exchange uint64) (bool, bool) {
			value := byte(0)
			if len(context) != 0 {
				value = context[(int(exchange)+1)%len(context)]
			}
			head, connect := value&1 != 0, value&2 != 0
			r.event('R')
			r.appendUint(exchange)
			r.appendBool(head)
			r.appendBool(connect)
			return head, connect
		}
	}
	return callbacks, staticHead, staticConnect
}

func (r *parserTraceRecorder) appendSpan(destination *[]byte, span []byte) {
	if len(span) == 0 || cap(span) != len(span) {
		r.invalidSpan = true
	}
	*destination = append(*destination, span...)
}

func (r *parserTraceRecorder) event(tag byte) {
	r.events = append(r.events, tag)
}

func (r *parserTraceRecorder) uintEvent(tag byte, value uint64) {
	r.event(tag)
	r.appendUint(value)
}

func (r *parserTraceRecorder) appendUint(value uint64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	r.events = append(r.events, encoded[:]...)
}

func (r *parserTraceRecorder) appendBool(value bool) {
	if value {
		r.events = append(r.events, 1)
	} else {
		r.events = append(r.events, 0)
	}
}

func (r *parserTraceRecorder) blob(tag byte, value []byte) {
	r.event(tag)
	r.appendUint(uint64(len(value)))
	r.events = append(r.events, value...)
}

func (r *parserTraceRecorder) flushStartLine() {
	r.blob('m', r.method)
	r.blob('t', r.target)
	r.blob('r', r.reason)
	r.method = r.method[:0]
	r.target = r.target[:0]
	r.reason = r.reason[:0]
}

func (r *parserTraceRecorder) flushHeader(trailer bool) {
	r.event('h')
	r.appendBool(trailer)
	r.blob('n', r.headerName)
	r.blob('v', r.headerValue)
	r.headerName = r.headerName[:0]
	r.headerValue = r.headerValue[:0]
}

func (r *parserTraceRecorder) flushBody() {
	if len(r.pendingBody) == 0 {
		return
	}
	r.blob('b', r.pendingBody)
	r.pendingBody = r.pendingBody[:0]
}

func (r *parserTraceRecorder) messageEvent(tag byte, message Message) {
	r.event(tag)
	r.events = append(r.events, byte(message.Kind), byte(message.Method))
	r.appendUint(uint64(message.Status))
	r.appendUint(uint64(message.Major))
	r.appendUint(uint64(message.Minor))
	r.appendUint(message.ContentLength)
	r.appendBool(message.HasContentLength)
	r.appendBool(message.KeepAlive)
	r.appendUint(message.MessageNumber)
	r.appendUint(message.ExchangeNumber)
	r.appendBool(message.UpgradeRequested)
	r.appendBool(message.ConnectRequested)
}

func (r *parserTraceRecorder) signature() []byte {
	signature := append([]byte(nil), r.committedEvents...)
	signature = append(signature, r.events...)
	appendBlob := func(tag byte, value []byte) {
		signature = append(signature, tag)
		var encoded [8]byte
		binary.LittleEndian.PutUint64(encoded[:], uint64(len(value)))
		signature = append(signature, encoded[:]...)
		signature = append(signature, value...)
	}
	appendBlob('m', r.method)
	appendBlob('t', r.target)
	appendBlob('r', r.reason)
	appendBlob('n', r.headerName)
	appendBlob('v', r.headerValue)
	appendBlob('b', r.pendingBody)
	return signature
}

type parserTraceFuzzResult struct {
	outcome                       parserFuzzOutcome
	trace, body                   []byte
	committedTrace, committedBody []byte
	valid                         bool
}

func runParserTraceFuzz(kind Kind, input, segmentation []byte, segmented bool, limits Limits, context []byte) parserTraceFuzzResult {
	recorder := parserTraceRecorder{}
	callbacks, staticHead, staticConnect := recorder.callbacks(context)
	parser := NewParser(kind, callbacks, limits)
	if kind == Response && len(context) != 0 && context[0]&0x80 != 0 {
		parser.SetResponseContext(staticHead, staticConnect)
	}
	outcome, valid := driveParserFuzz(&parser, input, segmentation, segmented)
	if recorder.invalidSpan {
		valid = false
	}
	body := append([]byte(nil), recorder.committedBody...)
	body = append(body, recorder.body...)
	return parserTraceFuzzResult{
		outcome: outcome, trace: recorder.signature(), body: body,
		committedTrace: append([]byte(nil), recorder.committedEvents...),
		committedBody:  append([]byte(nil), recorder.committedBody...), valid: valid,
	}
}

type generatedParserCase struct {
	kind     Kind
	input    []byte
	context  []byte
	code     Code
	consumed int
	messages uint64
	body     []byte
}

func checkGeneratedParserCase(t *testing.T, test generatedParserCase, segmentation []byte) {
	t.Helper()
	limits := parserFuzzExpandedLimits()
	contiguous := runParserTraceFuzz(test.kind, test.input, nil, false, limits, test.context)
	if !contiguous.valid {
		t.Fatal("generated contiguous parse violated an invariant")
	}
	segmented := runParserTraceFuzz(test.kind, test.input, segmentation, true, limits, test.context)
	if !segmented.valid {
		t.Fatal("generated segmented parse violated an invariant")
	}
	if contiguous.outcome != segmented.outcome || !bytes.Equal(contiguous.trace, segmented.trace) || !bytes.Equal(contiguous.body, segmented.body) {
		t.Fatalf("generated parse differs across segmentation: contiguous=%+v segmented=%+v", contiguous.outcome, segmented.outcome)
	}
	if contiguous.outcome.code != test.code || contiguous.outcome.consumed != test.consumed || contiguous.outcome.messages != test.messages {
		t.Fatalf("generated parse = code %v consumed %d messages %d; want %v/%d/%d", contiguous.outcome.code, contiguous.outcome.consumed, contiguous.outcome.messages, test.code, test.consumed, test.messages)
	}
	if !bytes.Equal(contiguous.body, test.body) {
		t.Fatalf("generated body = %x, want %x", contiguous.body, test.body)
	}
}

type fuzzDataCursor struct {
	data   []byte
	offset int
}

func (c *fuzzDataCursor) next() byte {
	if len(c.data) == 0 {
		c.offset++
		return byte(c.offset * 37)
	}
	value := c.data[c.offset%len(c.data)]
	c.offset++
	return value
}

func (c *fuzzDataCursor) raw(max int) []byte {
	length := int(c.next()) % (max + 1)
	result := make([]byte, length)
	for index := range result {
		result[index] = c.next()
	}
	return result
}

func (c *fuzzDataCursor) token(max int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~"
	length := 1 + int(c.next())%max
	var result strings.Builder
	result.Grow(length)
	for range length {
		result.WriteByte(alphabet[int(c.next())%len(alphabet)])
	}
	return result.String()
}

func (c *fuzzDataCursor) path(max int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!$&'()*+,-./:;=?@_~"
	length := int(c.next()) % (max + 1)
	var result strings.Builder
	result.Grow(length + 1)
	result.WriteByte('/')
	for range length {
		if c.next()%11 == 0 {
			result.WriteByte('%')
			const hex = "0123456789ABCDEF"
			result.WriteByte(hex[c.next()%16])
			result.WriteByte(hex[c.next()%16])
			continue
		}
		result.WriteByte(alphabet[int(c.next())%len(alphabet)])
	}
	return result.String()
}

func (c *fuzzDataCursor) fieldValue(max int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ,;=\"\\/()[]?._-"
	length := int(c.next()) % (max + 1)
	var result strings.Builder
	result.Grow(length)
	for range length {
		if c.next()%17 == 0 {
			result.WriteByte('\t')
			continue
		}
		result.WriteByte(alphabet[int(c.next())%len(alphabet)])
	}
	return result.String()
}

func (c *fuzzDataCursor) mixedCase(value string) string {
	result := []byte(value)
	for index := range result {
		if result[index] >= 'a' && result[index] <= 'z' && c.next()&1 != 0 {
			result[index] -= 'a' - 'A'
		}
	}
	return string(result)
}

func buildGeneratedRequest(data []byte) generatedParserCase {
	cursor := fuzzDataCursor{data: data}
	messageCount := 1 + int(cursor.next())%3
	var wire strings.Builder
	var body []byte
	for messageIndex := 0; messageIndex < messageCount; messageIndex++ {
		for range int(cursor.next()) % 3 {
			wire.WriteString("\r\n")
		}
		methodVariant := cursor.next() % 7
		method := "GET"
		target := cursor.path(32)
		switch methodVariant {
		case 1:
			method = "POST"
		case 2:
			method = "PATCH"
		case 3:
			method = "OPTIONS"
			target = "*"
		case 4:
			method = "CONNECT"
			if cursor.next()&1 == 0 {
				target = "example.test:" + strconv.Itoa(1+int(cursor.next())*257)
			} else {
				target = "[2001:db8::1]:443"
			}
		case 5:
			method = cursor.token(12)
		case 6:
			target = "http://example.test" + cursor.path(24)
		}
		version11 := cursor.next()%4 != 0
		framing := cursor.next() % 3
		if !version11 && framing == 2 {
			framing = 1
		}
		wire.WriteString(method)
		wire.WriteByte(' ')
		wire.WriteString(target)
		if version11 {
			wire.WriteString(" HTTP/1.1\r\n")
			wire.WriteString(cursor.mixedCase("Host"))
			wire.WriteString(": \t")
			switch cursor.next() % 4 {
			case 0:
				wire.WriteString("example.test")
			case 1:
				wire.WriteString("example.test:8080")
			case 2:
				wire.WriteString("[2001:db8::1]:443")
			case 3:
				wire.WriteString("[fe80::1%25eth0]")
			}
			wire.WriteString(" \r\n")
		} else {
			wire.WriteString(" HTTP/1.0\r\n")
		}
		for range int(cursor.next()) % 4 {
			wire.WriteString("X-")
			wire.WriteString(cursor.token(10))
			wire.WriteString(": \t")
			wire.WriteString(cursor.fieldValue(24))
			wire.WriteString(" \t\r\n")
		}
		if cursor.next()%5 == 0 {
			wire.WriteString(cursor.mixedCase("Connection"))
			wire.WriteString(": keep-alive, Upgrade\r\n")
			wire.WriteString(cursor.mixedCase("Upgrade"))
			wire.WriteString(": websocket/13, h2c\r\n")
		}
		messageBody := cursor.raw(48)
		switch framing {
		case 0:
			messageBody = nil
			wire.WriteString("\r\n")
		case 1:
			wire.WriteString(cursor.mixedCase("Content-Length"))
			wire.WriteString(": \t")
			wire.WriteString(strconv.Itoa(len(messageBody)))
			wire.WriteString(" \t\r\n\r\n")
			wire.Write(messageBody)
		case 2:
			wire.WriteString(cursor.mixedCase("Transfer-Encoding"))
			wire.WriteString(": gzip; level=\"a\\b\", chunked\r\n")
			wire.WriteString("Trailer: X-End\r\n\r\n")
			remaining := messageBody
			for len(remaining) != 0 {
				size := 1 + int(cursor.next())%7
				if size > len(remaining) {
					size = len(remaining)
				}
				wire.WriteString(strconv.FormatInt(int64(size), 16))
				wire.WriteString(";flag;token=value;quoted=\"a\\b\"\r\n")
				wire.Write(remaining[:size])
				wire.WriteString("\r\n")
				remaining = remaining[size:]
			}
			wire.WriteString("0;done\r\nX-End: yes \t\r\n\r\n")
		}
		body = append(body, messageBody...)
	}
	input := []byte(wire.String())
	return generatedParserCase{kind: Request, input: input, code: CodeNone, consumed: len(input), messages: uint64(messageCount), body: body}
}

func buildGeneratedResponse(data []byte) generatedParserCase {
	cursor := fuzzDataCursor{data: data}
	body := cursor.raw(64)
	variant := cursor.next() % 10
	var wire strings.Builder
	result := generatedParserCase{kind: Response, code: CodeNone, messages: 1}
	switch variant {
	case 0:
		wire.WriteString("HTTP/1.1 200 OK\r\nContent-Length: ")
		wire.WriteString(strconv.Itoa(len(body)))
		wire.WriteString("\r\nX-Value: ")
		wire.WriteString(cursor.fieldValue(24))
		wire.WriteString("\r\n\r\n")
		wire.Write(body)
		result.body = body
	case 1:
		wire.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip; q=token, chunked\r\nTrailer: X-End\r\n\r\n")
		remaining := body
		for len(remaining) != 0 {
			size := 1 + int(cursor.next())%9
			if size > len(remaining) {
				size = len(remaining)
			}
			wire.WriteString(strconv.FormatInt(int64(size), 16))
			wire.WriteString(";x=\"y\\z\"\r\n")
			wire.Write(remaining[:size])
			wire.WriteString("\r\n")
			remaining = remaining[size:]
		}
		wire.WriteString("0\r\nX-End: yes\r\n\r\n")
		result.body = body
	case 2:
		wire.WriteString("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
		wire.Write(body)
		result.body = body
	case 3:
		wire.WriteString("HTTP/1.1 204 No Content\r\nX-Empty: yes\r\n\r\n")
	case 4:
		wire.WriteString("HTTP/1.1 200 OK\r\nContent-Length: ")
		wire.WriteString(strconv.Itoa(len(body)))
		wire.WriteString("\r\n\r\n")
		result.context = []byte{0x81}
	case 5:
		wire.WriteString("HTTP/1.1 200 Connection Established\r\nProxy-Agent: fuzz\r\n\r\n")
		result.context = []byte{0x82}
		result.code = CodeUpgrade
		result.messages = 1
	case 6:
		wire.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket/13\r\n\r\n")
		result.context = []byte{0x80}
		result.code = CodeUpgrade
	case 7:
		wire.WriteString("HTTP/1.1 103 Early Hints\r\nLink: </asset>; rel=preload\r\n\r\n")
		wire.WriteString("HTTP/1.1 200 OK\r\nContent-Length: ")
		wire.WriteString(strconv.Itoa(len(body)))
		wire.WriteString("\r\n\r\n")
		wire.Write(body)
		result.context = []byte{0x80}
		result.messages = 2
		result.body = body
	case 8:
		wire.WriteString("HTTP/1.1 103 Early Hints\r\nX-Hint: yes\r\n\r\n")
		wire.WriteString("HTTP/1.1 200 OK\r\nContent-Length: ")
		wire.WriteString(strconv.Itoa(len(body)))
		wire.WriteString("\r\n\r\n")
		result.context = []byte{0x81}
		result.messages = 2
	case 9:
		wire.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip; q=\"x\"\r\n\r\n")
		wire.Write(body)
		result.body = body
	}
	if result.code == CodeUpgrade {
		result.consumed = wire.Len()
		wire.WriteString("opaque protocol bytes")
	} else {
		result.consumed = wire.Len()
	}
	result.input = []byte(wire.String())
	return result
}
