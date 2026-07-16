package http2

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestSessionAdditionalErrorAndControlCoverage(t *testing.T) {
	connection := &ProtocolError{Code: ErrCodeProtocol}
	stream := &ProtocolError{StreamID: 3, Code: ErrCodeCancel, Reason: "reason"}
	if connection.Error() == "" || stream.Error() == "" || !errors.Is(stream, ErrSessionFailed) {
		t.Fatal("protocol error methods")
	}

	var target []byte
	writer := &sessionAppendWriter{target: &target, limit: 2}
	if n, err := writer.Write([]byte("ok")); n != 2 || err != nil {
		t.Fatalf("writer=%d,%v", n, err)
	}
	if _, err := writer.Write([]byte("x")); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("limit=%v", err)
	}
	if _, err := writer.Write(nil); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("sticky=%v", err)
	}
	if _, err := (&sessionAppendWriter{limit: 1}).Write([]byte("x")); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("nil target=%v", err)
	}

	client := mustSession(t, RoleClient, SessionLimits{})
	if client.Role() != RoleClient {
		t.Fatal("role")
	}
	if _, err := client.Feed([]byte{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := client.Finish(); !errors.Is(err, ErrSessionFailed) {
		t.Fatalf("finish=%v", err)
	}
	client.Close()
	if err := client.Finish(); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed finish=%v", err)
	}

	for _, test := range []struct {
		code Code
		want ErrorCode
	}{
		{CodeFrameTooLarge, ErrCodeFrameSize}, {CodeInvalidFrameSize, ErrCodeFrameSize},
		{CodeInvalidSetting, ErrCodeProtocol}, {CodeHeaderBlockTooLarge, ErrCodeEnhanceYourCalm},
		{CodeTooManyContinuations, ErrCodeEnhanceYourCalm}, {CodeNone, ErrCodeProtocol},
	} {
		if got := frameCodeToError(test.code); got != test.want {
			t.Fatalf("frame code %v=%v", test.code, got)
		}
	}

	s := mustSession(t, RoleServer, SessionLimits{MaxQueuedOutputBytes: 1 << 20, MaxQueuedEventBytes: 1 << 20})
	s.streams[1] = &sessionStream{id: 1, state: StreamOpen, local: false, sendWindow: 10, recvWindow: 10}
	s.activePeer = 1
	if err := s.streamError(1, ErrCodeCancel, "cancel"); err == nil {
		t.Fatal("streamError nil")
	}
	if event, ok := s.NextEvent(); !ok || event.Type != EventStreamReset {
		t.Fatalf("reset event=%+v", event)
	}
	s.onPriority(3, PriorityParam{StreamDependency: 1, Weight: 2})
	s.onUnknown(FrameHeader{Type: FrameType(0x10)}, []byte{0, 0, 0, 3, 'u', '=', '1'})
	s.onFrameComplete(FrameHeader{Type: FrameType(0x10)})
	if len(drainEvents(s)) != 2 {
		t.Fatal("control events")
	}

	for _, err := range []error{errors.New("plain"), &ProtocolError{Code: ErrCodeFlowControl, Reason: "connection"}, &ProtocolError{StreamID: 5, Code: ErrCodeCancel, Reason: "stream"}} {
		target := mustSession(t, RoleServer, SessionLimits{})
		if protocol := new(ProtocolError); errors.As(err, &protocol) && protocol.StreamID != 0 {
			target.streams[protocol.StreamID] = &sessionStream{id: protocol.StreamID, state: StreamOpen}
		}
		target.handleProtocolError(err)
	}
}

