package request

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"sync/atomic"
	"testing"
	"time"

	h2 "github.com/wago-org/http/http2"
)

func TestTransportRequestAndReaderCoverage(t *testing.T) {
	base := basicRequest()
	base.Body = []byte("body")
	fields, body, length, err := transportRequest(base)
	if err != nil || body == nil || length != 4 || len(fields) < 5 {
		t.Fatalf("request=%d,%v", length, err)
	}
	read, err := io.ReadAll(body)
	if err != nil || string(read) != "body" {
		t.Fatalf("body=%q,%v", read, err)
	}

	connect := Request{Method: []byte("CONNECT"), Authority: []byte("x:443")}
	if fields, _, _, err = transportRequest(connect); err != nil || len(fields) != 2 {
		t.Fatalf("connect=%v", err)
	}
	extended := Request{Method: []byte("CONNECT"), Scheme: []byte("https"), Authority: []byte("x"), Path: []byte("/"), Protocol: []byte("websocket")}
	if fields, _, _, err = transportRequest(extended); err != nil || len(fields) != 5 {
		t.Fatalf("extended=%v", err)
	}

	for _, request := range []Request{
		{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("x"), Path: []byte("/"), Body: []byte("x"), BodyReader: bytes.NewReader(nil)},
		{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("x"), Path: []byte("/"), Headers: []Header{{Name: []byte("host"), Value: []byte("x")}}},
		{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("x"), Path: []byte("/"), Protocol: []byte("websocket")},
		{Method: []byte("POST"), Scheme: []byte("http"), Authority: []byte("x"), Path: []byte("/"), BodyReader: bytes.NewReader(nil), HasBodyLength: true, BodyLength: -1},
	} {
		if _, _, _, err := transportRequest(request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
	if !reservedTrailer([]byte("te")) || !reservedTrailer([]byte("content-length")) || reservedTrailer([]byte("x-ok")) {
		t.Fatal("reserved trailer classification")
	}
	if !reservedHeaderName([]byte("upgrade")) || reservedHeaderName([]byte("x-ok")) {
		t.Fatal("reserved header classification")
	}
}

func TestTransportExchangeConsumeCoverage(t *testing.T) {
	var headers, trailers, bodies, complete atomic.Int32
	exchange := &transportExchange{maxBody: 8, callbacks: &Callbacks{
		Header:           func(h2.HeaderField, bool) { headers.Add(1) },
		HeadersComplete:  func(Response, bool) { headers.Add(1) },
		Body:             func([]byte) { bodies.Add(1) },
		TrailersComplete: func() { trailers.Add(1) },
		ResponseComplete: func(Response) { complete.Add(1) },
	}}
	if err := exchange.consume(h2.Event{Type: h2.EventHeaders, Headers: []h2.HeaderField{{Name: ":status", Value: "103"}}}); err != nil {
		t.Fatal(err)
	}
	if err := exchange.consume(h2.Event{Type: h2.EventHeaders, Headers: []h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "2"}}}); err != nil {
		t.Fatal(err)
	}
	if err := exchange.consume(h2.Event{Type: h2.EventData, Data: []byte("ok")}); err != nil {
		t.Fatal(err)
	}
	if err := exchange.consume(h2.Event{Type: h2.EventHeaders, Trailer: true, Headers: []h2.HeaderField{{Name: "x", Value: "y"}}}); err != nil {
		t.Fatal(err)
	}
	if err := exchange.consume(h2.Event{Type: h2.EventStreamEnd}); err != nil {
		t.Fatal(err)
	}
	if headers.Load() < 5 || trailers.Load() != 1 || bodies.Load() != 1 || complete.Load() != 1 {
		t.Fatal("callbacks missing")
	}

	for _, event := range []h2.Event{
		{Type: h2.EventHeaders, Headers: []h2.HeaderField{{Name: ":status", Value: "bad"}}},
		{Type: h2.EventHeaders, Headers: []h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "bad"}}},
		{Type: h2.EventHeaders, Headers: []h2.HeaderField{{Name: "x", Value: "y"}}},
		{Type: h2.EventData, Data: []byte("x")},
		{Type: h2.EventStreamEnd},
	} {
		fresh := &transportExchange{maxBody: 1}
		if err := fresh.consume(event); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("event=%+v err=%v", event, err)
		}
	}
	tooLarge := &transportExchange{maxBody: 1, finalHeaders: true}
	if err := tooLarge.consume(h2.Event{Type: h2.EventData, Data: []byte("xx")}); !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("large=%v", err)
	}
	reset := &transportExchange{}
	var streamErr *StreamError
	if err := reset.consume(h2.Event{Type: h2.EventStreamReset, ErrorCode: h2.ErrCodeRefusedStream}); !errors.As(err, &streamErr) || !streamErr.Retryable {
		t.Fatalf("reset=%v", err)
	}
}

