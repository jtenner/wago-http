package request

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	h2 "github.com/wago-org/http/http2"
	"golang.org/x/net/http2/hpack"
)

func TestValidateRequestCorpus(t *testing.T) {
	valid := Request{Method: []byte("GET"), Scheme: []byte("https"), Authority: []byte("example.test"), Path: []byte("/")}
	tests := []struct {
		name string
		req  Request
		err  error
	}{
		{"valid", valid, nil},
		{"valid-extension-method", Request{Method: []byte("M-SEARCH"), Scheme: valid.Scheme, Authority: valid.Authority, Path: []byte("*")}, nil},
		{"valid-te-trailers", Request{Method: valid.Method, Scheme: valid.Scheme, Authority: valid.Authority, Path: valid.Path, Headers: []Header{{Name: []byte("te"), Value: []byte("trailers")}}}, nil},
		{"missing-method", Request{Scheme: valid.Scheme, Authority: valid.Authority, Path: valid.Path}, ErrInvalidRequest},
		{"bad-method-space", Request{Method: []byte("GE T"), Scheme: valid.Scheme, Authority: valid.Authority, Path: valid.Path}, ErrInvalidRequest},
		{"missing-scheme", Request{Method: valid.Method, Authority: valid.Authority, Path: valid.Path}, ErrInvalidRequest},
		{"bad-scheme", Request{Method: valid.Method, Scheme: []byte("1http"), Authority: valid.Authority, Path: valid.Path}, ErrInvalidRequest},
		{"missing-authority", Request{Method: valid.Method, Scheme: valid.Scheme, Path: valid.Path}, ErrInvalidRequest},
		{"authority-injection", Request{Method: valid.Method, Scheme: valid.Scheme, Authority: []byte("x\r\ny"), Path: valid.Path}, ErrInvalidRequest},
		{"missing-path", Request{Method: valid.Method, Scheme: valid.Scheme, Authority: valid.Authority}, ErrInvalidRequest},
		{"path-space", Request{Method: valid.Method, Scheme: valid.Scheme, Authority: valid.Authority, Path: []byte("/bad path")}, ErrInvalidRequest},
		{"uppercase-header", withHeader(valid, "X-Test", "value"), ErrInvalidRequest},
		{"empty-header", withHeader(valid, "", "value"), ErrInvalidRequest},
		{"pseudo-header", withHeader(valid, ":status", "200"), ErrInvalidRequest},
		{"header-injection", withHeader(valid, "x-test", "ok\r\nbad"), ErrInvalidRequest},
		{"header-control", withHeader(valid, "x-test", "ok\x01bad"), ErrInvalidRequest},
		{"header-leading-space", withHeader(valid, "x-test", " value"), ErrInvalidRequest},
		{"authority-tab", Request{Method: valid.Method, Scheme: valid.Scheme, Authority: []byte("bad\thost"), Path: valid.Path}, ErrInvalidRequest},
		{"host", withHeader(valid, "host", "other.test"), ErrInvalidRequest},
		{"content-length", withHeader(valid, "content-length", "0"), ErrInvalidRequest},
		{"connection", withHeader(valid, "connection", "close"), ErrInvalidRequest},
		{"proxy-connection", withHeader(valid, "proxy-connection", "close"), ErrInvalidRequest},
		{"keep-alive", withHeader(valid, "keep-alive", "timeout=1"), ErrInvalidRequest},
		{"transfer-encoding", withHeader(valid, "transfer-encoding", "chunked"), ErrInvalidRequest},
		{"upgrade", withHeader(valid, "upgrade", "websocket"), ErrInvalidRequest},
		{"bad-te", withHeader(valid, "te", "gzip"), ErrInvalidRequest},
		{"body-window", Request{Method: []byte("POST"), Scheme: valid.Scheme, Authority: valid.Authority, Path: valid.Path, Body: make([]byte, 65536)}, ErrRequestBodyTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (Client{}).Validate(test.req)
			if test.err == nil && err != nil {
				t.Fatal(err)
			}
			if test.err != nil && !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
		})
	}
}

