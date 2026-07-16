package http2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"golang.org/x/net/http2/hpack"
)

// Role identifies which side of an HTTP/2 connection a Session implements.
type Role uint8

const (
	RoleClient Role = iota + 1
	RoleServer
)

// StreamState is the RFC 9113 stream lifecycle state.
type StreamState uint8

const (
	StreamIdle StreamState = iota
	StreamReservedLocal
	StreamReservedRemote
	StreamOpen
	StreamHalfClosedLocal
	StreamHalfClosedRemote
	StreamClosed
)

// EventType identifies one committed connection or stream event.
type EventType uint8

const (
	EventSettings EventType = iota + 1
	EventSettingsAck
	EventHeaders
	EventData
	EventStreamEnd
	EventStreamReset
	EventPing
	EventPingAck
	EventGoAway
	EventPriority
	EventPriorityUpdate
	EventPushPromise
	EventWindowUpdate
	EventUnknown
)

// Event is one bounded, retained Session event. Headers and Data are owned by
// the event and remain valid until the caller discards it.
type Event struct {
	Type            EventType
	StreamID        uint32
	EndStream       bool
	Trailer         bool
	Headers         []HeaderField
	Data            []byte
	Settings        []Setting
	ErrorCode       ErrorCode
	LastStreamID    uint32
	WindowIncrement uint32
	Priority        PriorityParam
	Ping            [8]byte
}

// SessionLimits bounds connection-wide retained state. Zero values select
// conservative finite defaults.
type SessionLimits struct {
	Frame                 Limits
	Header                HeaderLimits
	MaxConcurrentStreams  uint32
	MaxStreams            uint32
	MaxClosedStreams      uint32
	MaxQueuedOutputBytes  uint32
	MaxQueuedEventBytes   uint32
	MaxQueuedEvents       uint32
	InitialWindowSize     uint32
	MaxControlFrames      uint32
	EnablePush            bool
	EnableExtendedConnect bool
}

const (
	defaultSessionConcurrentStreams = 100
	defaultSessionMaxStreams        = 1024
	defaultSessionClosedStreams     = 256
	defaultSessionOutputBytes       = 4 << 20
	defaultSessionEventBytes        = 4 << 20
	defaultSessionEvents            = 4096
	defaultInitialWindowSize        = 65535
	defaultMaxControlFrames         = 4096
)

func (limits SessionLimits) normalized() SessionLimits {
	limits.Frame = limits.Frame.normalized()
	limits.Header = limits.Header.normalized()
	if limits.MaxConcurrentStreams == 0 {
		limits.MaxConcurrentStreams = defaultSessionConcurrentStreams
	}
	if limits.MaxStreams == 0 {
		limits.MaxStreams = defaultSessionMaxStreams
	}
	if limits.MaxStreams < limits.MaxConcurrentStreams {
		limits.MaxStreams = limits.MaxConcurrentStreams
	}
	if limits.MaxClosedStreams == 0 {
		limits.MaxClosedStreams = defaultSessionClosedStreams
	}
	if limits.MaxClosedStreams > limits.MaxStreams {
		limits.MaxClosedStreams = limits.MaxStreams
	}
	if limits.MaxQueuedOutputBytes == 0 {
		limits.MaxQueuedOutputBytes = defaultSessionOutputBytes
	}
	if limits.MaxQueuedEventBytes == 0 {
		limits.MaxQueuedEventBytes = defaultSessionEventBytes
	}
	if limits.MaxQueuedEvents == 0 {
		limits.MaxQueuedEvents = defaultSessionEvents
	}
	if limits.InitialWindowSize == 0 {
		limits.InitialWindowSize = defaultInitialWindowSize
	}
	if limits.InitialWindowSize > math.MaxInt32 {
		limits.InitialWindowSize = math.MaxInt32
	}
	if limits.MaxControlFrames == 0 {
		limits.MaxControlFrames = defaultMaxControlFrames
	}
	return limits
}

var (
	ErrInvalidRole       = errors.New("wagohttp/http2: invalid session role")
	ErrSessionClosed     = errors.New("wagohttp/http2: session is closed")
	ErrSessionFailed     = errors.New("wagohttp/http2: session failed")
	ErrWouldBlock        = errors.New("wagohttp/http2: operation would block")
	ErrStreamNotFound    = errors.New("wagohttp/http2: stream not found")
	ErrStreamState       = errors.New("wagohttp/http2: invalid stream state")
	ErrStreamIDExhausted = errors.New("wagohttp/http2: stream identifier exhausted")
	ErrStreamLimit       = errors.New("wagohttp/http2: stream resource limit")
	ErrOutputLimit       = errors.New("wagohttp/http2: queued output limit")
	ErrEventLimit        = errors.New("wagohttp/http2: queued event limit")
	ErrInvalidHeaders    = errors.New("wagohttp/http2: malformed field section")
)

// ProtocolError reports an HTTP/2 connection or stream error. StreamID zero
// denotes a connection error and Code is suitable for GOAWAY or RST_STREAM.
type ProtocolError struct {
	StreamID uint32
	Code     ErrorCode
	Reason   string
}

func (err *ProtocolError) Error() string {
	scope := "connection"
	if err.StreamID != 0 {
		scope = "stream " + strconv.FormatUint(uint64(err.StreamID), 10)
	}
	if err.Reason == "" {
		return "wagohttp/http2: " + scope + " error " + strconv.FormatUint(uint64(err.Code), 10)
	}
	return "wagohttp/http2: " + scope + " error: " + err.Reason
}

func (err *ProtocolError) Unwrap() error { return ErrSessionFailed }

type sessionStream struct {
	id               uint32
	state            StreamState
	local            bool
	sendWindow       int64
	recvWindow       int64
	sentHeaders      bool
	receivedHeaders  bool
	sentTrailers     bool
	receivedTrailers bool
	method           string
	receiveNoBody    bool
	sendNoBody       bool
	receiveHasLength bool
	receiveLength    uint64
	receiveBody      uint64
	sendHasLength    bool
	sendLength       uint64
	sendBody         uint64
}

type sessionAppendWriter struct {
	target *[]byte
	limit  int
	err    error
}

func (writer *sessionAppendWriter) Write(src []byte) (int, error) {
	if writer.err != nil {
		return 0, writer.err
	}
	if writer.target == nil || len(src) > writer.limit-len(*writer.target) {
		writer.err = ErrOutputLimit
		return 0, writer.err
	}
	*writer.target = append(*writer.target, src...)
	return len(src), nil
}