func TestSessionAdditionalSettingsAndWindowCoverage(t *testing.T) {
	client := mustSession(t, RoleClient, SessionLimits{MaxStreams: 32, MaxConcurrentStreams: 16, MaxQueuedOutputBytes: 1 << 20, EnableExtendedConnect: true})
	settings := []Setting{
		{ID: SettingHeaderTableSize, Value: 128}, {ID: SettingEnablePush, Value: 1},
		{ID: SettingMaxConcurrentStreams, Value: 4}, {ID: SettingInitialWindowSize, Value: 2048},
		{ID: SettingMaxFrameSize, Value: defaultMaxFrameSize}, {ID: SettingMaxHeaderListSize, Value: 1024},
		{ID: SettingEnableConnectProtocol, Value: 1}, {ID: SettingNoRFC7540Priorities, Value: 1},
	}
	if err := client.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]Setting{
		{{ID: SettingEnablePush, Value: 2}},
		{{ID: SettingInitialWindowSize, Value: math.MaxUint32}},
		{{ID: SettingMaxFrameSize, Value: 1}},
		{{ID: SettingNoRFC7540Priorities, Value: 0}},
		{{ID: SettingMaxConcurrentStreams, Value: 33}},
		{{ID: SettingMaxHeaderListSize, Value: math.MaxUint32}},
	} {
		if err := client.UpdateSettings(invalid); err == nil {
			t.Fatalf("invalid settings accepted: %+v", invalid)
		}
	}
	server := mustSession(t, RoleServer, SessionLimits{})
	if err := server.UpdateSettings([]Setting{{ID: SettingEnablePush, Value: 0}}); err == nil {
		t.Fatal("server ENABLE_PUSH accepted")
	}

	window := mustSession(t, RoleClient, SessionLimits{MaxQueuedEventBytes: 1 << 20})
	window.connectionSendWindow = math.MaxInt32
	window.onWindowUpdate(0, 1)
	if window.Failed() == nil {
		t.Fatal("connection window overflow accepted")
	}
	window = mustSession(t, RoleClient, SessionLimits{MaxQueuedEventBytes: 1 << 20})
	window.streams[1] = &sessionStream{id: 1, state: StreamOpen, sendWindow: 10}
	window.onWindowUpdate(1, 2)
	if event, ok := window.NextEvent(); !ok || event.Type != EventWindowUpdate {
		t.Fatalf("window event=%+v", event)
	}
	window = mustSession(t, RoleClient, SessionLimits{MaxQueuedEventBytes: 1 << 20})
	window.streams[1] = &sessionStream{id: 1, state: StreamOpen, sendWindow: math.MaxInt32}
	window.onWindowUpdate(1, 1)
	if event, ok := window.NextEvent(); !ok || event.Type != EventStreamReset {
		t.Fatalf("overflow reset=%+v", event)
	}
	window = mustSession(t, RoleClient, SessionLimits{})
	window.onWindowUpdate(99, 1)
	if window.Failed() == nil {
		t.Fatal("idle window update accepted")
	}
}

func TestSessionAdditionalInboundCallbackCoverage(t *testing.T) {
	valid := func() *Session {
		s := mustSession(t, RoleServer, SessionLimits{InitialWindowSize: 16, MaxQueuedEventBytes: 1024, MaxQueuedEvents: 8})
		s.peerInitialSettings = true
		s.streams[1] = &sessionStream{id: 1, state: StreamOpen, receivedHeaders: true, recvWindow: 16, sendWindow: 16}
		s.connectionRecvWindow = 16
		return s
	}
	s := valid()
	s.onFrameBegin(FrameHeader{Type: FrameData, StreamID: 1, Length: 2})
	s.onData(1, []byte("ok"), false)
	if event, ok := s.NextEvent(); !ok || event.Type != EventData || string(event.Data) != "ok" {
		t.Fatalf("data event=%+v", event)
	}
	s.onData(99, []byte("ignored"), false)
	s.onData(1, nil, false)

	for _, mutate := range []func(*Session){
		func(s *Session) { s.connectionRecvWindow = 1 },
		func(s *Session) { s.streams[1].recvWindow = 1 },
		func(s *Session) { s.streams[1].receivedHeaders = false },
		func(s *Session) { s.streams[1].receivedTrailers = true },
		func(s *Session) { s.streams[1].receiveNoBody = true },
	} {
		target := valid()
		mutate(target)
		target.onFrameBegin(FrameHeader{Type: FrameData, StreamID: 1, Length: 2})
	}
	target := valid()
	target.streams[1].receiveHasLength = true
	target.streams[1].receiveLength = 1
	target.onData(1, []byte("xx"), false)
	if event, ok := target.NextEvent(); !ok || event.Type != EventStreamReset {
		t.Fatalf("length reset=%+v", event)
	}
	target = mustSession(t, RoleServer, SessionLimits{MaxQueuedEvents: 1, MaxQueuedEventBytes: 1})
	target.streams[1] = &sessionStream{id: 1, state: StreamOpen, receivedHeaders: true}
	target.onData(1, []byte("xx"), false)
	if target.Failed() == nil {
		t.Fatal("event limit did not fail session")
	}

	target = valid()
	target.onRSTStream(1, ErrCodeCancel)
	if event, ok := target.NextEvent(); !ok || event.Type != EventStreamReset {
		t.Fatalf("RST event=%+v", event)
	}
	target = valid()
	target.lastPeerStream = 1
	target.onRSTStream(1, ErrCodeCancel)
	target.onRSTStream(3, ErrCodeCancel)
	if target.Failed() == nil {
		t.Fatal("idle RST accepted")
	}
	target = valid()
	target.localSettingsOutstanding = 0
	target.onSettingsEnd(true)
	if target.Failed() == nil {
		t.Fatal("unexpected settings ACK accepted")
	}

	target = valid()
	target.onFrameBegin(FrameHeader{Type: FrameType(0x10)})
	if len(target.extensionPayload) != 0 {
		t.Fatal("extension payload not reset")
	}
	target.failed = errors.New("failed")
	target.onFrameBegin(FrameHeader{Type: FrameData, StreamID: 1})
	target.onData(1, []byte("x"), false)
	target.onPriority(1, PriorityParam{})

	quota := mustSession(t, RoleClient, SessionLimits{MaxControlFrames: 1})
	quota.onFrameComplete(FrameHeader{Type: FramePing})
	quota.onFrameComplete(FrameHeader{Type: FrameData})
	quota.onFrameComplete(FrameHeader{Type: FramePing})
	if quota.Failed() != nil {
		t.Fatalf("control quota was not reset: %v", quota.Failed())
	}
	quota.onFrameComplete(FrameHeader{Type: FramePing})
	if quota.Failed() == nil {
		t.Fatal("consecutive control quota not enforced")
	}
}