func TestRequestHeaderLimits(t *testing.T) {
	req := basicRequest()
	req.Headers = []Header{{Name: []byte("x-test"), Value: []byte("value")}}
	if err := (Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 4}}).Validate(req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("field limit = %v", err)
	}
	if err := (Client{HeaderLimits: h2.HeaderLimits{MaxHeaders: 4}}).Validate(req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("count limit = %v", err)
	}
	if err := (Client{HeaderLimits: h2.HeaderLimits{MaxHeaderListBytes: 1}}).Validate(req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("list limit = %v", err)
	}
	large := basicRequest()
	large.Headers = []Header{{Name: []byte("x-large"), Value: bytes.Repeat([]byte("x"), 20<<10)}}
	if err := (Client{}).Validate(large); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("default field limit = %v", err)
	}
}

func TestAppendRequestWireAndFragmentation(t *testing.T) {
	req := Request{
		Method: []byte("POST"), Scheme: []byte("https"), Authority: []byte("example.test"), Path: []byte("/items?q=1"),
		Headers: []Header{{Name: []byte("content-type"), Value: []byte("application/octet-stream")}, {Name: []byte("authorization"), Value: []byte("secret"), Sensitive: true}},
		Body:    bytes.Repeat([]byte("b"), 40000),
	}
	wire, err := (Client{}).Append([]byte("prefix"), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire[:6]) != "prefix" || string(wire[6:6+len(h2.ClientPreface)]) != h2.ClientPreface {
		t.Fatal("missing prefix or client preface")
	}

	var fields []h2.HeaderField
	decoder := h2.NewHeaderDecoder(h2.HeaderLimits{}, func(field h2.HeaderField) { fields = append(fields, field) })
	var body []byte
	var frameHeaders []h2.FrameHeader
	callbacks := h2.Callbacks{
		FrameBegin: func(header h2.FrameHeader) {
			frameHeaders = append(frameHeaders, header)
			if header.Type == h2.FrameHeaders {
				if err := decoder.BeginBlock(); err != nil {
					t.Fatal(err)
				}
			}
		},
		HeaderBlock: func(_ uint32, fragment []byte) {
			if _, err := decoder.Write(fragment); err != nil {
				t.Fatal(err)
			}
		},
		HeaderBlockEnd: func(_ uint32, _ bool) {
			if err := decoder.EndBlock(); err != nil {
				t.Fatal(err)
			}
		},
		Data: func(_ uint32, fragment []byte, _ bool) { body = append(body, fragment...) },
	}
	parser := h2.NewParser(&callbacks, h2.Limits{})
	payload := wire[6+len(h2.ClientPreface):]
	for offset := 0; offset < len(payload); {
		size := 1 + offset%37
		if size > len(payload)-offset {
			size = len(payload) - offset
		}
		n, code := parser.Parse(payload[offset : offset+size])
		if code != h2.CodeNone || n != size {
			t.Fatalf("Parse = %d/%d, %v", n, size, code)
		}
		offset += size
	}
	if code := parser.Finish(); code != h2.CodeNone {
		t.Fatal(code)
	}
	wantFields := []h2.HeaderField{
		{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/items?q=1"},
		{Name: "content-type", Value: "application/octet-stream"}, {Name: "authorization", Value: "secret", Sensitive: true}, {Name: "content-length", Value: "40000"},
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("fields:\n got %#v\nwant %#v", fields, wantFields)
	}
	if !bytes.Equal(body, req.Body) {
		t.Fatalf("body length = %d", len(body))
	}
	if len(frameHeaders) != 5 || frameHeaders[0].Type != h2.FrameSettings || frameHeaders[1].Type != h2.FrameHeaders {
		t.Fatalf("frame sequence = %#v", frameHeaders)
	}
	for i, header := range frameHeaders {
		if header.Length > 16<<10 {
			t.Fatalf("frame %d length = %d", i, header.Length)
		}
	}
	if !frameHeaders[len(frameHeaders)-1].Flags.Has(h2.FlagEndStream) {
		t.Fatal("final DATA lacks END_STREAM")
	}
}

func TestAppendLargeHeaderUsesContinuation(t *testing.T) {
	req := Request{Method: []byte("GET"), Scheme: []byte("https"), Authority: []byte("example.test"), Path: []byte("/"), Headers: []Header{{Name: []byte("x-large"), Value: bytes.Repeat([]byte("z"), 40000)}}}
	wire, err := (Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 65535, MaxHeaderListBytes: 1 << 20}}).Append(nil, req)
	if err != nil {
		t.Fatal(err)
	}
	var headers []h2.FrameHeader
	parser := h2.NewParser(&h2.Callbacks{FrameBegin: func(header h2.FrameHeader) { headers = append(headers, header) }}, h2.Limits{})
	if _, code := parser.Parse(wire[len(h2.ClientPreface):]); code != h2.CodeNone {
		t.Fatal(code)
	}
	if len(headers) < 4 || headers[1].Type != h2.FrameHeaders || headers[2].Type != h2.FrameContinuation {
		t.Fatalf("headers = %#v", headers)
	}
	if !headers[1].Flags.Has(h2.FlagEndStream) || headers[1].Flags.Has(h2.FlagEndHeaders) {
		t.Fatalf("first HEADERS flags = %x", headers[1].Flags)
	}
	if !headers[len(headers)-1].Flags.Has(h2.FlagEndHeaders) {
		t.Fatal("final CONTINUATION lacks END_HEADERS")
	}
}

