package request

import (
	"bytes"
	"io"
	"testing"

	h2 "github.com/wago-org/http/http2"
	"golang.org/x/net/http2/hpack"
)

func FuzzValidateAndAppendRequest(f *testing.F) {
	f.Add("GET", "https", "example.test", "/", "x-test", "value", []byte("body"))
	f.Add("", "1", "\r\n", " bad", "Connection", "close", []byte{})
	f.Fuzz(func(t *testing.T, method, scheme, authority, path, name, value string, body []byte) {
		if len(method)+len(scheme)+len(authority)+len(path)+len(name)+len(value)+len(body) > 1<<20 {
			t.Skip()
		}
		request := Request{
			Method: []byte(method), Scheme: []byte(scheme), Authority: []byte(authority), Path: []byte(path),
			Headers: []Header{{Name: []byte(name), Value: []byte(value)}}, Body: body,
		}
		client := Client{MaxRequestBodyBytes: 65535}
		validationErr := client.Validate(request)
		wire, appendErr := client.Append(nil, request)
		if validationErr != nil {
			if appendErr == nil || len(wire) != 0 {
				t.Fatalf("Validate=%v Append=(%d,%v)", validationErr, len(wire), appendErr)
			}
			return
		}
		if appendErr != nil {
			t.Fatalf("valid request append: %v", appendErr)
		}
		if !bytes.HasPrefix(wire, []byte(h2.ClientPreface)) {
			t.Fatal("wire lacks client preface")
		}
		parser := h2.NewParser(nil, h2.Limits{})
		frames := wire[len(h2.ClientPreface):]
		if n, code := parser.Parse(frames); n != len(frames) || code != h2.CodeNone {
			t.Fatalf("self-generated frames rejected: %d/%d, %v", n, len(frames), code)
		}
	})
}

func FuzzClientValidStreamingResponse(f *testing.F) {
	f.Add(uint16(200), []byte("response body"), uint8(1), true)
	f.Add(uint16(204), []byte{}, uint8(17), false)
	f.Fuzz(func(t *testing.T, status uint16, body []byte, stride uint8, contentLength bool) {
		if status < 200 || status > 599 || len(body) > 1<<20 || (status == 204 || status == 304) && len(body) != 0 || status == 204 && contentLength {
			t.Skip()
		}
		fields := []hpack.HeaderField{{Name: ":status", Value: statusString(status)}}
		if contentLength {
			fields = append(fields, hpack.HeaderField{Name: "content-length", Value: decimalString(uint64(len(body)))})
		}
		frames := []serverFrame{
			{h2.FrameHeader{Type: h2.FrameSettings}, nil},
			{h2.FrameHeader{Type: h2.FrameHeaders, Flags: h2.FlagEndHeaders, StreamID: 1}, headerBlock(t, fields...)},
		}
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
			frames = append(frames, serverFrame{h2.FrameHeader{Type: h2.FrameData, Flags: flags, StreamID: 1}, append([]byte(nil), remaining[:count]...)})
			remaining = remaining[count:]
		}
		if len(body) == 0 {
			frames[1].header.Flags |= h2.FlagEndStream
		}
		wire := serverWire(t, frames...)
		chunks := splitBytes(wire, 1+int(stride%127))
		stream := &scriptedStream{reads: chunks}
		var got []byte
		response, err := (Client{MaxResponseBodyBytes: uint64(len(body)) + 1}).DoBuffer(stream, basicRequest(), &Callbacks{Body: func(fragment []byte) { got = append(got, fragment...) }}, make([]byte, 1+int(stride)))
		if err != nil {
			t.Fatal(err)
		}
		if response.Status != status || response.BodyBytes != uint64(len(body)) || !bytes.Equal(got, body) {
			t.Fatalf("response=%+v body=%x/%x", response, got, body)
		}
	})
}

func FuzzClientArbitraryServerBytes(f *testing.F) {
	settings := mustRequestFuzzFrame(h2.FrameHeader{Type: h2.FrameSettings}, nil)
	f.Add(append(append([]byte(nil), settings...), 0xff), uint8(1))
	f.Add(serverWireSeed(
		serverFrame{h2.FrameHeader{Type: h2.FrameSettings}, nil},
		serverFrame{h2.FrameHeader{Type: h2.FramePing}, []byte("12345678")},
	), uint8(9))
	f.Fuzz(func(t *testing.T, wire []byte, stride uint8) {
		if len(wire) > 1<<20 {
			t.Skip()
		}
		stream := &fuzzStream{reads: splitBytes(wire, 1+int(stride%64))}
		_, _ = (Client{
			FrameLimits:          h2.Limits{MaxFrameSize: 64 << 10, MaxHeaderBlockBytes: 64 << 10, MaxContinuationFrames: 64},
			HeaderLimits:         h2.HeaderLimits{MaxDynamicTableBytes: 4096, MaxFieldBytes: 4096, MaxHeaderListBytes: 16384, MaxHeaders: 64},
			MaxResponseBodyBytes: 1 << 20,
		}).DoBuffer(stream, basicRequest(), nil, make([]byte, 1+int(stride%127)))
	})
}

func statusString(status uint16) string {
	return string([]byte{'0' + byte(status/100), '0' + byte(status/10%10), '0' + byte(status%10)})
}

func decimalString(value uint64) string {
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

func mustRequestFuzzFrame(header h2.FrameHeader, payload []byte) []byte {
	wire, code := h2.AppendFrame(nil, header, payload)
	if code != h2.CodeNone {
		panic(code.String())
	}
	return wire
}

func serverWireSeed(frames ...serverFrame) []byte {
	var wire []byte
	for _, frame := range frames {
		var code h2.Code
		wire, code = h2.AppendFrame(wire, frame.header, frame.payload)
		if code != h2.CodeNone {
			panic(code.String())
		}
	}
	return wire
}

type fuzzStream struct {
	writes bytes.Buffer
	reads  [][]byte
	index  int
	offset int
}

func (stream *fuzzStream) Write(data []byte) (int, error) { return stream.writes.Write(data) }
func (stream *fuzzStream) Read(dst []byte) (int, error) {
	if stream.index >= len(stream.reads) {
		return 0, io.EOF
	}
	current := stream.reads[stream.index]
	n := copy(dst, current[stream.offset:])
	stream.offset += n
	if stream.offset == len(current) {
		stream.index++
		stream.offset = 0
	}
	return n, nil
}
