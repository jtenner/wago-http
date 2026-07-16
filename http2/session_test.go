package http2

import (
	"bytes"
	"errors"
	"testing"
)

func TestSessionClientServerMultiplexedExchange(t *testing.T) {
	client := mustSession(t, RoleClient, SessionLimits{EnableExtendedConnect: true})
	server := mustSession(t, RoleServer, SessionLimits{EnableExtendedConnect: true})
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	drainSettingsEvents(client)
	drainSettingsEvents(server)

	requestHeaders := []HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.test"},
		{Name: ":path", Value: "/upload"},
		{Name: "content-length", Value: "5"},
	}
	stream1, err := client.OpenStream(requestHeaders, false)
	if err != nil || stream1 != 1 {
		t.Fatalf("OpenStream=%d,%v", stream1, err)
	}
	stream3, err := client.OpenStream([]HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/other"},
	}, true)
	if err != nil || stream3 != 3 {
		t.Fatalf("second OpenStream=%d,%v", stream3, err)
	}
	if n, err := client.SendData(stream1, []byte("hello"), true); n != 5 || err != nil {
		t.Fatalf("SendData=%d,%v", n, err)
	}
	pumpSessions(t, client, server)

	events := drainEvents(server)
	if !hasEvent(events, EventHeaders, stream1) || !hasEvent(events, EventData, stream1) || !hasEvent(events, EventStreamEnd, stream1) ||
		!hasEvent(events, EventHeaders, stream3) || !hasEvent(events, EventStreamEnd, stream3) {
		t.Fatalf("server events=%#v", events)
	}

	if err := server.SendHeaders(stream3, []HeaderField{{Name: ":status", Value: "204"}}, true); err != nil {
		t.Fatal(err)
	}
	if err := server.SendHeaders(stream1, []HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "5"}}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := server.SendData(stream1, []byte("world"), true); n != 5 || err != nil {
		t.Fatalf("server SendData=%d,%v", n, err)
	}
	pumpSessions(t, server, client)
	events = drainEvents(client)
	if !hasEvent(events, EventHeaders, stream1) || !hasEvent(events, EventData, stream1) || !hasEvent(events, EventStreamEnd, stream1) ||
		!hasEvent(events, EventHeaders, stream3) || !hasEvent(events, EventStreamEnd, stream3) {
		t.Fatalf("client events=%#v", events)
	}
	if state, _ := client.StreamState(stream1); state != StreamClosed {
		t.Fatalf("client stream1 state=%v", state)
	}
	if state, _ := server.StreamState(stream1); state != StreamClosed {
		t.Fatalf("server stream1 state=%v", state)
	}
}