func TestClientResponseEverySplitAndControlFrames(t *testing.T) {
	responseWire := serverWire(t,
		serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, setting(h2.SettingMaxConcurrentStreams, 100)},
		serverFrame{h2.FrameHeader{Type: h2.FramePing}, []byte("12345678")},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "content-length", Value: "4"})},
		serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: h2.FlagEndStream, StreamID: 1}, []byte("body")},
	)
	req := basicRequest()
	for split := 1; split < len(responseWire); split++ {
		stream := &scriptedStream{reads: [][]byte{responseWire[:split], responseWire[split:]}}
		var body []byte
		response, err := (Client{}).DoBuffer(stream, req, &Callbacks{Body: func(fragment []byte) { body = append(body, fragment...) }}, make([]byte, len(responseWire)+16))
		if err != nil {
			t.Fatalf("split %d: %v", split, err)
		}
		if response.Status != 200 || response.ContentLength != 4 || response.BodyBytes != 4 || string(body) != "body" {
			t.Fatalf("split %d response=%+v body=%q", split, response, body)
		}
		requestWire, err := (Client{}).Append(nil, req)
		if err != nil {
			t.Fatal(err)
		}
		control := stream.writes.Bytes()[len(requestWire):]
		got := controlFrameTypes(t, control)
		want := []h2.FrameType{h2.FrameSettings, h2.FramePing, h2.FrameWindowUpdate, h2.FrameWindowUpdate}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d controls=%v want=%v", split, got, want)
		}
	}
}

func TestClientInformationalFinalTrailersAndBuffered(t *testing.T) {
	wire := serverWire(t,
		serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "103"}, hpack.HeaderField{Name: "link", Value: "</a>"})},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"})[:1]},
		serverFrame{h2.FrameHeader{Type: h2.FrameContinuation, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"})[1:]},
		serverFrame{h2.FrameHeader{Type: h2.FrameData, StreamID: 1}, []byte("hello")},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders | h2.FlagEndStream, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: "x-checksum", Value: "ok"})},
	)
	wire = append(wire, []byte("TAIL")...)
	stream := &scriptedStream{reads: [][]byte{wire}}
	var events []string
	var body []byte
	response, err := (Client{}).DoBuffer(stream, basicRequest(), &Callbacks{
		Header: func(field h2.HeaderField, trailer bool) {
			events = append(events, field.Name+"="+field.Value+":"+strconvBool(trailer))
		},
		HeadersComplete: func(response Response, informational bool) {
			events = append(events, "headers:"+strconvBool(informational))
		},
		Body:             func(fragment []byte) { body = append(body, fragment...) },
		TrailersComplete: func() { events = append(events, "trailers") },
		ResponseComplete: func(Response) { events = append(events, "complete") },
	}, make([]byte, len(wire)))
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || response.BodyBytes != 5 || string(response.Buffered) != "TAIL" || string(body) != "hello" {
		t.Fatalf("response=%+v body=%q", response, body)
	}
	if !containsString(events, "headers:true") || !containsString(events, "headers:false") || !containsString(events, "trailers") || events[len(events)-1] != "complete" {
		t.Fatalf("events = %v", events)
	}
}