// Session is a bounded, event-driven HTTP/2 connection engine. It performs no
// I/O itself: callers feed received bytes, drain output bytes, and send stream
// operations. This makes it usable with wago-org/net, ordinary net.Conn values,
// deterministic tests, or another non-TLS byte transport.
type Session struct {
	role          Role
	limits        SessionLimits
	caps          SessionLimits
	parser        Parser
	decoder       *HeaderDecoder
	encoder       *hpack.Encoder
	encoderWriter sessionAppendWriter

	streams                 map[uint32]*sessionStream
	closedStreams           []uint32
	nextLocalStream         uint32
	lastPeerStream          uint32
	lastProcessedPeerStream uint32
	activeLocal             uint32
	activePeer              uint32

	peerMaxConcurrent        uint32
	peerMaxFrame             uint32
	localHeaderTable         uint32
	peerInitialWindow        int64
	connectionSendWindow     int64
	connectionRecvWindow     int64
	peerEnablePush           bool
	peerEnableConnect        bool
	peerNoPriorities         bool
	localSettingsOutstanding uint32
	controlFrames            uint32

	started             bool
	closed              bool
	failed              error
	peerInitialSettings bool
	goAwayReceived      bool
	goAwaySent          bool
	peerLastStream      uint32
	prefacePos          uint8

	output                   []byte
	events                   []Event
	eventBytes               uint32
	headerBlock              []byte
	currentHeaders           []HeaderField
	currentHeaderStream      uint32
	currentHeaderEventStream uint32
	currentHeaderEnd         bool
	currentHeaderTrailer     bool
	currentHeaderPush        bool
	currentFrame             FrameHeader
	pendingSettings          []Setting
	extensionPayload         []byte
}

// NewSession creates and starts a client or server session. Client output begins
// with ClientPreface and SETTINGS; server output begins with SETTINGS.
func NewSession(role Role, limits SessionLimits) (*Session, error) {
	if role != RoleClient && role != RoleServer {
		return nil, ErrInvalidRole
	}
	limits = limits.normalized()
	session := &Session{
		role:                 role,
		limits:               limits,
		caps:                 limits,
		streams:              make(map[uint32]*sessionStream, minSessionInt(int(limits.MaxStreams), 128)),
		peerMaxConcurrent:    math.MaxUint32,
		peerMaxFrame:         defaultMaxFrameSize,
		localHeaderTable:     limits.Header.MaxDynamicTableBytes,
		peerInitialWindow:    defaultInitialWindowSize,
		connectionSendWindow: defaultInitialWindowSize,
		connectionRecvWindow: int64(limits.InitialWindowSize),
		peerEnablePush:       true,
	}
	if role == RoleClient {
		session.nextLocalStream = 1
	} else {
		session.nextLocalStream = 2
	}
	session.encoderWriter.limit = int(limits.Header.MaxHeaderListBytes)
	session.encoder = hpack.NewEncoder(&session.encoderWriter)
	session.encoder.SetMaxDynamicTableSizeLimit(defaultMaxDynamicTableBytes)
	session.decoder = NewHeaderDecoder(limits.Header, session.onDecodedHeader)
	callbacks := Callbacks{
		FrameBegin:     session.onFrameBegin,
		Data:           session.onData,
		HeaderBlock:    session.onHeaderBlock,
		HeaderBlockEnd: session.onHeaderBlockEnd,
		Priority:       session.onPriority,
		RSTStream:      session.onRSTStream,
		Setting:        session.onSetting,
		SettingsEnd:    session.onSettingsEnd,
		PushPromise:    session.onPushPromise,
		Ping:           session.onPing,
		GoAway:         session.onGoAway,
		WindowUpdate:   session.onWindowUpdate,
		Unknown:        session.onUnknown,
		FrameComplete:  session.onFrameComplete,
	}
	session.parser = NewParser(&callbacks, limits.Frame)
	if err := session.start(); err != nil {
		return nil, err
	}
	return session, nil
}

func (session *Session) start() error {
	if session.started {
		return nil
	}
	session.started = true
	if session.role == RoleClient {
		if !session.reserveOutput(len(ClientPreface)) {
			return ErrOutputLimit
		}
		session.output = append(session.output, ClientPreface...)
	}
	settings := []Setting{
		{ID: SettingHeaderTableSize, Value: session.limits.Header.MaxDynamicTableBytes},
		{ID: SettingEnablePush, Value: boolSetting(session.limits.EnablePush)},
		{ID: SettingMaxConcurrentStreams, Value: session.limits.MaxConcurrentStreams},
		{ID: SettingInitialWindowSize, Value: session.limits.InitialWindowSize},
		{ID: SettingMaxFrameSize, Value: session.limits.Frame.MaxFrameSize},
		{ID: SettingMaxHeaderListSize, Value: clampUint64To32(session.limits.Header.MaxHeaderListBytes)},
		{ID: SettingEnableConnectProtocol, Value: boolSetting(session.limits.EnableExtendedConnect)},
		{ID: SettingNoRFC7540Priorities, Value: 1},
	}
	if session.role == RoleServer {
		// SETTINGS_ENABLE_PUSH is only sent by clients.
		settings = append(settings[:1], settings[2:]...)
	}
	if err := session.appendSettings(settings, false); err != nil {
		return err
	}
	session.localSettingsOutstanding++
	return nil
}

// Role reports the local endpoint role.
func (session *Session) Role() Role { return session.role }

// Failed reports the sticky connection failure, if any.
func (session *Session) Failed() error { return session.failed }

// Feed consumes received connection bytes and queues committed events and
// protocol-control output. On a stream error it queues RST_STREAM and continues;
// connection errors are sticky and queue GOAWAY when output capacity permits.
func (session *Session) Feed(src []byte) (int, error) {
	if session.closed {
		return 0, ErrSessionClosed
	}
	if session.failed != nil {
		return 0, session.failed
	}
	consumed := 0
	if session.role == RoleServer && session.prefacePos < uint8(len(ClientPreface)) {
		for consumed < len(src) && session.prefacePos < uint8(len(ClientPreface)) {
			if src[consumed] != ClientPreface[session.prefacePos] {
				return consumed, session.connectionError(ErrCodeProtocol, "invalid client connection preface")
			}
			session.prefacePos++
			consumed++
		}
		if consumed == len(src) || session.prefacePos < uint8(len(ClientPreface)) {
			return consumed, nil
		}
	}
	for consumed < len(src) {
		n, _, code := session.parser.ParseOne(src[consumed:])
		consumed += n
		if code != CodeNone {
			return consumed, session.connectionError(frameCodeToError(code), code.String())
		}
		if session.failed != nil {
			return consumed, session.failed
		}
		if n == 0 {
			break
		}
	}
	return consumed, nil
}

// Finish reports a truncated preface, frame, or continuation sequence.
func (session *Session) Finish() error {
	if session.closed {
		return ErrSessionClosed
	}
	if session.failed != nil {
		return session.failed
	}
	if session.role == RoleServer && session.prefacePos != uint8(len(ClientPreface)) {
		return session.connectionError(ErrCodeProtocol, "truncated client connection preface")
	}
	if code := session.parser.Finish(); code != CodeNone {
		return session.connectionError(frameCodeToError(code), code.String())
	}
	return nil
}

// Output returns the currently queued wire bytes. The slice is invalidated by
// the next mutating Session call.
func (session *Session) Output() []byte { return session.output }

// ConsumeOutput releases n bytes previously returned by Output.
func (session *Session) ConsumeOutput(n int) error {
	if n < 0 || n > len(session.output) {
		return io.ErrShortBuffer
	}
	copy(session.output, session.output[n:])
	session.output = session.output[:len(session.output)-n]
	return nil
}