func TestTransportBodyAndTrailerInternalCoverage(t *testing.T) {
	newOpen := func(t *testing.T) (*Transport, uint32, *transportExchange) {
		t.Helper()
		session, err := h2.NewSession(h2.RoleClient, h2.SessionLimits{MaxQueuedOutputBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		transport := &Transport{session: session, stream: &coverageTransportStream{}, pending: make(map[uint32]*transportExchange), eventBuffer: 4, maxResponseBody: 8, window: make(chan struct{}, 1), done: make(chan struct{})}
		transport.mu.Lock()
		_ = transport.flushLocked()
		transport.mu.Unlock()
		id, err := session.OpenStream([]h2.HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}, false)
		if err != nil {
			t.Fatal(err)
		}
		exchange := &transportExchange{events: make(chan h2.Event, 4), maxBody: 8}
		transport.pending[id] = exchange
		return transport, id, exchange
	}
	transport, id, _ := newOpen(t)
	if err := transport.sendTrailers(id, []Header{{Name: []byte("x-end"), Value: []byte("yes"), Sensitive: true}}); err != nil {
		t.Fatal(err)
	}
	for _, trailers := range [][]Header{
		{{Name: []byte("Bad"), Value: []byte("x")}},
		{{Name: []byte("x"), Value: []byte{'x', '\r'}}},
		{{Name: []byte("content-length"), Value: []byte("1")}},
	} {
		transport, id, _ = newOpen(t)
		if err := transport.sendTrailers(id, trailers); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("trailers=%v err=%v", trailers, err)
		}
	}
	for _, test := range []struct {
		name   string
		reader io.Reader
		want   error
	}{
		{"negative", readerFunc(func([]byte) (int, error) { return -1, nil }), io.ErrShortBuffer},
		{"oversized", readerFunc(func(dst []byte) (int, error) { return len(dst) + 1, nil }), io.ErrShortBuffer},
		{"zero", readerFunc(func([]byte) (int, error) { return 0, nil }), io.ErrNoProgress},
		{"error", readerFunc(func([]byte) (int, error) { return 0, errors.New("body") }), errors.New("body")},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport, id, exchange := newOpen(t)
			err := transport.sendBody(context.Background(), id, test.reader, nil, exchange)
			if err == nil || err.Error() != test.want.Error() {
				t.Fatalf("sendBody=%v", err)
			}
		})
	}
	transport, id, exchange := newOpen(t)
	if err := transport.sendBody(context.Background(), id, bytes.NewReader([]byte("abc")), nil, exchange); err != nil {
		t.Fatal(err)
	}
	transport, id, exchange = newOpen(t)
	if err := transport.sendBody(context.Background(), id, bytes.NewReader(nil), []Header{{Name: []byte("x-end"), Value: []byte("yes")}}, exchange); err != nil {
		t.Fatal(err)
	}

	transport, _, exchange = newOpen(t)
	exchange.events <- h2.Event{Type: h2.EventHeaders, Headers: []h2.HeaderField{{Name: ":status", Value: "bad"}}}
	if err := transport.consumeAvailable(exchange); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("consume error=%v", err)
	}
	closed := &transportExchange{events: make(chan h2.Event)}
	close(closed.events)
	transport.err = errors.New("closed")
	if err := transport.consumeAvailable(closed); err == nil || err.Error() != "closed" {
		t.Fatalf("consume closed=%v", err)
	}
}