func TestClientLargeStreamingBodyFlowControl(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 200000)
	frames := []serverFrame{{h2.FrameHeader{Type: h2.FrameSettings}, nil}, {h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "content-length", Value: "200000"})}}
	for len(body) != 0 {
		count := len(body)
		if count > 16384 {
			count = 16384
		}
		flags := h2.Flags(0)
		if count == len(body) {
			flags = h2.FlagEndStream
		}
		frames = append(frames, serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: flags, StreamID: 1}, append([]byte(nil), body[:count]...)})
		body = body[count:]
	}
	wire := serverWire(t, frames...)
	stream := &scriptedStream{reads: splitBytes(wire, 997)}
	var received uint64
	response, err := (Client{MaxResponseBodyBytes: 200000}).DoBuffer(stream, basicRequest(), &Callbacks{Body: func(fragment []byte) { received += uint64(len(fragment)) }}, make([]byte, 1024))
	if err != nil {
		t.Fatal(err)
	}
	if response.BodyBytes != 200000 || received != 200000 {
		t.Fatalf("response=%+v received=%d", response, received)
	}
}

func TestClientRejectsMalformedResponseCorpus(t *testing.T) {
	validHeaders := headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"})
	cases := []struct {
		name string
		wire []byte
		want error
	}{
		{"missing-settings", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders | h2.FlagEndStream, StreamID: 1}, validHeaders}), ErrMissingSettings},
		{"settings-ack-first", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings, Flags: h2.FlagACK}, nil}), ErrMissingSettings},
		{"bad-frame", rawServerFrame(h2.FrameHeader{Type: h2.FramePing}, []byte("short")), ErrInvalidResponse},
		{"invalid-hpack", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, []byte{0xff}}), ErrInvalidResponse},
		{"missing-status", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: "x", Value: "y"})}), ErrInvalidResponse},
		{"duplicate-status", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: ":status", Value: "201"}), ErrInvalidResponse},
		{"pseudo-after-regular", semanticResponse(t, hpack.HeaderField{Name: "x", Value: "y"}, hpack.HeaderField{Name: ":status", Value: "200"}), ErrInvalidResponse},
		{"uppercase", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "X-Test", Value: "y"}), ErrInvalidResponse},
		{"connection", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "connection", Value: "close"}), ErrInvalidResponse},
		{"bad-status", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "99"}), ErrInvalidResponse},
		{"switching-protocols", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "101"}), ErrInvalidResponse},
		{"invalid-name-token", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "bad(name", Value: "x"}), ErrInvalidResponse},
		{"leading-value-space", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x", Value: " value"}), ErrInvalidResponse},
		{"204-content-length", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "204"}, hpack.HeaderField{Name: "content-length", Value: "0"}), ErrInvalidResponse},
		{"duplicate-content-length", semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "content-length", Value: "0"}, hpack.HeaderField{Name: "content-length", Value: "0"}), ErrInvalidResponse},
		{"content-length-mismatch", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "content-length", Value: "2"})}, serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: h2.FlagEndStream, StreamID: 1}, []byte("x")}), ErrInvalidResponse},
		{"204-data", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "204"})}, serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: h2.FlagEndStream, StreamID: 1}, nil}), ErrInvalidResponse},
		{"data-before-headers", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameData, StreamID: 1}, []byte("x")}), ErrInvalidResponse},
		{"headers-other-stream", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 3}, validHeaders}), ErrInvalidResponse},
		{"rst", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameRSTStream, StreamID: 1}, uint32Bytes(uint32(h2.ErrCodeCancel))}), ErrStreamReset},
		{"goaway", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FrameGoAway}, append(uint32Bytes(0), uint32Bytes(uint32(h2.ErrCodeNo))...)}), ErrGoAway},
		{"push", serverWire(t, serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil}, serverFrame{h2.FrameHeader{Type: h2.FramePushPromise, Flags: h2.FlagEndHeaders, StreamID: 1}, append(uint32Bytes(2), 0x82)}), ErrPushPromise},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			stream := &scriptedStream{reads: splitBytes(test.wire, 3)}
			_, err := (Client{}).DoBuffer(stream, basicRequest(), nil, make([]byte, 7))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %T %v, want %v", err, err, test.want)
			}
		})
	}
}