// NextEvent removes the oldest committed event.
func (session *Session) NextEvent() (Event, bool) {
	if len(session.events) == 0 {
		return Event{}, false
	}
	event := session.events[0]
	copy(session.events, session.events[1:])
	session.events = session.events[:len(session.events)-1]
	session.eventBytes -= eventRetainedBytes(event)
	return event, true
}

// StreamState reports the current state of a retained stream.
func (session *Session) StreamState(streamID uint32) (StreamState, bool) {
	stream, ok := session.streams[streamID]
	if !ok {
		return StreamIdle, false
	}
	return stream.state, true
}

// OpenStream allocates a locally initiated stream and sends its initial field
// section. Client streams are odd and server push streams are even.
func (session *Session) OpenStream(headers []HeaderField, endStream bool) (uint32, error) {
	if session.goAwayReceived || session.goAwaySent {
		return 0, ErrSessionClosed
	}
	if session.role != RoleClient {
		return 0, ErrStreamState
	}
	if session.nextLocalStream == 0 || session.nextLocalStream > math.MaxInt32 {
		return 0, ErrStreamIDExhausted
	}
	if session.activeLocal >= session.peerMaxConcurrent {
		return 0, ErrWouldBlock
	}
	if uint32(len(session.streams)) >= session.limits.MaxStreams {
		return 0, ErrStreamLimit
	}
	streamID := session.nextLocalStream
	session.nextLocalStream += 2
	stream := &sessionStream{id: streamID, state: StreamOpen, local: true, sendWindow: session.peerInitialWindow, recvWindow: int64(session.limits.InitialWindowSize)}
	session.streams[streamID] = stream
	session.activeLocal++
	if err := session.SendHeaders(streamID, headers, endStream); err != nil {
		delete(session.streams, streamID)
		session.activeLocal--
		return 0, err
	}
	return streamID, nil
}

// PushPromise reserves a server-initiated stream and sends its promised request
// field section on parentStreamID. Server push must be enabled by both peers.
func (session *Session) PushPromise(parentStreamID uint32, headers []HeaderField) (uint32, error) {
	if session.role != RoleServer || !session.limits.EnablePush || !session.peerEnablePush {
		return 0, ErrStreamState
	}
	parent := session.streams[parentStreamID]
	if parent == nil || !canSend(parent.state) {
		return 0, ErrStreamNotFound
	}
	if session.nextLocalStream == 0 || session.nextLocalStream > math.MaxInt32 {
		return 0, ErrStreamIDExhausted
	}
	if session.activeLocal >= session.peerMaxConcurrent {
		return 0, ErrWouldBlock
	}
	if uint32(len(session.streams)) >= session.limits.MaxStreams {
		return 0, ErrStreamLimit
	}
	if err := validateHeaderSection(RoleClient, true, false, headers, session.limits.EnableExtendedConnect); err != nil {
		return 0, err
	}
	promisedID := session.nextLocalStream
	session.nextLocalStream += 2
	stream := &sessionStream{id: promisedID, state: StreamReservedLocal, local: true, sendWindow: session.peerInitialWindow, recvWindow: int64(session.limits.InitialWindowSize)}
	metadata := inspectHeaderSection(headers)
	stream.method = metadata.method
	if err := session.encodeAndAppendHeaders(FramePushPromise, parentStreamID, promisedID, headers, false); err != nil {
		return 0, err
	}
	session.streams[promisedID] = stream
	session.activeLocal++
	return promisedID, nil
}

// PriorityUpdate sends the RFC 9218 PRIORITY_UPDATE extension frame.
func (session *Session) PriorityUpdate(streamID uint32, value []byte) error {
	if len(value) > int(session.peerMaxFrame)-4 {
		return ErrOutputLimit
	}
	payload := make([]byte, 4, 4+len(value))
	binary.BigEndian.PutUint32(payload, streamID&math.MaxInt32)
	payload = append(payload, value...)
	return session.appendFrame(FrameHeader{Type: 0x10}, payload)
}

// SendHeaders sends initial headers or trailers on an existing stream.
func (session *Session) SendHeaders(streamID uint32, headers []HeaderField, endStream bool) error {
	if err := session.ready(); err != nil {
		return err
	}
	stream, ok := session.streams[streamID]
	if !ok {
		return ErrStreamNotFound
	}
	if !canSend(stream.state) || stream.sentTrailers {
		return ErrStreamState
	}
	trailer := stream.sentHeaders
	if trailer && !endStream {
		return ErrInvalidHeaders
	}
	if err := validateHeaderSection(session.role, true, trailer, headers, session.limits.EnableExtendedConnect); err != nil {
		return err
	}
	metadata := inspectHeaderSection(headers)
	if metadata.informational {
		if trailer || endStream {
			return ErrInvalidHeaders
		}
	} else if metadata.noBody && !endStream {
		return ErrInvalidHeaders
	}
	projectedNoBody := metadata.noBody || stream.method == "HEAD" && metadata.response
	if endStream && metadata.hasContentLength && !projectedNoBody && metadata.contentLength != stream.sendBody {
		return ErrInvalidHeaders
	}
	if err := session.encodeAndAppendHeaders(FrameHeaders, streamID, 0, headers, endStream); err != nil {
		return err
	}
	if stream.state == StreamReservedLocal {
		stream.state = StreamHalfClosedRemote
	}
	if !metadata.informational {
		stream.sentHeaders = true
		stream.method = firstNonEmpty(stream.method, metadata.method)
		stream.sendNoBody = projectedNoBody
		stream.sendHasLength = metadata.hasContentLength
		stream.sendLength = metadata.contentLength
	}
	if trailer {
		stream.sentTrailers = true
	}
	if endStream {
		if stream.sendHasLength && !stream.sendNoBody && stream.sendLength != stream.sendBody {
			return ErrInvalidHeaders
		}
		session.localEnd(stream)
	}
	return nil
}

