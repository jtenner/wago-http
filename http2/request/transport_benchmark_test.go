package request

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	h2 "github.com/wago-org/http/http2"
	h2server "github.com/wago-org/http/http2/server"
)

func BenchmarkTransportPersistent(b *testing.B) {
	b.Run("sequential", func(b *testing.B) {
		transport, closeBenchmark := benchmarkTransport(b)
		defer closeBenchmark()
		request := Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("benchmark"), Path: []byte("/")}
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			response, err := transport.Do(context.Background(), request, nil)
			if err != nil || response.Status != 204 {
				b.Fatalf("Do=%d,%v", response.Status, err)
			}
		}
	})
	b.Run("concurrent-16", func(b *testing.B) {
		transport, closeBenchmark := benchmarkTransport(b)
		defer closeBenchmark()
		request := Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("benchmark"), Path: []byte("/")}
		var next atomic.Int64
		errs := make(chan error, 16)
		b.ReportAllocs()
		b.ResetTimer()
		var workers sync.WaitGroup
		for worker := 0; worker < 16; worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					if next.Add(1) > int64(b.N) {
						return
					}
					response, err := transport.Do(context.Background(), request, nil)
					if err != nil || response.Status != 204 {
						if err == nil {
							err = ErrInvalidResponse
						}
						errs <- err
						return
					}
				}
			}()
		}
		workers.Wait()
		close(errs)
		for err := range errs {
			b.Fatal(err)
		}
	})
}

func BenchmarkPoolReuse(b *testing.B) {
	transport, closeBenchmark := benchmarkTransport(b)
	defer closeBenchmark()
	pool := &Pool{current: transport, transports: map[*Transport]struct{}{transport: {}}}
	request := Request{Method: []byte("GET"), Scheme: []byte("http"), Authority: []byte("benchmark"), Path: []byte("/")}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		response, err := pool.Do(context.Background(), request, nil)
		if err != nil || response.Status != 204 {
			b.Fatalf("Do=%d,%v", response.Status, err)
		}
	}
}

func BenchmarkTransportFlowControlledUpload(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		b.Run(sessionBenchmarkLabel(size), func(b *testing.B) {
			transport, closeBenchmark := benchmarkTransport(b)
			defer closeBenchmark()
			body := make([]byte, size)
			request := Request{Method: []byte("POST"), Scheme: []byte("http"), Authority: []byte("benchmark"), Path: []byte("/upload"), Body: body}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				response, err := transport.Do(context.Background(), request, nil)
				if err != nil || response.Status != 204 {
					b.Fatalf("Do=%d,%v", response.Status, err)
				}
			}
		})
	}
}

func benchmarkTransport(b *testing.B) (*Transport, func()) {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		stream, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		connection, newErr := h2server.New(stream, h2server.HandlerFuncs{OnEnd: func(writer *h2server.ResponseWriter) {
			_ = writer.Headers([]h2.HeaderField{{Name: ":status", Value: "204"}}, true)
		}}, h2server.Options{Session: h2.SessionLimits{MaxStreams: ^uint32(0) >> 1, MaxConcurrentStreams: 1024, MaxQueuedOutputBytes: 8 << 20, MaxQueuedEventBytes: 8 << 20}})
		if newErr == nil {
			_ = connection.Serve()
		} else {
			_ = stream.Close()
		}
	}()
	stream, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	transport, err := NewTransport(stream, TransportOptions{Session: h2.SessionLimits{MaxStreams: ^uint32(0) >> 1, MaxConcurrentStreams: 1024, MaxQueuedOutputBytes: 8 << 20, MaxQueuedEventBytes: 8 << 20}})
	if err != nil {
		b.Fatal(err)
	}
	return transport, func() { _ = transport.Close(); _ = listener.Close(); wait.Wait() }
}

func sessionBenchmarkLabel(value int) string {
	const digits = "0123456789"
	var buffer [24]byte
	index := len(buffer)
	for {
		index--
		buffer[index] = digits[value%10]
		value /= 10
		if value == 0 {
			return string(buffer[index:])
		}
	}
}
