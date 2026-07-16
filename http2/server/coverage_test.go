package server

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	h2 "github.com/wago-org/http/http2"
)

func TestHandlerFuncsAndDispatchCoverage(t *testing.T) {
	var headers, data, ends, writable, resets int
	handler := HandlerFuncs{
		OnHeaders:  func(*ResponseWriter, []h2.HeaderField, bool, bool) { headers++ },
		OnData:     func(*ResponseWriter, []byte) { data++ },
		OnEnd:      func(*ResponseWriter) { ends++ },
		OnWritable: func(*ResponseWriter, uint32) { writable++ },
		OnReset:    func(uint32, h2.ErrorCode) { resets++ },
	}
	conn := &Conn{handler: handler, writers: make(map[uint32]*ResponseWriter)}
	conn.writers[1] = &ResponseWriter{conn: conn, streamID: 1}
	conn.dispatch([]h2.Event{
		{Type: h2.EventHeaders, StreamID: 1},
		{Type: h2.EventData, StreamID: 1, Data: []byte("x")},
		{Type: h2.EventStreamEnd, StreamID: 1},
		{Type: h2.EventWindowUpdate, StreamID: 1, WindowIncrement: 1},
		{Type: h2.EventWindowUpdate, WindowIncrement: 2},
		{Type: h2.EventStreamReset, StreamID: 1, ErrorCode: h2.ErrCodeCancel},
	})
	if headers != 1 || data != 1 || ends != 1 || writable != 2 || resets != 1 {
		t.Fatalf("callbacks=%d,%d,%d,%d,%d", headers, data, ends, writable, resets)
	}
	HandlerFuncs{}.Headers(nil, nil, false, false)
	HandlerFuncs{}.Data(nil, nil)
	HandlerFuncs{}.End(nil)
	HandlerFuncs{}.Writable(nil, 0)
	HandlerFuncs{}.Reset(0, 0)
}

func TestResponseWriterPushCloseAndErrors(t *testing.T) {
	conn, client, stream := coverageServerConn(t, true)
	writer := conn.writer(1)
	if err := writer.Headers([]h2.HeaderField{{Name: ":status", Value: "103"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := writer.PriorityUpdate([]byte("u=1")); err != nil {
		t.Fatal(err)
	}
	pushed, err := writer.PushPromise([]h2.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/p"}})
	if err != nil || pushed.StreamID() != 2 {
		t.Fatalf("push=%v,%v", pushed, err)
	}
	if err := pushed.Reset(h2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	if err := writer.Headers([]h2.HeaderField{{Name: ":status", Value: "200"}}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("ok"), true); n != 2 || err != nil {
		t.Fatalf("write=%d,%v", n, err)
	}
	wire := append([]byte(nil), stream.writes.Bytes()...)
	if n, err := client.Feed(wire); n != len(wire) || err != nil {
		t.Fatalf("client feed=%d,%v", n, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Headers(nil, true); !errors.Is(err, h2.ErrSessionClosed) {
		t.Fatalf("closed headers=%v", err)
	}
	if n, err := writer.Write(nil, true); n != 0 || !errors.Is(err, h2.ErrSessionClosed) {
		t.Fatalf("closed write=%d,%v", n, err)
	}
	if err := (*Conn)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServerConstructionServeAndWriteFailures(t *testing.T) {
	if _, err := New(nil, HandlerFuncs{}, Options{}); err == nil {
		t.Fatal("nil stream accepted")
	}
	if _, err := New(&coverageStream{}, nil, Options{}); err == nil {
		t.Fatal("nil handler accepted")
	}
	if uint64(math.MaxUint32) > uint64(^uint(0)>>1) {
		if _, err := New(&coverageStream{}, HandlerFuncs{}, Options{ReadBufferBytes: math.MaxUint32}); !errors.Is(err, io.ErrShortBuffer) {
			t.Fatalf("large buffer=%v", err)
		}
	}
	for _, test := range []struct {
		name string
		read func([]byte) (int, error)
		want error
	}{
		{"negative", func([]byte) (int, error) { return -1, nil }, io.ErrShortBuffer},
		{"oversized", func(dst []byte) (int, error) { return len(dst) + 1, nil }, io.ErrShortBuffer},
		{"empty", func([]byte) (int, error) { return 0, nil }, io.ErrNoProgress},
		{"read-error", func([]byte) (int, error) { return 0, errors.New("read") }, errors.New("read")},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &functionServerStream{read: test.read}
			conn, err := New(stream, HandlerFuncs{}, Options{ReadBufferBytes: 8})
			if err != nil {
				t.Fatal(err)
			}
			err = conn.Serve()
			if err == nil || err.Error() != test.want.Error() {
				t.Fatalf("Serve=%v", err)
			}
		})
	}
	for _, write := range []func([]byte) (int, error){
		func([]byte) (int, error) { return -1, nil },
		func(src []byte) (int, error) { return len(src) + 1, nil },
		func([]byte) (int, error) { return 0, nil },
		func([]byte) (int, error) { return 0, errors.New("write") },
	} {
		if _, err := New(&functionServerStream{write: write}, HandlerFuncs{}, Options{}); err == nil {
			t.Fatal("bad initial write accepted")
		}
	}
}

func coverageServerConn(t *testing.T, push bool) (*Conn, *h2.Session, *coverageStream) {
	t.Helper()
	stream := &coverageStream{}
	conn, err := New(stream, HandlerFuncs{}, Options{Session: h2.SessionLimits{EnablePush: push}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := h2.NewSession(h2.RoleClient, h2.SessionLimits{EnablePush: push})
	if err != nil {
		t.Fatal(err)
	}
	serverInitial := append([]byte(nil), stream.writes.Bytes()...)
	stream.writes.Reset()
	if _, err := client.Feed(serverInitial); err != nil {
		t.Fatal(err)
	}
	request := []h2.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}
	if _, err := client.OpenStream(request, true); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), client.Output()...)
	if _, err := conn.session.Feed(wire); err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := conn.session.NextEvent(); !ok {
			break
		}
	}
	stream.writes.Reset()
	return conn, client, stream
}

type coverageStream struct {
	writes bytes.Buffer
	closed bool
}

func (*coverageStream) Read([]byte) (int, error)             { return 0, io.EOF }
func (stream *coverageStream) Write(src []byte) (int, error) { return stream.writes.Write(src) }
func (stream *coverageStream) Close() error                  { stream.closed = true; return nil }

type functionServerStream struct {
	read  func([]byte) (int, error)
	write func([]byte) (int, error)
}

func (stream *functionServerStream) Read(dst []byte) (int, error) {
	if stream.read != nil {
		return stream.read(dst)
	}
	return 0, io.EOF
}
func (stream *functionServerStream) Write(src []byte) (int, error) {
	if stream.write != nil {
		return stream.write(src)
	}
	return len(src), nil
}