// SendData sends as much DATA as current stream, connection, frame, and output
// limits permit. A zero-length endStream call emits an empty terminal DATA.
func (session *Session) SendData(streamID uint32, src []byte, endStream bool) (int, error) {
	if err := session.ready(); err != nil {
		return 0, err
	}
	stream, ok := session.streams[streamID]
	if !ok {
		return 0, ErrStreamNotFound
	}
	if !stream.sentHeaders || !canSend(stream.state) || stream.sentTrailers || stream.sendNoBody {
		return 0, ErrStreamState
	}
	if stream.sendHasLength && uint64(len(src)) > stream.sendLength-stream.sendBody {
		return 0, ErrInvalidHeaders
	}
	if len(src) == 0 {
		if !endStream {
			return 0, nil
		}
		if stream.sendHasLength && stream.sendLength != stream.sendBody {
			return 0, ErrInvalidHeaders
		}
		if err := session.appendFrame(FrameHeader{Type: FrameData, Flags: FlagEndStream, StreamID: streamID}, nil); err != nil {
			return 0, err
		}
		session.localEnd(stream)
		return 0, nil
	}
	available := minSessionInt(len(src), int(session.peerMaxFrame))
	if int64(available) > session.connectionSendWindow {
		available = int(session.connectionSendWindow)
	}
	if int64(available) > stream.sendWindow {
		available = int(stream.sendWindow)
	}
	if available <= 0 {
		return 0, ErrWouldBlock
	}
	flags := Flags(0)
	if available == len(src) && endStream {
		if stream.sendHasLength && stream.sendBody+uint64(available) != stream.sendLength {
			return 0, ErrInvalidHeaders
		}
		flags = FlagEndStream
	}
	if err := session.appendFrame(FrameHeader{Type: FrameData, Flags: flags, StreamID: streamID}, src[:available]); err != nil {
		return 0, err
	}
	session.connectionSendWindow -= int64(available)
	stream.sendWindow -= int64(available)
	stream.sendBody += uint64(available)
	if flags.Has(FlagEndStream) {
		session.localEnd(stream)
	}
	return available, nil
}

// ResetStream sends RST_STREAM and closes retained stream state.
func (session *Session) ResetStream(streamID uint32, code ErrorCode) error {
	stream, ok := session.streams[streamID]
	if !ok || stream.state == StreamIdle {
		return ErrStreamNotFound
	}
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	if err := session.appendFrame(FrameHeader{Type: FrameRSTStream, StreamID: streamID}, payload[:]); err != nil {
		return err
	}
	session.closeStream(stream)
	return nil
}

// UpdateSettings sends and applies a bounded local SETTINGS update. Values may
// reduce configured limits but cannot exceed the Session's original bounds.
func (session *Session) UpdateSettings(settings []Setting) error {
	if err := session.ready(); err != nil {
		return err
	}
	if len(settings) > int(session.peerMaxFrame)/6 {
		return ErrInvalidHeaders
	}
	if !session.reserveOutput(9 + len(settings)*6) {
		return ErrOutputLimit
	}
	newHeaderTable := session.localHeaderTable
	newEnablePush := session.limits.EnablePush
	newMaxConcurrent := session.limits.MaxConcurrentStreams
	newInitialWindow := session.limits.InitialWindowSize
	newMaxFrame := session.parser.limits.MaxFrameSize
	newEnableConnect := session.limits.EnableExtendedConnect
	for _, setting := range settings {
		if !validSetting(setting) {
			return ErrInvalidHeaders
		}
		switch setting.ID {
		case SettingHeaderTableSize:
			if setting.Value > session.caps.Header.MaxDynamicTableBytes || session.decoder.active {
				return ErrInvalidHeaders
			}
			newHeaderTable = setting.Value
		case SettingEnablePush:
			if session.role != RoleClient {
				return ErrInvalidHeaders
			}
			newEnablePush = setting.Value != 0
		case SettingMaxConcurrentStreams:
			if setting.Value > session.caps.MaxStreams {
				return ErrInvalidHeaders
			}
			newMaxConcurrent = setting.Value
		case SettingInitialWindowSize:
			newInitialWindow = setting.Value
		case SettingMaxFrameSize:
			if setting.Value > session.caps.Frame.MaxFrameSize {
				return ErrInvalidHeaders
			}
			newMaxFrame = setting.Value
		case SettingMaxHeaderListSize:
			if uint64(setting.Value) > session.caps.Header.MaxHeaderListBytes {
				return ErrInvalidHeaders
			}
		case SettingEnableConnectProtocol:
			newEnableConnect = setting.Value != 0
		case SettingNoRFC7540Priorities:
			if setting.Value != 1 {
				return ErrInvalidHeaders
			}
		}
	}
	windowChange := int64(newInitialWindow) - int64(session.limits.InitialWindowSize)
	for _, stream := range session.streams {
		updated := stream.recvWindow + windowChange
		if updated > math.MaxInt32 || updated < math.MinInt32 {
			return ErrStreamState
		}
	}
	if err := session.decoder.SetAllowedDynamicTableBytes(newHeaderTable); err != nil {
		return err
	}
	if err := session.appendSettings(settings, false); err != nil {
		return err
	}
	session.localHeaderTable = newHeaderTable
	session.limits.EnablePush = newEnablePush
	session.limits.MaxConcurrentStreams = newMaxConcurrent
	session.limits.InitialWindowSize = newInitialWindow
	session.parser.limits.MaxFrameSize = newMaxFrame
	session.limits.EnableExtendedConnect = newEnableConnect
	for _, stream := range session.streams {
		stream.recvWindow += windowChange
	}
	session.localSettingsOutstanding++
	return nil
}

// Ping sends one non-ACK PING frame.
func (session *Session) Ping(data [8]byte) error {
	return session.appendFrame(FrameHeader{Type: FramePing}, data[:])
}

// GoAway starts graceful local shutdown.
func (session *Session) GoAway(code ErrorCode, debug []byte) error {
	if session.goAwaySent {
		return nil
	}
	if len(debug) > int(session.peerMaxFrame)-8 {
		debug = debug[:int(session.peerMaxFrame)-8]
	}
	payload := make([]byte, 8, 8+len(debug))
	binary.BigEndian.PutUint32(payload[:4], session.lastProcessedPeerStream)
	binary.BigEndian.PutUint32(payload[4:8], uint32(code))
	payload = append(payload, debug...)
	if err := session.appendFrame(FrameHeader{Type: FrameGoAway}, payload); err != nil {
		return err
	}
	session.goAwaySent = true
	return nil
}

// Close releases retained stream, event, compression, and output state.
func (session *Session) Close() {
	if session == nil || session.closed {
		return
	}
	session.closed = true
	clear(session.streams)
	session.events = nil
	session.output = nil
	session.headerBlock = nil
	session.currentHeaders = nil
}

func (session *Session) ready() error {
	if session == nil || session.closed {
		return ErrSessionClosed
	}
	if session.failed != nil {
		return session.failed
	}
	return nil
}

func (session *Session) onFrameBegin(header FrameHeader) {
	if session.failed != nil {
		return
	}
	session.currentFrame = header
	if !session.peerInitialSettings {
		if header.Type != FrameSettings || header.Flags.Has(FlagACK) {
			session.connectionError(ErrCodeProtocol, "peer did not begin with SETTINGS")
			return
		}
	}
	if header.Type == FrameHeaders {
		if err := session.beginInboundHeaders(header.StreamID, header.Flags.Has(FlagEndStream)); err != nil {
			session.handleProtocolError(err)
		}
	}
	if header.Type == FrameData {
		stream, ok := session.streams[header.StreamID]
		if !ok || !canReceive(stream.state) || !stream.receivedHeaders || stream.receivedTrailers || stream.receiveNoBody {
			session.streamError(header.StreamID, ErrCodeStreamClosed, "DATA in invalid stream state")
			return
		}
		length := int64(header.Length)
		if length > session.connectionRecvWindow {
			session.connectionError(ErrCodeFlowControl, "connection receive window exceeded")
			return
		}
		if length > stream.recvWindow {
			session.streamError(header.StreamID, ErrCodeFlowControl, "stream receive window exceeded")
			return
		}
		session.connectionRecvWindow -= length
		stream.recvWindow -= length
	}
	if header.Type == 0x10 {
		session.extensionPayload = session.extensionPayload[:0]
	}
}

