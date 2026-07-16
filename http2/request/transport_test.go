package request

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	h2 "github.com/wago-org/http/http2"
	h2server "github.com/wago-org/http/http2/server"
)

func TestTransportPersistentMultiplexingAndStreamingBody(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		var mu sync.Mutex
		bodies := make(map[uint32]int)
		handler := h2server.HandlerFuncs{
			OnHeaders: func(writer *h2server.ResponseWriter, fields []h2.HeaderField, end, trailer bool) {
				if trailer {
					return
				}
				method := headerValue(fields, ":method")
				if method == "GET" && end {
					body := []byte("ok-" + headerValue(fields, ":path"))
					_ = writer.Headers([]h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: strconv.Itoa(len(body))}}, false)
					_, _ = writer.Write(body, true)
				}
			},
			OnData: func(writer *h2server.ResponseWriter, data []byte) {
				mu.Lock()
				bodies[writer.StreamID()] += len(data)
				mu.Unlock()
			},
			OnEnd: func(writer *h2server.ResponseWriter) {
				mu.Lock()
				total := bodies[writer.StreamID()]
				mu.Unlock()
				body := []byte(strconv.Itoa(total))
				_ = writer.Headers([]h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: strconv.Itoa(len(body))}}, false)
				_, _ = writer.Write(body, true)
			},
		}
		server, err := h2server.New(conn, handler, h2server.Options{Session: h2.SessionLimits{MaxQueuedOutputBytes: 2 << 20, MaxQueuedEventBytes: 2 << 20}})
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- server.Serve()
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewTransport(conn, TransportOptions{Session: h2.SessionLimits{MaxQueuedOutputBytes: 2 << 20, MaxQueuedEventBytes: 2 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()

	const requests = 16
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			path := "/" + strconv.Itoa(index)
			var body bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := transport.Do(ctx, Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("example.test"), Path: []byte(path)}, &Callbacks{Body: func(data []byte) { body.Write(data) }})
			if err == nil && body.String() != "ok-"+path {
				err = io.ErrUnexpectedEOF
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var trailerOnlyBody bytes.Buffer
	response, err := transport.Do(nil, Request{
		Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("example.test"), Path: []byte("/trailers"),
		Trailers: []Header{{Name: []byte("x-finished"), Value: []byte("yes")}},
	}, &Callbacks{Body: func(data []byte) { trailerOnlyBody.Write(data) }})
	if err != nil || response.Status != 200 || trailerOnlyBody.String() != "0" {
		t.Fatalf("trailer-only response=%+v body=%q err=%v", response, trailerOnlyBody.String(), err)
	}

	large := bytes.Repeat([]byte("x"), 200000)
	var responseBody bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err = transport.Do(ctx, Request{
		Method: []byte("POST"), Scheme: []byte("http"), Authority: []byte("example.test"), Path: []byte("/upload"),
		BodyReader: bytes.NewReader(large), BodyLength: int64(len(large)), HasBodyLength: true,
		Trailers: []Header{{Name: []byte("x-upload-complete"), Value: []byte("yes")}},
	}, &Callbacks{Body: func(data []byte) { responseBody.Write(data) }})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || responseBody.String() != "200000" {
		t.Fatalf("response=%+v body=%q", response, responseBody.String())
	}
}

func TestTransportServerPush(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		stream, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		connection, err := h2server.New(stream, h2server.HandlerFuncs{OnEnd: func(writer *h2server.ResponseWriter) {
			pushed, pushErr := writer.PushPromise([]h2.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/asset"}})
			if pushErr != nil {
				t.Error(pushErr)
				return
			}
			if pushErr = pushed.Headers([]h2.HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "4"}}, false); pushErr != nil {
				t.Error(pushErr)
				return
			}
			if _, pushErr = pushed.Write([]byte("push"), true); pushErr != nil {
				t.Error(pushErr)
				return
			}
			if pushErr = writer.Headers([]h2.HeaderField{{Name: ":status", Value: "204"}}, true); pushErr != nil {
				t.Error(pushErr)
			}
		}}, h2server.Options{Session: h2.SessionLimits{EnablePush: true}})
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- connection.Serve()
	}()
	stream, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	pushedBody := make(chan string, 1)
	var body bytes.Buffer
	transport, err := NewTransport(stream, TransportOptions{
		Session: h2.SessionLimits{EnablePush: true},
		PushHandler: func(request PushRequest) (*Callbacks, bool) {
			if request.StreamID != 2 || headerValue(request.Headers, ":path") != "/asset" {
				t.Errorf("push=%+v", request)
			}
			return &Callbacks{Body: func(fragment []byte) { body.Write(fragment) }, ResponseComplete: func(Response) { pushedBody <- body.String() }}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	response, err := transport.Do(context.Background(), Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("example.test"), Path: []byte("/")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 204 {
		t.Fatalf("status=%d", response.Status)
	}
	select {
	case got := <-pushedBody:
		if got != "push" {
			t.Fatalf("push body=%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("push timeout")
	}
	_ = transport.Close()
	select {
	case <-serverErr:
	case <-time.After(2 * time.Second):
		t.Fatal("server shutdown timeout")
	}
}

func TestPoolRetriesRefusedStreamOnNewConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		first, _ := listener.Accept()
		if first != nil {
			go func() {
				connection, newErr := h2server.New(first, h2server.HandlerFuncs{OnEnd: func(writer *h2server.ResponseWriter) {
					_ = writer.Reset(h2.ErrCodeRefusedStream)
				}}, h2server.Options{})
				if newErr == nil {
					_ = connection.Serve()
				} else {
					_ = first.Close()
				}
			}()
		}
		second, _ := listener.Accept()
		if second != nil {
			connection, newErr := h2server.New(second, h2server.HandlerFuncs{OnEnd: func(writer *h2server.ResponseWriter) {
				_ = writer.Headers([]h2.HeaderField{{Name: ":status", Value: "204"}}, true)
			}}, h2server.Options{})
			if newErr == nil {
				_ = connection.Serve()
			} else {
				_ = second.Close()
			}
		}
	}()
	pool := &Pool{Dial: func(context.Context) (Stream, error) { return net.Dial("tcp", listener.Addr().String()) }}
	defer pool.Close()
	response, err := pool.Do(context.Background(), Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("x"), Path: []byte("/")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 204 {
		t.Fatalf("status=%d", response.Status)
	}
}

func TestTransportContextCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		server, err := h2server.New(conn, h2server.HandlerFuncs{}, h2server.Options{})
		if err != nil {
			return
		}
		_ = server.Serve()
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewTransport(conn, TransportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = transport.Do(ctx, Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("example.test"), Path: []byte("/")}, nil)
	if err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func headerValue(fields []h2.HeaderField, name string) string {
	for _, field := range fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}
