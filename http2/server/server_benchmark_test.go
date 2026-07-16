package server

import (
	"bytes"
	"io"
	"testing"

	h2 "github.com/wago-org/http/http2"
)

func BenchmarkServerPersistentResponses(b *testing.B) {
	for _, streams := range []int{1, 16, 100} {
		b.Run(serverBenchmarkLabel(streams), func(b *testing.B) {
			limits := h2.SessionLimits{MaxStreams: ^uint32(0) >> 1, MaxConcurrentStreams: 1024, MaxQueuedOutputBytes: 8 << 20, MaxQueuedEventBytes: 8 << 20}
			wire := &benchmarkServerStream{}
			conn, err := New(wire, HandlerFuncs{}, Options{Session: limits})
			if err != nil {
				b.Fatal(err)
			}
			client, err := h2.NewSession(h2.RoleClient, limits)
			if err != nil {
				b.Fatal(err)
			}
			feedBenchmarkSession(b, wire.buffer.Bytes(), client)
			wire.buffer.Reset()
			feedBenchmarkSession(b, client.Output(), conn.session)
			_ = client.ConsumeOutput(len(client.Output()))
			for {
				if _, ok := conn.session.NextEvent(); !ok {
					break
				}
			}
			request := []h2.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "benchmark"}, {Name: ":path", Value: "/"}}
			response := []h2.HeaderField{{Name: ":status", Value: "204"}}
			ids := make([]uint32, streams)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				for index := range ids {
					ids[index], _ = client.OpenStream(request, true)
				}
				feedBenchmarkSession(b, client.Output(), conn.session)
				_ = client.ConsumeOutput(len(client.Output()))
				for {
					if _, ok := conn.session.NextEvent(); !ok {
						break
					}
				}
				for _, id := range ids {
					if err := conn.writer(id).Headers(response, true); err != nil {
						b.Fatal(err)
					}
				}
				feedBenchmarkSession(b, wire.buffer.Bytes(), client)
				wire.buffer.Reset()
				for {
					if _, ok := client.NextEvent(); !ok {
						break
					}
				}
			}
		})
	}
}

func feedBenchmarkSession(b *testing.B, wire []byte, session *h2.Session) {
	b.Helper()
	for len(wire) != 0 {
		n, err := session.Feed(wire)
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal(io.ErrNoProgress)
		}
		wire = wire[n:]
	}
}

type benchmarkServerStream struct{ buffer bytes.Buffer }

func (*benchmarkServerStream) Read([]byte) (int, error)             { return 0, io.EOF }
func (stream *benchmarkServerStream) Write(src []byte) (int, error) { return stream.buffer.Write(src) }

func serverBenchmarkLabel(value int) string {
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