func (session *Session) onData(streamID uint32, data []byte, endStream bool) {
	if session.failed != nil || len(data) == 0 {
		return
	}
	stream := session.streams[streamID]
	if stream == nil {
		return
	}
	if stream.receiveHasLength && uint64(len(data)) > stream.receiveLength-stream.receiveBody {
		session.streamError(streamID, ErrCodeProtocol, "received DATA exceeds Content-Length")
		return
	}
	stream.receiveBody += uint64(len(data))
	copyData := append([]byte(nil), data...)
	if err := session.queueEvent(Event{Type: EventData, StreamID: streamID, EndStream: endStream, Data: copyData}); err != nil {
		session.connectionError(ErrCodeEnhanceYourCalm, err.Error())
	}
}

func (session *Session) beginInboundHeaders(streamID uint32, endStream bool) error {
	stream, ok := session.streams[streamID]
	if !ok {
		if !session.peerMayOpen(streamID) {
			return &ProtocolError{Code: ErrCodeProtocol, Reason: "peer opened an invalid stream identifier"}
		}
		if session.activePeer >= session.limits.MaxConcurrentStreams {
			return &ProtocolError{StreamID: streamID, Code: ErrCodeRefusedStream, Reason: "peer concurrent stream limit exceeded"}
		}
		if uint32(len(session.streams)) >= session.limits.MaxStreams {
			return &ProtocolError{StreamID: streamID, Code: ErrCodeRefusedStream, Reason: "stream storage exhausted"}
		}
		stream = &sessionStream{id: streamID, state: StreamOpen, local: false, sendWindow: session.peerInitialWindow, recvWindow: int64(session.limits.InitialWindowSize)}
		session.streams[streamID] = stream
		session.activePeer++
		session.lastPeerStream = streamID
	} else if stream.state == StreamClosed {
		return &ProtocolError{Code: ErrCodeStreamClosed, Reason: "HEADERS on a closed stream"}
	} else if !canReceive(stream.state) || stream.receivedTrailers {
		return &ProtocolError{StreamID: streamID, Code: ErrCodeStreamClosed, Reason: "HEADERS in invalid stream state"}
	}
	if stream.receivedHeaders && !endStream {
		return &ProtocolError{StreamID: streamID, Code: ErrCodeProtocol, Reason: "trailing HEADERS omitted END_STREAM"}
	}
	if stream.state == StreamReservedRemote {
		stream.state = StreamHalfClosedLocal
	}
	session.currentHeaders = session.currentHeaders[:0]
	session.currentHeaderStream = streamID
	session.currentHeaderEventStream = streamID
	session.currentHeaderEnd = endStream
	session.currentHeaderTrailer = stream.receivedHeaders
	session.currentHeaderPush = false
	return session.decoder.BeginBlock()
}

func (session *Session) onHeaderBlock(streamID uint32, fragment []byte) {
	if session.failed != nil || streamID != session.currentHeaderStream {
		return
	}
	if _, err := session.decoder.Write(fragment); err != nil {
		session.connectionError(ErrCodeCompression, err.Error())
	}
}

func (session *Session) onDecodedHeader(field HeaderField) {
	if session.failed != nil {
		return
	}
	session.currentHeaders = append(session.currentHeaders, field)
}

func (session *Session) onHeaderBlockEnd(streamID uint32, endStream bool) {
	if session.failed != nil || streamID != session.currentHeaderStream {
		return
	}
	if err := session.decoder.EndBlock(); err != nil {
		session.connectionError(ErrCodeCompression, err.Error())
		return
	}
	eventStreamID := session.currentHeaderEventStream
	stream := session.streams[eventStreamID]
	if stream == nil {
		return
	}
	validationRole := session.role
	if session.currentHeaderPush {
		validationRole = RoleServer
	}
	if err := validateHeaderSection(validationRole, false, session.currentHeaderTrailer, session.currentHeaders, session.limits.EnableExtendedConnect); err != nil {
		session.streamError(eventStreamID, ErrCodeProtocol, err.Error())
		return
	}
	metadata := inspectHeaderSection(session.currentHeaders)
	headers := append([]HeaderField(nil), session.currentHeaders...)
	eventType := EventHeaders
	if session.currentHeaderPush {
		eventType = EventPushPromise
	}
	event := Event{Type: eventType, StreamID: eventStreamID, EndStream: endStream, Trailer: session.currentHeaderTrailer, Headers: headers}
	if err := session.queueEvent(event); err != nil {
		session.connectionError(ErrCodeEnhanceYourCalm, err.Error())
		return
	}
	if session.currentHeaderPush {
		stream.method = metadata.method
	} else if !metadata.informational {
		stream.receivedHeaders = true
		stream.method = firstNonEmpty(stream.method, metadata.method)
		stream.receiveNoBody = metadata.noBody || stream.method == "HEAD" && metadata.response
		stream.receiveHasLength = metadata.hasContentLength
		stream.receiveLength = metadata.contentLength
		if session.currentHeaderTrailer {
			stream.receivedTrailers = true
		}
		if endStream {
			session.remoteEnd(stream)
		}
	} else if endStream {
		session.streamError(eventStreamID, ErrCodeProtocol, "informational response ended stream")
	}
	session.currentHeaderStream = 0
	session.currentHeaderEventStream = 0
	session.currentHeaderPush = false
	session.currentHeaders = session.currentHeaders[:0]
}

func (session *Session) onPriority(streamID uint32, priority PriorityParam) {
	if session.failed == nil {
		_ = session.queueEvent(Event{Type: EventPriority, StreamID: streamID, Priority: priority})
	}
}

func (session *Session) onRSTStream(streamID uint32, code ErrorCode) {
	stream := session.streams[streamID]
	if stream == nil {
		if streamID > session.lastPeerStream {
			session.connectionError(ErrCodeProtocol, "RST_STREAM on idle stream")
		}
		return
	}
	session.closeStream(stream)
	_ = session.queueEvent(Event{Type: EventStreamReset, StreamID: streamID, ErrorCode: code})
}

func (session *Session) onSetting(setting Setting) {
	session.pendingSettings = append(session.pendingSettings, setting)
}

func (session *Session) onSettingsEnd(ack bool) {
	if session.failed != nil {
		return
	}
	if ack {
		if session.localSettingsOutstanding == 0 {
			session.connectionError(ErrCodeProtocol, "unexpected SETTINGS acknowledgement")
			return
		}
		session.localSettingsOutstanding--
		_ = session.queueEvent(Event{Type: EventSettingsAck})
		return
	}
	for _, setting := range session.pendingSettings {
		if err := session.applyPeerSetting(setting); err != nil {
			session.handleProtocolError(err)
			return
		}
	}
	settings := append([]Setting(nil), session.pendingSettings...)
	session.pendingSettings = session.pendingSettings[:0]
	session.peerInitialSettings = true
	if err := session.appendSettings(nil, true); err != nil {
		session.connectionError(ErrCodeInternal, err.Error())
		return
	}
	_ = session.queueEvent(Event{Type: EventSettings, Settings: settings})
}

