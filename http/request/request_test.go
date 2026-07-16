package request

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	http1 "github.com/wago-org/http/http"
)

func TestEncodeRequest(t *testing.T) {
	req := Request{
		Method: []byte("POST"), Target: []byte("/items?q=1"), Host: []byte("example.test"),
		Headers: []Header{{Name: []byte("Content-Type"), Value: []byte("application/json")}},
		Body:    []byte(`{"ok":true}`),
	}
	const want = "POST /items?q=1 HTTP/1.1\r\nHost: example.test\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n{\"ok\":true}"

	size, err := EncodedLen(req, http1.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if size != len(want) {
		t.Fatalf("EncodedLen = %d, want %d", size, len(want))
	}
	buf := make([]byte, size)
	n, err := Encode(buf, req, http1.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != want {
		t.Fatalf("Encode = %q, want %q", got, want)
	}

	var written bytes.Buffer
	if err := Write(&written, req, http1.Limits{}); err != nil {
		t.Fatal(err)
	}
	if written.String() != want {
		t.Fatalf("Write = %q, want %q", written.String(), want)
	}
}

func TestWriteRetriesShortWrites(t *testing.T) {
	req := Request{Method: []byte("GET"), Target: []byte("/"), Host: []byte("example.test")}
	writer := &shortWriter{maximum: 2}
	if err := Write(writer, req, http1.Limits{}); err != nil {
		t.Fatal(err)
	}
	const want = "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"
	if writer.String() != want {
		t.Fatalf("Write = %q, want %q", writer.String(), want)
	}
}

func TestExplicitZeroContentLength(t *testing.T) {
	req := Request{Method: []byte("POST"), Target: []byte("/"), Host: []byte("example.test"), ContentLength: true}
	encoded, err := Append(nil, req, http1.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	const want = "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 0\r\n\r\n"
	if string(encoded) != want {
		t.Fatalf("Append = %q, want %q", encoded, want)
	}
}

func TestRequestValidation(t *testing.T) {
	valid := Request{Method: []byte("GET"), Target: []byte("/"), Host: []byte("example.test")}
	tests := []struct {
		name string
		req  Request
		want error
	}{
		{name: "missing method", req: Request{Target: valid.Target, Host: valid.Host}, want: ErrInvalidRequest},
		{name: "header injection", req: Request{Method: valid.Method, Target: valid.Target, Host: valid.Host, Headers: []Header{{Name: []byte("X-Test"), Value: []byte("ok\r\nInjected: yes")}}}},
		{name: "bad target", req: Request{Method: valid.Method, Target: []byte("/has space"), Host: valid.Host}},
		{name: "duplicate host", req: Request{Method: valid.Method, Target: valid.Target, Host: valid.Host, Headers: []Header{{Name: []byte("Host"), Value: []byte("other.test")}}}, want: ErrReservedHeader},
		{name: "content length", req: Request{Method: valid.Method, Target: valid.Target, Host: valid.Host, Headers: []Header{{Name: []byte("content-length"), Value: []byte("0")}}}, want: ErrReservedHeader},
		{name: "transfer encoding", req: Request{Method: valid.Method, Target: valid.Target, Host: valid.Host, Headers: []Header{{Name: []byte("Transfer-Encoding"), Value: []byte("chunked")}}}, want: ErrReservedHeader},
		{name: "body limit", req: Request{Method: []byte("POST"), Target: valid.Target, Host: valid.Host, Body: []byte("12345")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := http1.Limits{}
			if test.name == "body limit" {
				limits.MaxBodyBytes = 4
			}
			err := Validate(test.req, limits)
			if err == nil {
				t.Fatal("Validate succeeded")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Validate error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestEncodeNoAllocations(t *testing.T) {
	req := Request{
		Method: []byte("POST"), Target: []byte("/items"), Host: []byte("example.test"),
		Headers: []Header{{Name: []byte("Content-Type"), Value: []byte("text/plain")}},
		Body:    []byte("payload"),
	}
	size, err := EncodedLen(req, http1.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, size)
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, err := Encode(dst, req, http1.Limits{}); err != nil {
			panic(err)
		}
	}); allocations != 0 {
		t.Fatalf("Encode allocations = %v, want 0", allocations)
	}
}

func TestEncodeShortBufferDoesNotModifyDestination(t *testing.T) {
	req := Request{Method: []byte("GET"), Target: []byte("/"), Host: []byte("example.test")}
	dst := bytes.Repeat([]byte{0xaa}, 8)
	before := append([]byte(nil), dst...)
	if _, err := Encode(dst, req, http1.Limits{}); !errors.Is(err, ErrShortBuffer) {
		t.Fatalf("Encode error = %v", err)
	}
	if !bytes.Equal(dst, before) {
		t.Fatal("Encode modified a short destination")
	}
}

func TestClientInformationalThenFinalAndBuffered(t *testing.T) {
	stream := &scriptedStream{reads: [][]byte{
		[]byte("HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 4\r\nX-Test: yes\r\n\r\ndataTAIL"),
	}}
	req := Request{Method: []byte("GET"), Target: []byte("/"), Host: []byte("example.test")}
	var statuses []uint16
	var body []byte
	callbacks := http1.Callbacks{
		Body: func(fragment []byte) { body = append(body, fragment...) },
		MessageComplete: func(message http1.Message) {
			statuses = append(statuses, message.Status)
		},
	}
	response, err := (Client{}).DoBuffer(stream, req, &callbacks, make([]byte, 256))
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Status != 200 || response.Upgraded {
		t.Fatalf("response = %+v", response)
	}
	if string(response.Buffered) != "TAIL" {
		t.Fatalf("Buffered = %q", response.Buffered)
	}
	if string(body) != "data" {
		t.Fatalf("body = %q", body)
	}
	if !reflect.DeepEqual(statuses, []uint16{103, 200}) {
		t.Fatalf("statuses = %v", statuses)
	}
	const wantRequest = "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"
	if stream.writes.String() != wantRequest {
		t.Fatalf("request = %q, want %q", stream.writes.String(), wantRequest)
	}
}

func TestClientCloseDelimitedResponse(t *testing.T) {
	stream := &scriptedStream{reads: [][]byte{
		[]byte("HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\nhello"),
	}}
	req := Request{Method: []byte("GET"), Target: []byte("/"), Host: []byte("example.test")}
	var body []byte
	response, err := (Client{}).DoBuffer(stream, req, &http1.Callbacks{
		Body: func(fragment []byte) { body = append(body, fragment...) },
	}, make([]byte, 64))
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Status != 200 || string(body) != "hello" {
		t.Fatalf("response = %+v, body = %q", response, body)
	}
}

func TestClientUpgradeReturnsFollowingBytes(t *testing.T) {
	stream := &scriptedStream{reads: [][]byte{
		[]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nframes"),
	}}
	req := Request{Method: []byte("GET"), Target: []byte("/chat"), Host: []byte("example.test"), Headers: []Header{
		{Name: []byte("Connection"), Value: []byte("Upgrade")},
		{Name: []byte("Upgrade"), Value: []byte("websocket")},
	}}
	response, err := (Client{}).DoBuffer(stream, req, nil, make([]byte, 128))
	if err != nil {
		t.Fatal(err)
	}
	if !response.Upgraded || response.Message.Status != 101 || string(response.Buffered) != "frames" {
		t.Fatalf("response = %+v", response)
	}
}

func TestClientRejectsMalformedResponse(t *testing.T) {
	stream := &scriptedStream{reads: [][]byte{[]byte("not http\r\n")}}
	req := Request{Method: []byte("GET"), Target: []byte("/"), Host: []byte("example.test")}
	_, err := (Client{}).DoBuffer(stream, req, nil, make([]byte, 32))
	var parseErr *ParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("error = %T %v, want *ParseError", err, err)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

type shortWriter struct {
	bytes.Buffer
	maximum int
}

func (w *shortWriter) Write(data []byte) (int, error) {
	if len(data) > w.maximum {
		data = data[:w.maximum]
	}
	return w.Buffer.Write(data)
}

type scriptedStream struct {
	writes     bytes.Buffer
	reads      [][]byte
	readIndex  int
	readOffset int
}

func (s *scriptedStream) Write(data []byte) (int, error) {
	return s.writes.Write(data)
}

func (s *scriptedStream) Read(dst []byte) (int, error) {
	if s.readIndex >= len(s.reads) {
		return 0, io.EOF
	}
	current := s.reads[s.readIndex]
	n := copy(dst, current[s.readOffset:])
	s.readOffset += n
	if s.readOffset == len(current) {
		s.readIndex++
		s.readOffset = 0
	}
	return n, nil
}
