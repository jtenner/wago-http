package server

import (
	"bytes"
	"errors"
	"io"
	"testing"

	h2 "github.com/wago-org/http/http2"
)

func TestServerStreamingConnection(t *testing.T) {
	client, err := h2.NewSession(h2.RoleClient, h2.SessionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	streamID, err := client.OpenStream([]h2.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), client.Output()...)
	_ = client.ConsumeOutput(len(wire))
	transport := &scriptedServerStream{reads: [][]byte{wire}}
	var headers int
	conn, err := New(transport, HandlerFuncs{OnHeaders: func(writer *ResponseWriter, fields []h2.HeaderField, end, trailer bool) {
		headers++
		if writer.StreamID() != streamID || !end || trailer {
			t.Fatalf("headers stream=%d end=%t trailer=%t", writer.StreamID(), end, trailer)
		}
		if err := writer.Headers([]h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "2"}}, false); err != nil {
			t.Fatal(err)
		}
		if n, err := writer.Write([]byte("ok"), true); n != 2 || err != nil {
			t.Fatalf("Write=%d,%v", n, err)
		}
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Serve(); !errors.Is(err, io.EOF) {
		t.Fatalf("Serve=%v", err)
	}
	if headers != 1 {
		t.Fatalf("headers=%d", headers)
	}
	serverWire := transport.writes.Bytes()
	if consumed, err := client.Feed(serverWire); consumed != len(serverWire) || err != nil {
		t.Fatalf("client Feed=%d/%d,%v", consumed, len(serverWire), err)
	}
	var gotBody string
	for {
		event, ok := client.NextEvent()
		if !ok {
			break
		}
		if event.Type == h2.EventData {
			gotBody += string(event.Data)
		}
	}
	if gotBody != "ok" {
		t.Fatalf("body=%q", gotBody)
	}
}

func TestServerResponseWriterResetAndPriority(t *testing.T) {
	client, _ := h2.NewSession(h2.RoleClient, h2.SessionLimits{})
	streamID, err := client.OpenStream([]h2.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), client.Output()...)
	transport := &scriptedServerStream{reads: [][]byte{wire}}
	conn, err := New(transport, HandlerFuncs{OnHeaders: func(writer *ResponseWriter, _ []h2.HeaderField, _, _ bool) {
		if err := writer.PriorityUpdate([]byte("u=1")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Reset(h2.ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Serve()
	if len(transport.writes.Bytes()) == 0 || streamID != 1 {
		t.Fatal("no server output")
	}
}

type scriptedServerStream struct {
	reads  [][]byte
	index  int
	writes bytes.Buffer
}

func (stream *scriptedServerStream) Read(dst []byte) (int, error) {
	if stream.index == len(stream.reads) {
		return 0, io.EOF
	}
	n := copy(dst, stream.reads[stream.index])
	stream.index++
	return n, nil
}
func (stream *scriptedServerStream) Write(src []byte) (int, error) { return stream.writes.Write(src) }