func TestTransportInternalFailureCoverage(t *testing.T) {
	for _, read := range []func([]byte) (int, error){
		func([]byte) (int, error) { return -1, nil },
		func(dst []byte) (int, error) { return len(dst) + 1, nil },
		func([]byte) (int, error) { return 0, nil },
		func([]byte) (int, error) { return 0, errors.New("read") },
		func([]byte) (int, error) { return 0, io.EOF },
	} {
		stream := &coverageTransportStream{read: read}
		transport, err := NewTransport(stream, TransportOptions{ReadBufferBytes: 8})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-transport.done:
		case <-time.After(time.Second):
			t.Fatal("read loop did not stop")
		}
	}
	if _, err := NewTransport(&coverageTransportStream{}, TransportOptions{EventBuffer: 1<<16 + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("event buffer=%v", err)
	}
	if uint64(math.MaxUint32) > uint64(^uint(0)>>1) {
		if _, err := NewTransport(&coverageTransportStream{}, TransportOptions{ReadBufferBytes: math.MaxUint32}); !errors.Is(err, ErrReadBufferTooLarge) {
			t.Fatalf("read buffer=%v", err)
		}
	}

	session, _ := h2.NewSession(h2.RoleClient, h2.SessionLimits{})
	transport := &Transport{session: session, stream: &coverageTransportStream{}, pending: make(map[uint32]*transportExchange), eventBuffer: 1, maxResponseBody: 8, window: make(chan struct{}, 1), done: make(chan struct{})}
	if err := transport.connectionError(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("connection=%v", err)
	}
	transport.dispatch([]h2.Event{{Type: h2.EventGoAway, LastStreamID: 3}, {Type: h2.EventUnknown, StreamID: 99}})
	if transport.goAway == nil {
		t.Fatal("GOAWAY not retained")
	}
	if _, err := transport.Do(context.Background(), basicRequest(), nil); !errors.Is(err, ErrGoAway) {
		t.Fatalf("Do GOAWAY=%v", err)
	}
	transport.goAway = nil
	transport.err = errors.New("failed")
	if _, err := transport.Do(context.Background(), basicRequest(), nil); err == nil || err.Error() != "failed" {
		t.Fatalf("Do failed=%v", err)
	}
	transport.acceptPush(h2.Event{Type: h2.EventPushPromise, StreamID: 2})
	transport.pushHandler = func(PushRequest) (*Callbacks, bool) { return nil, false }
	transport.acceptPush(h2.Event{Type: h2.EventPushPromise, StreamID: 4})
	if err := transport.connectionError(); err == nil || err.Error() != "failed" {
		t.Fatalf("connection failed=%v", err)
	}
	transport.fail(errors.New("ignored"))

	waitingSession, _ := h2.NewSession(h2.RoleClient, h2.SessionLimits{})
	waitingDone := make(chan struct{})
	close(waitingDone)
	waiting := &Transport{session: waitingSession, stream: &coverageTransportStream{}, pending: make(map[uint32]*transportExchange), eventBuffer: 1, maxResponseBody: 8, window: make(chan struct{}, 1), done: waitingDone}
	if _, err := waiting.Do(context.Background(), basicRequest(), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("done Do=%v", err)
	}

	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (*Transport)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

type readerFunc func([]byte) (int, error)

func (reader readerFunc) Read(dst []byte) (int, error) { return reader(dst) }

type coverageTransportStream struct {
	read   func([]byte) (int, error)
	write  func([]byte) (int, error)
	closed bool
}

func (stream *coverageTransportStream) Read(dst []byte) (int, error) {
	if stream.read != nil {
		return stream.read(dst)
	}
	return 0, io.EOF
}
func (stream *coverageTransportStream) Write(src []byte) (int, error) {
	if stream.write != nil {
		return stream.write(src)
	}
	return len(src), nil
}
func (stream *coverageTransportStream) Close() error { stream.closed = true; return nil }