func (session *Session) applyPeerSetting(setting Setting) error {
	switch setting.ID {
	case SettingHeaderTableSize:
		session.encoder.SetMaxDynamicTableSizeLimit(setting.Value)
		session.encoder.SetMaxDynamicTableSize(setting.Value)
	case SettingEnablePush:
		if session.role == RoleClient {
			return &ProtocolError{Code: ErrCodeProtocol, Reason: "server sent SETTINGS_ENABLE_PUSH"}
		}
		session.peerEnablePush = setting.Value != 0
	case SettingMaxConcurrentStreams:
		session.peerMaxConcurrent = setting.Value
	case SettingInitialWindowSize:
		change := int64(setting.Value) - session.peerInitialWindow
		for _, stream := range session.streams {
			if stream.state == StreamClosed {
				continue
			}
			updated := stream.sendWindow + change
			if updated > math.MaxInt32 || updated < math.MinInt32 {
				return &ProtocolError{Code: ErrCodeFlowControl, Reason: "SETTINGS_INITIAL_WINDOW_SIZE overflow"}
			}
			stream.sendWindow = updated
		}
		session.peerInitialWindow = int64(setting.Value)
	case SettingMaxFrameSize:
		session.peerMaxFrame = setting.Value
	case SettingEnableConnectProtocol:
		session.peerEnableConnect = setting.Value != 0
	case SettingNoRFC7540Priorities:
		if session.peerInitialSettings && session.peerNoPriorities != (setting.Value != 0) {
			return &ProtocolError{Code: ErrCodeProtocol, Reason: "SETTINGS_NO_RFC7540_PRIORITIES changed after initial SETTINGS"}
		}
		session.peerNoPriorities = setting.Value != 0
	}
	return nil
}

func (session *Session) onPushPromise(streamID, promisedStreamID uint32) {
	if session.failed != nil {
		return
	}
	parent := session.streams[streamID]
	if session.role != RoleClient || !session.limits.EnablePush || !session.peerEnablePush || promisedStreamID == 0 || promisedStreamID&1 != 0 || promisedStreamID <= session.lastPeerStream || parent == nil || !canReceive(parent.state) {
		session.connectionError(ErrCodeProtocol, "invalid PUSH_PROMISE")
		return
	}
	if session.activePeer >= session.limits.MaxConcurrentStreams || uint32(len(session.streams)) >= session.limits.MaxStreams {
		session.streamError(promisedStreamID, ErrCodeRefusedStream, "push stream storage exhausted")
		return
	}
	session.streams[promisedStreamID] = &sessionStream{id: promisedStreamID, state: StreamReservedRemote, local: false, sendWindow: session.peerInitialWindow, recvWindow: int64(session.limits.InitialWindowSize)}
	session.activePeer++
	session.lastPeerStream = promisedStreamID
	session.currentHeaders = session.currentHeaders[:0]
	session.currentHeaderStream = streamID
	session.currentHeaderEventStream = promisedStreamID
	session.currentHeaderEnd = false
	session.currentHeaderTrailer = false
	session.currentHeaderPush = true
	if err := session.decoder.BeginBlock(); err != nil {
		session.connectionError(ErrCodeCompression, err.Error())
	}
}

func (session *Session) onPing(data [8]byte, ack bool) {
	if session.failed != nil {
		return
	}
	if ack {
		_ = session.queueEvent(Event{Type: EventPingAck, Ping: data})
		return
	}
	if err := session.appendFrame(FrameHeader{Type: FramePing, Flags: FlagACK}, data[:]); err != nil {
		session.connectionError(ErrCodeInternal, err.Error())
		return
	}
	_ = session.queueEvent(Event{Type: EventPing, Ping: data})
}

func (session *Session) onGoAway(lastStreamID uint32, code ErrorCode) {
	if session.goAwayReceived && lastStreamID > session.peerLastStream {
		session.connectionError(ErrCodeProtocol, "GOAWAY last stream identifier increased")
		return
	}
	session.goAwayReceived = true
	session.peerLastStream = lastStreamID
	for _, stream := range session.streams {
		if stream.local && stream.id > lastStreamID && stream.state != StreamClosed {
			session.closeStream(stream)
			_ = session.queueEvent(Event{Type: EventStreamReset, StreamID: stream.id, ErrorCode: ErrCodeRefusedStream})
		}
	}
	_ = session.queueEvent(Event{Type: EventGoAway, LastStreamID: lastStreamID, ErrorCode: code})
}

func (session *Session) onWindowUpdate(streamID, increment uint32) {
	if session.failed != nil {
		return
	}
	if streamID == 0 {
		if session.connectionSendWindow > math.MaxInt32-int64(increment) {
			session.connectionError(ErrCodeFlowControl, "connection send window overflow")
			return
		}
		session.connectionSendWindow += int64(increment)
		_ = session.queueEvent(Event{Type: EventWindowUpdate, WindowIncrement: increment})
		return
	}
	stream := session.streams[streamID]
	if stream == nil {
		if streamID > session.lastPeerStream {
			session.connectionError(ErrCodeProtocol, "WINDOW_UPDATE on idle stream")
		}
		return
	}
	if stream.state == StreamClosed {
		return
	}
	if stream.sendWindow > math.MaxInt32-int64(increment) {
		session.streamError(streamID, ErrCodeFlowControl, "stream send window overflow")
		return
	}
	stream.sendWindow += int64(increment)
	_ = session.queueEvent(Event{Type: EventWindowUpdate, StreamID: streamID, WindowIncrement: increment})
}

func (session *Session) onUnknown(header FrameHeader, fragment []byte) {
	if header.Type == 0x10 {
		if len(fragment) > int(session.limits.Frame.MaxFrameSize)-len(session.extensionPayload) {
			session.connectionError(ErrCodeFrameSize, "PRIORITY_UPDATE too large")
			return
		}
		session.extensionPayload = append(session.extensionPayload, fragment...)
	}
}

