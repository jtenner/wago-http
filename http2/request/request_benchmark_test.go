package request

import (
	"bytes"
	"io"
	"testing"

	h2 "github.com/wago-org/http/http2"
	"golang.org/x/net/http2/hpack"
)

func BenchmarkAppendRequest(b *testing.B) {
	cases := []struct {
		name    string
		request Request
	}{
		{"get", basicRequest()},
		{"headers-32", benchmarkRequest(32, 0)},
		{"header-16k", Request{Method: []byte("GET"), Scheme: []byte("https"), Authority: []byte("example.test"), Path: []byte("/"), Headers: []Header{{Name: []byte("x-large"), Value: bytes.Repeat([]byte("x"), 16<<10)}}}},
		{"body-16k", benchmarkRequest(4, 16<<10)},
		{"body-64k", benchmarkRequest(4, 65535)},
	}
	client := Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 64 << 10, MaxHeaderListBytes: 1 << 20}}
	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			wire, err := client.Append(nil, test.request)
			if err != nil {
				b.Fatal(err)
			}
			dst := make([]byte, 0, len(wire))
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = dst[:0]
				if _, err := client.Append(dst, test.request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkValidateRequest(b *testing.B) {
	cases := []struct {
		name    string
		request Request
		valid   bool
	}{
		{"get", basicRequest(), true},
		{"headers-32", benchmarkRequest(32, 0), true},
		{"body-64k", benchmarkRequest(4, 65535), true},
		{"invalid-method-early", Request{}, false},
		{"invalid-header-late", benchmarkInvalidLateRequest(), false},
	}
	client := Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 64 << 10, MaxHeaderListBytes: 1 << 20}}
	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				err := client.Validate(test.request)
				if (err == nil) != test.valid {
					b.Fatalf("valid=%t err=%v", test.valid, err)
				}
			}
		})
	}
}

func BenchmarkEncodeRequest(b *testing.B) {
	for _, test := range []struct {
		name string
		req  Request
	}{
		{"get", basicRequest()},
		{"headers-32", benchmarkRequest(32, 0)},
		{"body-64k", benchmarkRequest(4, 65535)},
	} {
		client := Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 64 << 10, MaxHeaderListBytes: 1 << 20}}
		wire, err := client.Append(nil, test.req)
		if err != nil {
			b.Fatal(err)
		}
		dst := make([]byte, len(wire))
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for i := 0; i < b.N; i++ {
				if _, err := client.Encode(dst, test.req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWriteRequest(b *testing.B) {
	for _, test := range []struct {
		name string
		req  Request
	}{
		{"get", basicRequest()},
		{"headers-32", benchmarkRequest(32, 0)},
		{"body-64k", benchmarkRequest(4, 65535)},
	} {
		client := Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 64 << 10, MaxHeaderListBytes: 1 << 20}}
		wire, err := client.Append(nil, test.req)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for i := 0; i < b.N; i++ {
				if err := client.Write(io.Discard, test.req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkClientStreamingResponse(b *testing.B) {
	cases := []struct {
		bodySize  int
		readBytes int
		callback  bool
	}{
		{0, 16 << 10, false}, {0, 1, false},
		{1024, 16 << 10, false}, {1024, 64, false}, {1024, 1, false}, {1024, 64, true},
		{65535, 16 << 10, false}, {65535, 1024, false}, {65535, 64, false}, {65535, 1024, true},
		{1 << 20, 16 << 10, false}, {1 << 20, 1024, false}, {1 << 20, 16 << 10, true},
	}
	for _, test := range cases {
		name := "body-" + benchmarkDecimal(test.bodySize) + "/read-" + benchmarkDecimal(test.readBytes)
		if test.callback {
			name += "/callback"
		}
		b.Run(name, func(b *testing.B) {
			body := bytes.Repeat([]byte("x"), test.bodySize)
			fields := []hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: benchmarkDecimal(test.bodySize)}}
			frames := []serverFrame{{h2.FrameHeader{Type: h2.FrameSettings}, nil}, {h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(b, fields...)}}
			remaining := body
			for len(remaining) != 0 {
				count := len(remaining)
				if count > 16384 {
					count = 16384
				}
				flags := h2.Flags(0)
				if count == len(remaining) {
					flags = h2.FlagEndStream
				}
				frames = append(frames, serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: flags, StreamID: 1}, remaining[:count]})
				remaining = remaining[count:]
			}
			if test.bodySize == 0 {
				frames[1].header.Flags |= h2.FlagEndStream
			}
			wire := serverWire(b, frames...)
			buffer := make([]byte, test.readBytes)
			client := Client{MaxResponseBodyBytes: uint64(test.bodySize) + 1}
			var callbacks *Callbacks
			var sink int
			if test.callback {
				callbacks = &Callbacks{Body: func(fragment []byte) { sink += len(fragment) }}
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stream := benchmarkStream{reader: bytes.NewReader(wire)}
				if _, err := client.DoBuffer(&stream, basicRequest(), callbacks, buffer); err != nil {
					b.Fatal(err)
				}
			}
			_ = sink
		})
	}
}

