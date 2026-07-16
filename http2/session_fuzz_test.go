package http2

import "testing"

func FuzzSessionArbitraryInput(f *testing.F) {
	client, _ := NewSession(RoleClient, SessionLimits{})
	f.Add(append([]byte(nil), client.Output()...), uint8(RoleServer), uint8(1))
	f.Add([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), uint8(RoleServer), uint8(7))
	f.Add([]byte{0xff, 0xff, 0xff}, uint8(RoleClient), uint8(3))
	f.Fuzz(func(t *testing.T, input []byte, roleByte, strideByte uint8) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		role := RoleClient
		if roleByte&1 != 0 {
			role = RoleServer
		}
		session, err := NewSession(role, SessionLimits{
			MaxStreams: 64, MaxConcurrentStreams: 32, MaxQueuedOutputBytes: 1 << 20,
			MaxQueuedEventBytes: 1 << 20, MaxQueuedEvents: 256, MaxControlFrames: 256,
		})
		if err != nil {
			t.Fatal(err)
		}
		stride := 1 + int(strideByte%64)
		for offset := 0; offset < len(input); {
			end := offset + stride
			if end > len(input) {
				end = len(input)
			}
			n, feedErr := session.Feed(input[offset:end])
			if n < 0 || n > end-offset {
				t.Fatalf("consumed=%d/%d", n, end-offset)
			}
			offset += n
			if feedErr != nil {
				break
			}
			if n == 0 {
				break
			}
			for {
				if _, ok := session.NextEvent(); !ok {
					break
				}
			}
			if len(session.Output()) != 0 {
				_ = session.ConsumeOutput(len(session.Output()))
			}
		}
		_ = session.Finish()
		session.Close()
	})
}

func FuzzSessionClientServerOperations(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7}, uint8(17))
	f.Add([]byte("multiplex-flow-control"), uint8(1))
	f.Fuzz(func(t *testing.T, operations []byte, stride uint8) {
		if len(operations) > 4096 {
			t.Skip()
		}
		limits := SessionLimits{MaxStreams: 128, MaxConcurrentStreams: 64, MaxQueuedOutputBytes: 2 << 20, MaxQueuedEventBytes: 2 << 20, EnableExtendedConnect: true}
		client, _ := NewSession(RoleClient, limits)
		server, _ := NewSession(RoleServer, limits)
		fuzzPumpSessions(t, client, server, stride)
		fuzzPumpSessions(t, server, client, stride)
		var streams []uint32
		for _, operation := range operations {
			switch operation % 8 {
			case 0:
				id, err := client.OpenStream([]HeaderField{{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}, false)
				if err == nil {
					streams = append(streams, id)
				}
			case 1:
				if len(streams) != 0 {
					_, _ = client.SendData(streams[int(operation)%len(streams)], []byte{operation}, false)
				}
			case 2:
				if len(streams) != 0 {
					_, _ = client.SendData(streams[int(operation)%len(streams)], nil, true)
				}
			case 3:
				if len(streams) != 0 {
					_ = client.ResetStream(streams[int(operation)%len(streams)], ErrCodeCancel)
				}
			case 4:
				var ping [8]byte
				for index := range ping {
					ping[index] = operation
				}
				_ = client.Ping(ping)
			case 5:
				_ = client.PriorityUpdate(uint32(operation), []byte("u=1"))
			case 6:
				_ = client.UpdateSettings([]Setting{{ID: SettingMaxConcurrentStreams, Value: uint32(operation % 64)}})
			case 7:
				for {
					if _, ok := client.NextEvent(); !ok {
						break
					}
				}
				for {
					if _, ok := server.NextEvent(); !ok {
						break
					}
				}
			}
			fuzzPumpSessions(t, client, server, stride)
			fuzzPumpSessions(t, server, client, stride)
			if client.Failed() != nil || server.Failed() != nil {
				return
			}
		}
	})
}

func fuzzPumpSessions(t *testing.T, source, destination *Session, strideByte uint8) {
	t.Helper()
	wire := append([]byte(nil), source.Output()...)
	_ = source.ConsumeOutput(len(wire))
	stride := 1 + int(strideByte%31)
	for offset := 0; offset < len(wire); {
		end := offset + stride
		if end > len(wire) {
			end = len(wire)
		}
		n, err := destination.Feed(wire[offset:end])
		offset += n
		if err != nil || n == 0 {
			return
		}
	}
}