func TestClientHEADRejectsDATA(t *testing.T) {
	wire := serverWire(t,
		serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"})},
		serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: h2.FlagEndStream, StreamID: 1}, nil},
	)
	request := basicRequest()
	request.Method = []byte("HEAD")
	_, err := (Client{}).DoBuffer(&scriptedStream{reads: [][]byte{wire}}, request, nil, make([]byte, len(wire)))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("HEAD DATA error = %v", err)
	}
}

func TestClientIOLimitsAndShortWrites(t *testing.T) {
	if _, err := (Client{}).DoBuffer(nil, basicRequest(), nil, make([]byte, 1)); !errors.Is(err, ErrInvalidStream) {
		t.Fatal(err)
	}
	if _, err := (Client{}).DoBuffer(&scriptedStream{}, basicRequest(), nil, nil); !errors.Is(err, ErrEmptyReadBuffer) {
		t.Fatal(err)
	}

	writer := &shortWriter{maximum: 2}
	if err := (Client{}).Write(writer, basicRequest()); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(writer.Bytes(), []byte(h2.ClientPreface)) {
		t.Fatal("short writer lost preface")
	}

	wire := serverWire(t,
		serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, hpack.HeaderField{Name: ":status", Value: "200"})},
		serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: h2.FlagEndStream, StreamID: 1}, []byte("12345")},
	)
	stream := &scriptedStream{reads: [][]byte{wire}}
	_, err := (Client{MaxResponseBodyBytes: 4}).DoBuffer(stream, basicRequest(), nil, make([]byte, len(wire)))
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("body limit error = %v", err)
	}

	noProgress := &zeroStream{}
	if _, err := (Client{}).DoBuffer(noProgress, basicRequest(), nil, make([]byte, 8)); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("no progress = %v", err)
	}
}

func TestErrorFormattingAndAllocatingDo(t *testing.T) {
	if got := (&ValidationError{}).Error(); got != ErrInvalidRequest.Error() {
		t.Fatalf("ValidationError = %q", got)
	}
	if got := (&FrameError{Code: h2.CodeInvalidPadding}).Error(); !strings.Contains(got, "invalid padding") {
		t.Fatalf("FrameError = %q", got)
	}
	if got := (&CompressionError{Err: io.ErrUnexpectedEOF}).Error(); !strings.Contains(got, "HPACK") {
		t.Fatalf("CompressionError = %q", got)
	}

	wire := semanticResponse(t, hpack.HeaderField{Name: ":status", Value: "204"})
	response, err := (Client{ReadBufferBytes: 1}).Do(&scriptedStream{reads: splitBytes(wire, 1)}, basicRequest(), nil)
	if err != nil || response.Status != 204 {
		t.Fatalf("Do response=%+v error=%v", response, err)
	}
}

func TestEncodeShortBufferUnmodified(t *testing.T) {
	dst := bytes.Repeat([]byte{0xaa}, 8)
	before := append([]byte(nil), dst...)
	if _, err := (Client{}).Encode(dst, basicRequest()); !errors.Is(err, ErrShortBuffer) {
		t.Fatal(err)
	}
	if !bytes.Equal(dst, before) {
		t.Fatal("short destination modified")
	}
}