func (session *Session) onFrameComplete(header FrameHeader) {
	if session.failed != nil {
		return
	}
	if header.Type != FrameData && header.Type != FrameHeaders && header.Type != FrameContinuation {
		session.controlFrames++
		if session.controlFrames > session.limits.MaxControlFrames {
			session.connectionError(ErrCodeEnhanceYourCalm, "control frame quota exceeded")
			return
		}
	}
	if header.Type == FrameData {
		stream := session.streams[header.StreamID]
		if stream == nil {
			return
		}
		if session.connectionRecvWindow <= int64(session.limits.InitialWindowSize)/2 {
			increment := int64(session.limits.InitialWindowSize) - session.connectionRecvWindow
			if increment > 0 {
				_ = session.appendWindowUpdate(0, uint32(increment))
				session.connectionRecvWindow += increment
			}
		}
		if stream.recvWindow <= int64(session.limits.InitialWindowSize)/2 {
			increment := int64(session.limits.InitialWindowSize) - stream.recvWindow
			if increment > 0 {
				_ = session.appendWindowUpdate(header.StreamID, uint32(increment))
				stream.recvWindow += increment
			}
		}
		if header.Flags.Has(FlagEndStream) {
			session.remoteEnd(stream)
		}
	}
	if header.Type == 0x10 {
		if header.StreamID != 0 || len(session.extensionPayload) < 4 {
			session.connectionError(ErrCodeProtocol, "invalid PRIORITY_UPDATE")
			return
		}
		streamID := binary.BigEndian.Uint32(session.extensionPayload[:4]) & math.MaxInt32
		value := append([]byte(nil), session.extensionPayload[4:]...)
		_ = session.queueEvent(Event{Type: EventPriorityUpdate, StreamID: streamID, Data: value})
	}
}

func (session *Session) peerMayOpen(streamID uint32) bool {
	if streamID == 0 || streamID <= session.lastPeerStream {
		return false
	}
	if session.role == RoleServer {
		return streamID&1 == 1
	}
	return streamID&1 == 0 && session.limits.EnablePush
}

func (session *Session) localEnd(stream *sessionStream) {
	switch stream.state {
	case StreamOpen:
		stream.state = StreamHalfClosedLocal
	case StreamHalfClosedRemote:
		session.closeStream(stream)
	case StreamReservedLocal:
		stream.state = StreamHalfClosedRemote
	}
}

func (session *Session) remoteEnd(stream *sessionStream) {
	if stream.receiveHasLength && !stream.receiveNoBody && stream.receiveLength != stream.receiveBody {
		session.streamError(stream.id, ErrCodeProtocol, "received body does not match Content-Length")
		return
	}
	before := stream.state
	switch stream.state {
	case StreamOpen:
		stream.state = StreamHalfClosedRemote
	case StreamHalfClosedLocal:
		session.closeStream(stream)
	case StreamReservedRemote:
		stream.state = StreamHalfClosedLocal
	}
	if before != StreamClosed {
		_ = session.queueEvent(Event{Type: EventStreamEnd, StreamID: stream.id})
		session.lastProcessedPeerStream = maxSessionUint32(session.lastProcessedPeerStream, stream.id)
	}
}

func (session *Session) closeStream(stream *sessionStream) {
	if stream == nil || stream.state == StreamClosed {
		return
	}
	stream.state = StreamClosed
	if stream.local {
		if session.activeLocal != 0 {
			session.activeLocal--
		}
	} else if session.activePeer != 0 {
		session.activePeer--
	}
	session.closedStreams = append(session.closedStreams, stream.id)
	for uint32(len(session.closedStreams)) > session.limits.MaxClosedStreams {
		oldest := session.closedStreams[0]
		copy(session.closedStreams, session.closedStreams[1:])
		session.closedStreams = session.closedStreams[:len(session.closedStreams)-1]
		if closed := session.streams[oldest]; closed != nil && closed.state == StreamClosed {
			delete(session.streams, oldest)
		}
	}
}

func (session *Session) streamError(streamID uint32, code ErrorCode, reason string) error {
	stream := session.streams[streamID]
	if stream != nil {
		session.closeStream(stream)
	}
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	_ = session.appendFrame(FrameHeader{Type: FrameRSTStream, StreamID: streamID}, payload[:])
	_ = session.queueEvent(Event{Type: EventStreamReset, StreamID: streamID, ErrorCode: code})
	return &ProtocolError{StreamID: streamID, Code: code, Reason: reason}
}

func (session *Session) connectionError(code ErrorCode, reason string) error {
	if session.failed != nil {
		return session.failed
	}
	err := &ProtocolError{Code: code, Reason: reason}
	session.failed = err
	session.goAwaySent = true
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[:4], session.lastProcessedPeerStream)
	binary.BigEndian.PutUint32(payload[4:], uint32(code))
	_ = session.appendFrameEvenFailed(FrameHeader{Type: FrameGoAway}, payload)
	return err
}

func (session *Session) handleProtocolError(err error) {
	var protocol *ProtocolError
	if !errors.As(err, &protocol) {
		session.connectionError(ErrCodeInternal, err.Error())
		return
	}
	if protocol.StreamID == 0 {
		session.connectionError(protocol.Code, protocol.Reason)
	} else {
		session.streamError(protocol.StreamID, protocol.Code, protocol.Reason)
	}
}

func (session *Session) queueEvent(event Event) error {
	bytes := eventRetainedBytes(event)
	if uint32(len(session.events)) >= session.limits.MaxQueuedEvents || bytes > session.limits.MaxQueuedEventBytes-session.eventBytes {
		return ErrEventLimit
	}
	session.events = append(session.events, event)
	session.eventBytes += bytes
	return nil
}

func eventRetainedBytes(event Event) uint32 {
	total := uint64(len(event.Data)) + uint64(len(event.Settings))*6
	for _, field := range event.Headers {
		total += uint64(len(field.Name)) + uint64(len(field.Value)) + 32
	}
	if total > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(total)
}

func (session *Session) reserveOutput(bytes int) bool {
	return bytes >= 0 && bytes <= int(session.limits.MaxQueuedOutputBytes)-len(session.output)
}

func (session *Session) appendFrame(header FrameHeader, payload []byte) error {
	if err := session.ready(); err != nil {
		return err
	}
	return session.appendFrameEvenFailed(header, payload)
}

func (session *Session) appendFrameEvenFailed(header FrameHeader, payload []byte) error {
	needed := 9 + len(payload)
	if !session.reserveOutput(needed) {
		return ErrOutputLimit
	}
	var code Code
	session.output, code = AppendFrame(session.output, header, payload)
	if code != CodeNone {
		return fmt.Errorf("%w: %s", ErrInvalidHeaders, code.String())
	}
	return nil
}

func (session *Session) appendSettings(settings []Setting, ack bool) error {
	if ack {
		return session.appendFrameEvenFailed(FrameHeader{Type: FrameSettings, Flags: FlagACK}, nil)
	}
	payload := make([]byte, len(settings)*6)
	for index, setting := range settings {
		binary.BigEndian.PutUint16(payload[index*6:index*6+2], uint16(setting.ID))
		binary.BigEndian.PutUint32(payload[index*6+2:index*6+6], setting.Value)
	}
	return session.appendFrameEvenFailed(FrameHeader{Type: FrameSettings}, payload)
}

func (session *Session) appendWindowUpdate(streamID, increment uint32) error {
	if increment == 0 || increment > math.MaxInt32 {
		return ErrStreamState
	}
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], increment)
	return session.appendFrameEvenFailed(FrameHeader{Type: FrameWindowUpdate, StreamID: streamID}, payload[:])
}