func TestSessionAdditionalValidationAndLimitCoverage(t *testing.T) {
	for _, value := range []string{"ok", "", "bad\r", "bad\n", "bad\x00", string([]byte{0x7f})} {
		_ = validSessionHeaderValue(value)
	}
	for _, name := range []string{"x", "X", "", ":status", "bad space", string([]byte{0x80})} {
		_ = validSessionHeaderName(name)
	}
	for _, pair := range [][2]string{{"connection", "x"}, {"proxy-connection", "x"}, {"keep-alive", "x"}, {"transfer-encoding", "x"}, {"upgrade", "x"}, {"te", "trailers"}, {"te", "gzip"}, {"x", "y"}} {
		_ = isConnectionSpecific(pair[0], pair[1])
	}
	for _, value := range []string{"0", "01", "18446744073709551615", "18446744073709551616", "-1", "x"} {
		_, _ = parseSessionContentLength(value)
	}
	if got := eventRetainedBytes(Event{Data: make([]byte, 2), Settings: []Setting{{}}, Headers: []HeaderField{{Name: "x", Value: "y"}}}); got == 0 {
		t.Fatal("retained bytes")
	}
	if clampUint64To32(math.MaxUint64) != math.MaxUint32 || clampUint64To32(1) != 1 {
		t.Fatal("clamp")
	}

	limited := mustSession(t, RoleClient, SessionLimits{MaxQueuedEvents: 1, MaxQueuedEventBytes: 1})
	if err := limited.queueEvent(Event{Data: []byte("xx")}); !errors.Is(err, ErrEventLimit) {
		t.Fatalf("event bytes=%v", err)
	}
	if limited.reserveOutput(-1) {
		t.Fatal("negative output reserved")
	}
	limited.Close()
	if err := limited.appendFrame(FrameHeader{Type: FramePing}, make([]byte, 8)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed append=%v", err)
	}

	for _, bad := range [][]HeaderField{
		{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}, {Name: "content-length", Value: "1"}, {Name: "content-length", Value: "2"}},
		{{Name: ":method", Value: "CONNECT"}, {Name: ":authority", Value: ""}},
		{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/"}, {Name: "te", Value: "gzip"}},
	} {
		if err := validateHeaderSection(RoleClient, true, false, bad, false); err == nil {
			t.Fatalf("headers accepted: %+v", bad)
		}
	}
	if !strings.Contains((&ProtocolError{StreamID: 1, Code: 1}).Error(), "stream 1") {
		t.Fatal("stream scope")
	}
}