type serverFrame struct {
	header  h2.FrameHeader
	payload []byte
}

func serverWire(t testing.TB, frames ...serverFrame) []byte {
	t.Helper()
	var wire []byte
	for _, frame := range frames {
		var code h2.Code
		wire, code = h2.AppendFrame(wire, frame.header, frame.payload)
		if code != h2.CodeNone {
			t.Fatalf("AppendFrame: %v", code)
		}
	}
	return wire
}

func rawServerFrame(header h2.FrameHeader, payload []byte) []byte {
	wire := make([]byte, 9+len(payload))
	length := len(payload)
	wire[0], wire[1], wire[2] = byte(length>>16), byte(length>>8), byte(length)
	wire[3], wire[4] = byte(header.Type), byte(header.Flags)
	binary.BigEndian.PutUint32(wire[5:9], header.StreamID)
	copy(wire[9:], payload)
	return wire
}

func headerBlock(t testing.TB, fields ...hpack.HeaderField) []byte {
	t.Helper()
	var buffer bytes.Buffer
	encoder := hpack.NewEncoder(&buffer)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
}

func semanticResponse(t testing.TB, fields ...hpack.HeaderField) []byte {
	return serverWire(t,
		serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
		serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders | h2.FlagEndStream, StreamID: 1}, headerBlock(t, fields...)},
	)
}

func setting(id h2.SettingID, value uint32) []byte {
	payload := make([]byte, 6)
	binary.BigEndian.PutUint16(payload[:2], uint16(id))
	binary.BigEndian.PutUint32(payload[2:], value)
	return payload
}

func uint32Bytes(value uint32) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, value)
	return payload
}
func basicRequest() Request {
	return Request{Method: []byte("GET"), Scheme: []byte("https"), Authority: []byte("example.test"), Path: []byte("/")}
}
func withHeader(request Request, name, value string) Request {
	request.Headers = []Header{{Name: []byte(name), Value: []byte(value)}}
	return request
}
func splitBytes(data []byte, size int) [][]byte {
	var result [][]byte
	for len(data) != 0 {
		n := size
		if n > len(data) {
			n = len(data)
		}
		result = append(result, data[:n])
		data = data[n:]
	}
	return result
}
func strconvBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func controlFrameTypes(t *testing.T, wire []byte) []h2.FrameType {
	t.Helper()
	var types []h2.FrameType
	parser := h2.NewParser(&h2.Callbacks{FrameBegin: func(header h2.FrameHeader) { types = append(types, header.Type) }}, h2.Limits{})
	if n, code := parser.Parse(wire); n != len(wire) || code != h2.CodeNone {
		t.Fatalf("control parse = %d/%d, %v", n, len(wire), code)
	}
	return types
}

type scriptedStream struct {
	writes                bytes.Buffer
	reads                 [][]byte
	readIndex, readOffset int
}

func (stream *scriptedStream) Write(data []byte) (int, error) { return stream.writes.Write(data) }
func (stream *scriptedStream) Read(dst []byte) (int, error) {
	for stream.readIndex < len(stream.reads) && len(stream.reads[stream.readIndex]) == 0 {
		stream.readIndex++
	}
	if stream.readIndex >= len(stream.reads) {
		return 0, io.EOF
	}
	current := stream.reads[stream.readIndex]
	n := copy(dst, current[stream.readOffset:])
	stream.readOffset += n
	if stream.readOffset == len(current) {
		stream.readIndex++
		stream.readOffset = 0
	}
	return n, nil
}

type shortWriter struct {
	bytes.Buffer
	maximum int
}

func (writer *shortWriter) Write(data []byte) (int, error) {
	if len(data) > writer.maximum {
		data = data[:writer.maximum]
	}
	return writer.Buffer.Write(data)
}

type zeroStream struct{ bytes.Buffer }

func (*zeroStream) Read([]byte) (int, error) { return 0, nil }