func BenchmarkClientResponseSequences(b *testing.B) {
	cases := []struct {
		name string
		wire []byte
		ok   bool
	}{
		{"informational-trailers", serverWire(b,
			serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
			serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(b, hpack.HeaderField{Name: ":status", Value: "103"})},
			serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(b, hpack.HeaderField{Name: ":status", Value: "200"})},
			serverFrame{h2.FrameHeader{Type: h2.FrameData, StreamID: 1}, []byte("body")},
			serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders | h2.FlagEndStream, StreamID: 1}, headerBlock(b, hpack.HeaderField{Name: "x-trailer", Value: "done"})},
		), true},
		{"malformed-early", rawServerFrame(h2.FrameHeader{Type: h2.FramePing}, []byte("short")), false},
		{"malformed-late", serverWire(b,
			serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
			serverFrame{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(b, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "content-length", Value: "2"})},
			serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: h2.FlagEndStream, StreamID: 1}, []byte("x")},
		), false},
	}
	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			buffer := make([]byte, len(test.wire))
			b.ReportAllocs()
			b.SetBytes(int64(len(test.wire)))
			for i := 0; i < b.N; i++ {
				stream := benchmarkStream{reader: bytes.NewReader(test.wire)}
				_, err := (Client{}).DoBuffer(&stream, basicRequest(), nil, buffer)
				if (err == nil) != test.ok {
					b.Fatalf("ok=%t err=%v", test.ok, err)
				}
			}
		})
	}
}

func benchmarkInvalidLateRequest() Request {
	request := benchmarkRequest(31, 0)
	request.Headers = append(request.Headers, Header{Name: []byte("Connection"), Value: []byte("close")})
	return request
}

func benchmarkRequest(headers, body int) Request {
	request := Request{Method: []byte("POST"), Scheme: []byte("https"), Authority: []byte("example.test"), Path: []byte("/benchmark"), Body: bytes.Repeat([]byte("b"), body)}
	request.Headers = make([]Header, headers)
	for i := range request.Headers {
		request.Headers[i] = Header{Name: []byte("x-header-" + benchmarkDecimal(i)), Value: []byte("benchmark-value")}
	}
	return request
}

func benchmarkDecimal(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	i := len(buffer)
	for value != 0 {
		i--
		buffer[i] = '0' + byte(value%10)
		value /= 10
	}
	return string(buffer[i:])
}

type benchmarkStream struct {
	reader *bytes.Reader
	writes int
}

func (stream *benchmarkStream) Read(dst []byte) (int, error) { return stream.reader.Read(dst) }
func (stream *benchmarkStream) Write(src []byte) (int, error) {
	stream.writes += len(src)
	return len(src), nil
}

var _ io.ReadWriter = (*benchmarkStream)(nil)