func (session *Session) encodeAndAppendHeaders(frameType FrameType, streamID, promisedID uint32, headers []HeaderField, endStream bool) error {
	maxBlock := int(session.limits.Header.MaxHeaderListBytes)
	frameCount := maxBlock/int(session.peerMaxFrame) + 2
	if !session.reserveOutput(maxBlock + frameCount*9 + 4) {
		return ErrWouldBlock
	}
	session.headerBlock = session.headerBlock[:0]
	session.encoderWriter.target = &session.headerBlock
	session.encoderWriter.limit = maxBlock
	session.encoderWriter.err = nil
	for _, field := range headers {
		if err := session.encoder.WriteField(hpack.HeaderField{Name: field.Name, Value: field.Value, Sensitive: field.Sensitive}); err != nil {
			return err
		}
	}
	block := session.headerBlock
	first := true
	for first || len(block) != 0 {
		prefix := 0
		if first && frameType == FramePushPromise {
			prefix = 4
		}
		available := int(session.peerMaxFrame) - prefix
		count := minSessionInt(len(block), available)
		flags := Flags(0)
		if count == len(block) {
			flags |= FlagEndHeaders
		}
		if first && endStream && frameType == FrameHeaders {
			flags |= FlagEndStream
		}
		typ := FrameContinuation
		payload := block[:count]
		if first {
			typ = frameType
			if frameType == FramePushPromise {
				prefixed := make([]byte, 4, 4+count)
				binary.BigEndian.PutUint32(prefixed[:4], promisedID)
				payload = append(prefixed, payload...)
			}
		}
		if err := session.appendFrame(FrameHeader{Type: typ, Flags: flags, StreamID: streamID}, payload); err != nil {
			return err
		}
		block = block[count:]
		first = false
	}
	return nil
}

type headerSectionMetadata struct {
	request          bool
	response         bool
	informational    bool
	noBody           bool
	invalid          bool
	method           string
	status           string
	hasContentLength bool
	contentLength    uint64
}

func inspectHeaderSection(headers []HeaderField) headerSectionMetadata {
	var metadata headerSectionMetadata
	for _, field := range headers {
		switch field.Name {
		case ":method":
			metadata.request = true
			metadata.method = field.Value
		case ":status":
			metadata.response = true
			metadata.status = field.Value
			if len(field.Value) == 3 && field.Value[0] == '1' {
				metadata.informational = true
			}
			metadata.noBody = field.Value == "204" || field.Value == "304"
		case "content-length":
			if metadata.hasContentLength {
				metadata.invalid = true
				continue
			}
			value, ok := parseSessionContentLength(field.Value)
			if !ok {
				metadata.invalid = true
				continue
			}
			metadata.hasContentLength = true
			metadata.contentLength = value
		}
	}
	if metadata.status == "204" && metadata.hasContentLength {
		metadata.invalid = true
	}
	return metadata
}

func parseSessionContentLength(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	var parsed uint64
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
		digit := uint64(value[index] - '0')
		if parsed > (math.MaxUint64-digit)/10 {
			return 0, false
		}
		parsed = parsed*10 + digit
	}
	return parsed, true
}

func firstNonEmpty(current, next string) string {
	if current != "" {
		return current
	}
	return next
}

func validateHeaderSection(role Role, outbound, trailer bool, headers []HeaderField, extendedConnect bool) error {
	if len(headers) == 0 {
		return ErrInvalidHeaders
	}
	metadata := inspectHeaderSection(headers)
	if metadata.invalid {
		return ErrInvalidHeaders
	}
	pseudoDone := false
	seen := make(map[string]bool, 5)
	var method, scheme, authority, path, protocol, status string
	for _, field := range headers {
		if !validSessionHeaderName(field.Name) || !validSessionHeaderValue(field.Value) {
			return ErrInvalidHeaders
		}
		pseudo := field.Name[0] == ':'
		if pseudo {
			if trailer || pseudoDone || seen[field.Name] {
				return ErrInvalidHeaders
			}
			seen[field.Name] = true
			switch field.Name {
			case ":method":
				method = field.Value
			case ":scheme":
				scheme = field.Value
			case ":authority":
				authority = field.Value
			case ":path":
				path = field.Value
			case ":protocol":
				protocol = field.Value
			case ":status":
				status = field.Value
			default:
				return ErrInvalidHeaders
			}
		} else {
			pseudoDone = true
			if isConnectionSpecific(field.Name, field.Value) {
				return ErrInvalidHeaders
			}
		}
	}
	if trailer {
		return nil
	}
	requestSection := role == RoleClient && outbound || role == RoleServer && !outbound
	if requestSection {
		if method == "" || status != "" {
			return ErrInvalidHeaders
		}
		if method == "CONNECT" {
			if protocol == "" {
				if authority == "" || scheme != "" || path != "" {
					return ErrInvalidHeaders
				}
			} else if !extendedConnect || authority == "" || scheme == "" || path == "" {
				return ErrInvalidHeaders
			}
		} else if scheme == "" || path == "" || protocol != "" {
			return ErrInvalidHeaders
		}
		return nil
	}
	if status == "" || len(status) != 3 || method != "" || scheme != "" || authority != "" || path != "" || protocol != "" {
		return ErrInvalidHeaders
	}
	for _, b := range []byte(status) {
		if b < '0' || b > '9' {
			return ErrInvalidHeaders
		}
	}
	if status == "101" {
		return ErrInvalidHeaders
	}
	return nil
}

func validSessionHeaderName(name string) bool {
	if name == "" {
		return false
	}
	start := 0
	if name[0] == ':' {
		start = 1
		if len(name) == 1 {
			return false
		}
	}
	for index := start; index < len(name); index++ {
		b := name[index]
		if b >= 'a' && b <= 'z' || b >= '0' && b <= '9' {
			continue
		}
		switch b {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func validSessionHeaderValue(value string) bool {
	if value != "" && (value[0] == ' ' || value[0] == '\t' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		return false
	}
	for index := 0; index < len(value); index++ {
		b := value[index]
		if b != '\t' && b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

func isConnectionSpecific(name, value string) bool {
	switch name {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade":
		return true
	case "te":
		return value != "trailers"
	default:
		return false
	}
}

func canSend(state StreamState) bool {
	return state == StreamOpen || state == StreamHalfClosedRemote || state == StreamReservedLocal
}

func canReceive(state StreamState) bool {
	return state == StreamOpen || state == StreamHalfClosedLocal || state == StreamReservedRemote
}

func frameCodeToError(code Code) ErrorCode {
	switch code {
	case CodeFrameTooLarge, CodeInvalidFrameSize:
		return ErrCodeFrameSize
	case CodeInvalidSetting:
		return ErrCodeProtocol
	case CodeHeaderBlockTooLarge, CodeTooManyContinuations:
		return ErrCodeEnhanceYourCalm
	default:
		return ErrCodeProtocol
	}
}

func boolSetting(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func clampUint64To32(value uint64) uint32 {
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}

func minSessionInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxSessionUint32(left, right uint32) uint32 {
	if left > right {
		return left
	}
	return right
}

var _ io.Writer = (*sessionAppendWriter)(nil)
