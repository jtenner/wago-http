package http2

import "testing"

var benchmarkSessionSink uint64

func BenchmarkSessionPersistentRoundTrip(b *testing.B) {
	for _, streams := range []int{1, 16, 100} {
		b.Run(sessionBenchmarkName(streams), func(b *testing.B) {
			limits := SessionLimits{MaxStreams: ^uint32(0) >> 1, MaxConcurrentStreams: 1024, MaxClosedStreams: 128, MaxQueuedOutputBytes: 8 << 20, MaxQueuedEventBytes: 8 << 20}
			client, _ := NewSession(RoleClient, limits)
			server, _ := NewSession(RoleServer, limits)
			benchmarkPump(client, server)
			benchmarkPump(server, client)
			benchmarkDrain(client)
			benchmarkDrain(server)
			headers := []HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/benchmark"}}
			response := []HeaderField{{Name: ":status", Value: "204"}}
			ids := make([]uint32, streams)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				for index := range ids {
					ids[index], _ = client.OpenStream(headers, true)
				}
				benchmarkPump(client, server)
				benchmarkDrain(server)
				for _, id := range ids {
					_ = server.SendHeaders(id, response, true)
				}
				benchmarkPump(server, client)
				benchmarkSessionSink += uint64(benchmarkDrain(client))
			}
		})
	}
}

func BenchmarkSessionFlowControlledUpload(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		b.Run(sessionBenchmarkName(size), func(b *testing.B) {
			body := make([]byte, size)
			limits := SessionLimits{MaxStreams: ^uint32(0) >> 1, MaxConcurrentStreams: 1024, MaxQueuedOutputBytes: 2 << 20, MaxQueuedEventBytes: 2 << 20}
			client, _ := NewSession(RoleClient, limits)
			server, _ := NewSession(RoleServer, limits)
			benchmarkPump(client, server)
			benchmarkPump(server, client)
			benchmarkDrain(client)
			benchmarkDrain(server)
			request := []HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}
			response := []HeaderField{{Name: ":status", Value: "204"}}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				id, _ := client.OpenStream(request, false)
				offset := 0
				for offset < len(body) {
					n, err := client.SendData(id, body[offset:], false)
					if err != nil && err != ErrWouldBlock {
						b.Fatal(err)
					}
					offset += n
					benchmarkPump(client, server)
					benchmarkDrain(server)
					benchmarkPump(server, client)
					benchmarkDrain(client)
				}
				if _, err := client.SendData(id, nil, true); err != nil {
					b.Fatal(err)
				}
				benchmarkPump(client, server)
				benchmarkDrain(server)
				if err := server.SendHeaders(id, response, true); err != nil {
					b.Fatal(err)
				}
				benchmarkPump(server, client)
				benchmarkDrain(client)
				benchmarkSessionSink += uint64(offset)
			}
		})
	}
}

func benchmarkPump(source, destination *Session) {
	for len(source.Output()) != 0 {
		wire := source.Output()
		n, _ := destination.Feed(wire)
		_ = source.ConsumeOutput(n)
		if n == 0 {
			return
		}
	}
}

func benchmarkDrain(session *Session) int {
	count := 0
	for {
		_, ok := session.NextEvent()
		if !ok {
			return count
		}
		count++
	}
}

func sessionBenchmarkName(value int) string {
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