func TestSessionFlowControlRoundTrip(t *testing.T) {
	limits := SessionLimits{InitialWindowSize: 32768, MaxQueuedOutputBytes: 1 << 20, MaxQueuedEventBytes: 1 << 20}
	client := mustSession(t, RoleClient, limits)
	server := mustSession(t, RoleServer, limits)
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	drainEvents(client)
	drainEvents(server)
	streamID, err := client.OpenStream([]HeaderField{
		{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("x"), 100000)
	offset := 0
	for offset < len(body) {
		n, sendErr := client.SendData(streamID, body[offset:], offset == len(body)-1)
		if n > 0 {
			offset += n
			pumpSessions(t, client, server)
			drainEvents(server)
			pumpSessions(t, server, client)
			drainEvents(client)
			continue
		}
		if !errors.Is(sendErr, ErrWouldBlock) {
			t.Fatalf("offset %d SendData=%d,%v", offset, n, sendErr)
		}
		pumpSessions(t, server, client)
	}
	if _, err := client.SendData(streamID, nil, true); err != nil {
		t.Fatal(err)
	}
	pumpSessions(t, client, server)
	if state, _ := server.StreamState(streamID); state != StreamHalfClosedRemote {
		t.Fatalf("server state=%v", state)
	}
}

func TestSessionInformationalPushAndContentLength(t *testing.T) {
	client := mustSession(t, RoleClient, SessionLimits{EnablePush: true})
	server := mustSession(t, RoleServer, SessionLimits{EnablePush: true})
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	drainEvents(client)
	drainEvents(server)
	streamID, err := client.OpenStream([]HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	pumpSessions(t, client, server)
	drainEvents(server)
	if err := server.SendHeaders(streamID, []HeaderField{{Name: ":status", Value: "103"}, {Name: "link", Value: "</a>"}}, false); err != nil {
		t.Fatal(err)
	}
	promisedID, err := server.PushPromise(streamID, []HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/asset"},
	})
	if err != nil || promisedID != 2 {
		t.Fatalf("PushPromise=%d,%v", promisedID, err)
	}
	if err := server.SendHeaders(promisedID, []HeaderField{{Name: ":status", Value: "204"}}, true); err != nil {
		t.Fatal(err)
	}
	if err := server.SendHeaders(streamID, []HeaderField{{Name: ":status", Value: "200"}, {Name: "content-length", Value: "2"}}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := server.SendData(streamID, []byte("ok"), true); n != 2 || err != nil {
		t.Fatalf("SendData=%d,%v", n, err)
	}
	pumpSessions(t, server, client)
	events := drainEvents(client)
	if !hasEvent(events, EventPushPromise, promisedID) || !hasEvent(events, EventHeaders, promisedID) || !hasEvent(events, EventHeaders, streamID) {
		t.Fatalf("events=%#v", events)
	}

	client = mustSession(t, RoleClient, SessionLimits{})
	server = mustSession(t, RoleServer, SessionLimits{})
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	drainEvents(client)
	drainEvents(server)
	streamID, err = client.OpenStream([]HeaderField{
		{Name: ":method", Value: "POST"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"},
		{Name: "content-length", Value: "2"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendData(streamID, []byte("x"), true); !errors.Is(err, ErrInvalidHeaders) {
		t.Fatalf("short Content-Length=%v", err)
	}
}

func TestSessionRejectsBadPrefaceAndHeaderState(t *testing.T) {
	server := mustSession(t, RoleServer, SessionLimits{})
	if n, err := server.Feed([]byte("X")); n != 0 || err == nil {
		t.Fatalf("bad preface=%d,%v", n, err)
	}
	var protocol *ProtocolError
	if !errors.As(server.Failed(), &protocol) || protocol.Code != ErrCodeProtocol {
		t.Fatalf("failed=%v", server.Failed())
	}

	client := mustSession(t, RoleClient, SessionLimits{})
	server = mustSession(t, RoleServer, SessionLimits{})
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	drainEvents(client)
	drainEvents(server)
	if _, err := client.OpenStream([]HeaderField{{Name: ":method", Value: "GET"}}, true); !errors.Is(err, ErrInvalidHeaders) {
		t.Fatalf("invalid request headers=%v", err)
	}
}

func TestSessionSettingsGoAwayAndControlEvents(t *testing.T) {
	client := mustSession(t, RoleClient, SessionLimits{MaxConcurrentStreams: 8, EnableExtendedConnect: true})
	server := mustSession(t, RoleServer, SessionLimits{MaxConcurrentStreams: 8, EnableExtendedConnect: true})
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	drainEvents(client)
	drainEvents(server)
	if err := client.UpdateSettings([]Setting{{ID: SettingMaxConcurrentStreams, Value: 2}, {ID: SettingInitialWindowSize, Value: 1024}, {ID: SettingMaxConcurrentStreams, Value: 4}}); err != nil {
		t.Fatal(err)
	}
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	if client.localSettingsOutstanding != 0 {
		t.Fatalf("outstanding=%d", client.localSettingsOutstanding)
	}
	id, err := client.OpenStream([]HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.GoAway(ErrCodeNo, []byte("done")); err != nil {
		t.Fatal(err)
	}
	pumpSessions(t, server, client)
	if _, err := client.OpenStream([]HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/2"}}, true); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("open after GOAWAY=%v", err)
	}
	if client.peerLastStream != server.lastProcessedPeerStream || id != 1 {
		t.Fatalf("goaway last=%d", client.peerLastStream)
	}
	var ping [8]byte
	ping[0] = 7
	if err := client.Ping(ping); err != nil {
		t.Fatal(err)
	}
	pumpSessions(t, client, server)
	pumpSessions(t, server, client)
	if !hasEvent(drainEvents(client), EventPingAck, 0) {
		t.Fatal("missing ping ack")
	}
}

func TestSessionOutputAndEventLimits(t *testing.T) {
	if _, err := NewSession(Role(99), SessionLimits{}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("role error=%v", err)
	}
	client := mustSession(t, RoleClient, SessionLimits{MaxQueuedOutputBytes: 256})
	if _, err := client.OpenStream([]HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"},
	}, true); !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("output limit=%v", err)
	}
	if err := client.ConsumeOutput(len(client.Output()) + 1); err == nil {
		t.Fatal("oversized ConsumeOutput succeeded")
	}
}

func TestValidateHeaderSectionCorpus(t *testing.T) {
	validRequest := []HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/"}}
	validConnect := []HeaderField{{Name: ":method", Value: "CONNECT"}, {Name: ":authority", Value: "example.test:443"}}
	validExtended := []HeaderField{{Name: ":method", Value: "CONNECT"}, {Name: ":protocol", Value: "websocket"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: "example.test"}, {Name: ":path", Value: "/chat"}}
	for _, test := range []struct {
		name     string
		role     Role
		out      bool
		trailer  bool
		headers  []HeaderField
		extended bool
		valid    bool
	}{
		{"request", RoleClient, true, false, validRequest, false, true},
		{"connect", RoleClient, true, false, validConnect, false, true},
		{"extended", RoleClient, true, false, validExtended, true, true},
		{"extended-disabled", RoleClient, true, false, validExtended, false, false},
		{"response", RoleServer, true, false, []HeaderField{{Name: ":status", Value: "200"}}, false, true},
		{"101", RoleServer, true, false, []HeaderField{{Name: ":status", Value: "101"}}, false, false},
		{"pseudo-after", RoleClient, true, false, []HeaderField{{Name: "x", Value: "y"}, {Name: ":method", Value: "GET"}}, false, false},
		{"connection", RoleClient, true, false, append(append([]HeaderField(nil), validRequest...), HeaderField{Name: "connection", Value: "close"}), false, false},
		{"trailer", RoleClient, true, true, []HeaderField{{Name: "x", Value: "y"}}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateHeaderSection(test.role, test.out, test.trailer, test.headers, test.extended)
			if (err == nil) != test.valid {
				t.Fatalf("error=%v valid=%t", err, test.valid)
			}
		})
	}
}

func mustSession(t *testing.T, role Role, limits SessionLimits) *Session {
	t.Helper()
	session, err := NewSession(role, limits)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func pumpSessions(t *testing.T, source, destination *Session) {
	t.Helper()
	for len(source.Output()) != 0 {
		wire := append([]byte(nil), source.Output()...)
		consumed, err := destination.Feed(wire)
		if err != nil {
			t.Fatalf("Feed %v -> %v consumed %d/%d: %v", source.Role(), destination.Role(), consumed, len(wire), err)
		}
		if consumed != len(wire) {
			t.Fatalf("Feed consumed %d/%d", consumed, len(wire))
		}
		if err := source.ConsumeOutput(consumed); err != nil {
			t.Fatal(err)
		}
	}
}

func drainEvents(session *Session) []Event {
	var events []Event
	for {
		event, ok := session.NextEvent()
		if !ok {
			return events
		}
		events = append(events, event)
	}
}

func drainSettingsEvents(session *Session) {
	for _, event := range drainEvents(session) {
		if event.Type != EventSettings && event.Type != EventSettingsAck {
			panic("unexpected event")
		}
	}
}

func hasEvent(events []Event, typ EventType, streamID uint32) bool {
	for _, event := range events {
		if event.Type == typ && event.StreamID == streamID {
			return true
		}
	}
	return false
}
