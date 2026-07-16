// Package request provides bounded one-shot and persistent multiplexed HTTP/2
// clients over caller-owned or caller-dialed byte streams. TLS, ALPN, and
// deadline policy remain transport responsibilities.
package request

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strconv"

	h2 "github.com/wago-org/http/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	DefaultReadBufferBytes = 16 << 10
	defaultMaxRequestBody  = 65535
	defaultMaxResponseBody = 64 << 20
)

var (
	ErrInvalidRequest       = errors.New("wagohttp/http2/request: invalid request")
	ErrInvalidResponse      = errors.New("wagohttp/http2/request: invalid response")
	ErrInvalidStream        = errors.New("wagohttp/http2/request: invalid stream")
	ErrEmptyReadBuffer      = errors.New("wagohttp/http2/request: read buffer is empty")
	ErrReadBufferTooLarge   = errors.New("wagohttp/http2/request: read buffer size exceeds platform limits")
	ErrShortBuffer          = errors.New("wagohttp/http2/request: destination buffer is too small")
	ErrRequestBodyTooLarge  = errors.New("wagohttp/http2/request: request body exceeds the initial flow-control window")
	ErrResponseBodyTooLarge = errors.New("wagohttp/http2/request: response body too large")
	ErrMissingSettings      = errors.New("wagohttp/http2/request: peer did not begin with SETTINGS")
	ErrStreamReset          = errors.New("wagohttp/http2/request: stream reset")
	ErrGoAway               = errors.New("wagohttp/http2/request: connection received GOAWAY")
	ErrPushPromise          = errors.New("wagohttp/http2/request: server push is disabled")
)

// Header is one borrowed request header. HTTP/2 requires lowercase field names.
type Header struct {
	Name      []byte
	Value     []byte
	Sensitive bool
}

// Request is one HTTP/2 request. Pseudo-fields, Host, Content-Length, and
// connection-specific fields are writer-managed. BodyReader, BodyLength,
// HasBodyLength, Protocol, and Trailers are supported by Transport; the legacy
// one-shot Client accepts only Body and ordinary requests.
type Request struct {
	Method        []byte
	Scheme        []byte
	Authority     []byte
	Path          []byte
	Protocol      []byte
	Headers       []Header
	Body          []byte
	BodyReader    io.Reader
	BodyLength    int64
	HasBodyLength bool
	ReplayBody    func() (io.Reader, error)
	Trailers      []Header
}

// Stream is the connected transport used by Client.
type Stream interface {
	io.Reader
	io.Writer
}

// Response is final response metadata. Buffered aliases the DoBuffer scratch
// buffer and contains bytes read after stream 1 reached its message boundary.
type Response struct {
	Status           uint16
	ContentLength    uint64
	HasContentLength bool
	BodyBytes        uint64
	Buffered         []byte
}

// Callbacks receives decoded response fields and borrowed body fragments.
// Header values are delivered before the enclosing block is committed; callers
// should commit externally visible state only from HeadersComplete,
// TrailersComplete, or ResponseComplete.
type Callbacks struct {
	Header           func(field h2.HeaderField, trailer bool)
	HeadersComplete  func(response Response, informational bool)
	Body             func([]byte)
	TrailersComplete func()
	ResponseComplete func(Response)
}

// ValidationError describes a locally rejected request.
type ValidationError struct{ Reason string }

func (err *ValidationError) Error() string {
	if err.Reason == "" {
		return ErrInvalidRequest.Error()
	}
	return ErrInvalidRequest.Error() + ": " + err.Reason
}
func (err *ValidationError) Unwrap() error { return ErrInvalidRequest }

// FrameError wraps a strict HTTP/2 frame parser failure.
type FrameError struct{ Code h2.Code }

func (err *FrameError) Error() string { return ErrInvalidResponse.Error() + ": " + err.Code.String() }
func (err *FrameError) Unwrap() error { return ErrInvalidResponse }

// CompressionError wraps an HPACK decoder failure.
type CompressionError struct{ Err error }

func (err *CompressionError) Error() string {
	return ErrInvalidResponse.Error() + ": HPACK: " + err.Err.Error()
}
func (err *CompressionError) Unwrap() error { return ErrInvalidResponse }

// StreamError reports a peer reset. REFUSED_STREAM before response headers is
// safe for a replayable request to retry on another connection.
type StreamError struct {
	Code      h2.ErrorCode
	Retryable bool
}

func (err *StreamError) Error() string {
	return ErrStreamReset.Error() + ": " + strconv.FormatUint(uint64(err.Code), 10)
}
func (err *StreamError) Unwrap() error { return ErrStreamReset }

// GoAwayError reports a draining peer connection.
type GoAwayError struct {
	LastStreamID uint32
	Code         h2.ErrorCode
}

func (err *GoAwayError) Error() string {
	return ErrGoAway.Error() + ": last stream " + strconv.FormatUint(uint64(err.LastStreamID), 10)
}
func (err *GoAwayError) Unwrap() error { return ErrGoAway }

// Client bounds and executes one stream-1 exchange. A zero value is ready to
// use. Request bodies are bounded by both MaxRequestBodyBytes and the 65,535
// byte initial peer flow-control window because the request is written before
// response frames are read.
type Client struct {
	FrameLimits          h2.Limits
	HeaderLimits         h2.HeaderLimits
	ReadBufferBytes      uint32
	MaxRequestBodyBytes  uint64
	MaxResponseBodyBytes uint64
}

// Validate checks request syntax and finite body bounds.
func (client Client) Validate(request Request) error {
	if request.BodyReader != nil || request.HasBodyLength || request.ReplayBody != nil || len(request.Trailers) != 0 || len(request.Protocol) != 0 {
		return &ValidationError{Reason: "streaming bodies, trailers, and extended CONNECT require Transport"}
	}
	if !validMethod(request.Method) {
		return &ValidationError{Reason: "invalid :method"}
	}
	if !validScheme(request.Scheme) {
		return &ValidationError{Reason: "invalid :scheme"}
	}
	if len(request.Authority) == 0 || !validPseudoValue(request.Authority) {
		return &ValidationError{Reason: "invalid :authority"}
	}
	if len(request.Path) == 0 || !validPseudoValue(request.Path) {
		return &ValidationError{Reason: "invalid :path"}
	}
	maxField, maxList, maxHeaders := requestHeaderLimits(client.HeaderLimits)
	fieldCount := uint32(4)
	listSize := headerListEntrySize(len(":method"), len(request.Method)) +
		headerListEntrySize(len(":scheme"), len(request.Scheme)) +
		headerListEntrySize(len(":authority"), len(request.Authority)) +
		headerListEntrySize(len(":path"), len(request.Path))
	if uint64(len(request.Method)) > uint64(maxField) || uint64(len(request.Scheme)) > uint64(maxField) || uint64(len(request.Authority)) > uint64(maxField) || uint64(len(request.Path)) > uint64(maxField) {
		return &ValidationError{Reason: "pseudo-header field too large"}
	}
	for _, header := range request.Headers {
		if !validLowerName(header.Name) || !validValue(header.Value) || header.Name[0] == ':' {
			return &ValidationError{Reason: "invalid header field"}
		}
		if forbiddenRequestHeader(header.Name, header.Value) {
			return &ValidationError{Reason: "reserved or connection-specific header"}
		}
		if uint64(len(header.Name)) > uint64(maxField) || uint64(len(header.Value)) > uint64(maxField) {
			return &ValidationError{Reason: "header field too large"}
		}
		fieldCount++
		listSize += headerListEntrySize(len(header.Name), len(header.Value))
	}
	if len(request.Body) != 0 {
		fieldCount++
		listSize += headerListEntrySize(len("content-length"), decimalLength(uint64(len(request.Body))))
	}
	if fieldCount > maxHeaders || listSize > maxList {
		return &ValidationError{Reason: "header list too large"}
	}
	limit := client.MaxRequestBodyBytes
	if limit == 0 || limit > defaultMaxRequestBody {
		limit = defaultMaxRequestBody
	}
	if uint64(len(request.Body)) > limit {
		return ErrRequestBodyTooLarge
	}
	return nil
}

// Append appends the client connection preface, initial SETTINGS, and one
// complete stream-1 request. HPACK and DATA payloads are split at 16,384 bytes.
func (client Client) Append(dst []byte, request Request) ([]byte, error) {
	if err := client.Validate(request); err != nil {
		return dst, err
	}

	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	fields := []hpack.HeaderField{
		{Name: ":method", Value: string(request.Method)},
		{Name: ":scheme", Value: string(request.Scheme)},
		{Name: ":authority", Value: string(request.Authority)},
		{Name: ":path", Value: string(request.Path)},
	}
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			return dst, err
		}
	}
	for _, header := range request.Headers {
		if err := encoder.WriteField(hpack.HeaderField{Name: string(header.Name), Value: string(header.Value), Sensitive: header.Sensitive}); err != nil {
			return dst, err
		}
	}
	if len(request.Body) != 0 {
		if err := encoder.WriteField(hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(len(request.Body))}); err != nil {
			return dst, err
		}
	}

	dst = append(dst, h2.ClientPreface...)
	settings := make([]byte, 18)
	putSetting(settings[0:6], h2.SettingEnablePush, 0)
	putSetting(settings[6:12], h2.SettingHeaderTableSize, headerDynamicTableLimit(client.HeaderLimits))
	putSetting(settings[12:18], h2.SettingMaxHeaderListSize, headerListLimit(client.HeaderLimits))
	var code h2.Code
	dst, code = h2.AppendFrame(dst, h2.FrameHeader{Type: h2.FrameSettings}, settings)
	if code != h2.CodeNone {
		return dst, &FrameError{Code: code}
	}

	headerBlock := block.Bytes()
	first := true
	for first || len(headerBlock) != 0 {
		count := len(headerBlock)
		if count > 16<<10 {
			count = 16 << 10
		}
		flags := h2.Flags(0)
		if count == len(headerBlock) {
			flags |= h2.FlagEndHeaders
		}
		if first && len(request.Body) == 0 {
			flags |= h2.FlagEndStream
		}
		typ := h2.FrameContinuation
		if first {
			typ = h2.FrameHeaders
		}
		dst, code = h2.AppendFrame(dst, h2.FrameHeader{Type: typ, Flags: flags, StreamID: 1}, headerBlock[:count])
		if code != h2.CodeNone {
			return dst, &FrameError{Code: code}
		}
		headerBlock = headerBlock[count:]
		first = false
	}

	body := request.Body
	for len(body) != 0 {
		count := len(body)
		if count > 16<<10 {
			count = 16 << 10
		}
		flags := h2.Flags(0)
		if count == len(body) {
			flags = h2.FlagEndStream
		}
		dst, code = h2.AppendFrame(dst, h2.FrameHeader{Type: h2.FrameData, Flags: flags, StreamID: 1}, body[:count])
		if code != h2.CodeNone {
			return dst, &FrameError{Code: code}
		}
		body = body[count:]
	}
	return dst, nil
}

// Encode writes the complete connection preface and request to dst. A short
// destination is not modified.
func (client Client) Encode(dst []byte, request Request) (int, error) {
	wire, err := client.Append(nil, request)
	if err != nil {
		return 0, err
	}
	if len(dst) < len(wire) {
		return 0, ErrShortBuffer
	}
	return copy(dst, wire), nil
}

// Write writes one encoded connection preface and request, retrying short
// writes. The caller owns transport lifecycle and deadlines.
func (client Client) Write(writer io.Writer, request Request) error {
	if writer == nil {
		return ErrInvalidStream
	}
	wire, err := client.Append(nil, request)
	if err != nil {
		return err
	}
	return writeAll(writer, wire)
}

// Do allocates a finite read buffer and performs one exchange.
func (client Client) Do(stream Stream, request Request, callbacks *Callbacks) (Response, error) {
	size := client.ReadBufferBytes
	if size == 0 {
		size = DefaultReadBufferBytes
	}
	if uint64(size) > uint64(^uint(0)>>1) {
		return Response{}, ErrReadBufferTooLarge
	}
	return client.DoBuffer(stream, request, callbacks, make([]byte, int(size)))
}

// DoBuffer writes one request, processes connection control frames, streams the
// final response, acknowledges SETTINGS and PING, replenishes consumed DATA
// flow-control credit, and returns exactly at the stream-1 message boundary.
func (client Client) DoBuffer(stream Stream, request Request, callbacks *Callbacks, buffer []byte) (Response, error) {
	if stream == nil {
		return Response{}, ErrInvalidStream
	}
	if len(buffer) == 0 {
		return Response{}, ErrEmptyReadBuffer
	}
	if err := client.Write(stream, request); err != nil {
		return Response{}, err
	}

	state := responseState{client: client, stream: stream, callbacks: callbacks, firstFrame: true, noBody: equalASCII(request.Method, []byte("HEAD"))}
	state.decoder = h2.NewHeaderDecoder(client.HeaderLimits, state.onHeader)
	parserCallbacks := h2.Callbacks{
		FrameBegin:     state.onFrameBegin,
		Data:           state.onData,
		HeaderBlock:    state.onHeaderBlock,
		HeaderBlockEnd: state.onHeaderBlockEnd,
		RSTStream:      state.onRSTStream,
		SettingsEnd:    state.onSettingsEnd,
		PushPromise:    state.onPushPromise,
		Ping:           state.onPing,
		GoAway:         state.onGoAway,
		FrameComplete:  state.onFrameComplete,
	}
	parser := h2.NewParser(&parserCallbacks, client.FrameLimits)

	for {
		read, readErr := stream.Read(buffer)
		if read < 0 || read > len(buffer) {
			return Response{}, io.ErrShortBuffer
		}
		offset := 0
		for offset < read {
			consumed, complete, code := parser.ParseOne(buffer[offset:read])
			offset += consumed
			if code != h2.CodeNone {
				return Response{}, &FrameError{Code: code}
			}
			if state.err != nil {
				return Response{}, state.err
			}
			if state.complete {
				state.response.Buffered = buffer[offset:read:read]
				return state.response, nil
			}
			if consumed == 0 && !complete {
				return Response{}, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return Response{}, readErr
			}
			if code := parser.Finish(); code != h2.CodeNone {
				return Response{}, &FrameError{Code: code}
			}
			if state.complete {
				return state.response, nil
			}
			return Response{}, io.ErrUnexpectedEOF
		}
		if read == 0 {
			return Response{}, io.ErrNoProgress
		}
	}
}

type responseState struct {
	client    Client
	stream    Stream
	callbacks *Callbacks
	decoder   *h2.HeaderDecoder
	response  Response
	err       error

	firstFrame      bool
	finalHeaders    bool
	blockActive     bool
	blockTrailer    bool
	blockPseudoDone bool
	blockStatus     uint16
	blockSawStatus  bool
	blockContentLen uint64
	blockSawContent bool
	dataInFrame     uint64
	noBody          bool
	complete        bool
}

func (state *responseState) onFrameBegin(header h2.FrameHeader) {
	if state.err != nil {
		return
	}
	if state.firstFrame {
		state.firstFrame = false
		if header.Type != h2.FrameSettings || header.Flags.Has(h2.FlagACK) {
			state.err = ErrMissingSettings
			return
		}
	}
	if header.Type == h2.FrameHeaders {
		if header.StreamID != 1 || state.blockActive || state.complete {
			state.err = ErrInvalidResponse
			return
		}
		state.blockActive = true
		state.blockTrailer = state.finalHeaders
		state.blockPseudoDone = false
		state.blockStatus = 0
		state.blockSawStatus = false
		state.blockContentLen = 0
		state.blockSawContent = false
		if err := state.decoder.BeginBlock(); err != nil {
			state.err = &CompressionError{Err: err}
		}
	}
	if header.Type == h2.FrameData {
		state.dataInFrame = 0
		if header.StreamID != 1 || !state.finalHeaders || state.complete || state.noBody {
			state.err = ErrInvalidResponse
		}
	}
}

func (state *responseState) onHeaderBlock(streamID uint32, fragment []byte) {
	if state.err != nil {
		return
	}
	if streamID != 1 || !state.blockActive {
		state.err = ErrInvalidResponse
		return
	}
	written, err := state.decoder.Write(fragment)
	if err != nil || written != len(fragment) {
		if err == nil {
			err = io.ErrShortWrite
		}
		state.err = &CompressionError{Err: err}
	}
}

func (state *responseState) onHeader(field h2.HeaderField) {
	if state.err != nil {
		return
	}
	if !validDecodedName(field.Name) || !validDecodedValue(field.Value) {
		state.err = ErrInvalidResponse
		return
	}
	pseudo := len(field.Name) != 0 && field.Name[0] == ':'
	if pseudo {
		if state.blockTrailer || state.blockPseudoDone || field.Name != ":status" || state.blockSawStatus {
			state.err = ErrInvalidResponse
			return
		}
		status, ok := parseStatus(field.Value)
		if !ok {
			state.err = ErrInvalidResponse
			return
		}
		state.blockStatus = status
		state.blockSawStatus = true
	} else {
		state.blockPseudoDone = true
		if forbiddenResponseHeader(field.Name) {
			state.err = ErrInvalidResponse
			return
		}
		if field.Name == "content-length" {
			value, ok := parseContentLength(field.Value)
			if !ok || state.blockSawContent {
				state.err = ErrInvalidResponse
				return
			}
			state.blockSawContent = true
			state.blockContentLen = value
		}
	}
	if state.callbacks != nil && state.callbacks.Header != nil {
		state.callbacks.Header(field, state.blockTrailer)
	}
}

func (state *responseState) onHeaderBlockEnd(streamID uint32, endStream bool) {
	if state.err != nil {
		return
	}
	if streamID != 1 || !state.blockActive {
		state.err = ErrInvalidResponse
		return
	}
	if err := state.decoder.EndBlock(); err != nil {
		state.err = &CompressionError{Err: err}
		return
	}
	state.blockActive = false
	if state.blockTrailer {
		if state.blockSawStatus || state.blockSawContent || !endStream {
			state.err = ErrInvalidResponse
			return
		}
		if state.callbacks != nil && state.callbacks.TrailersComplete != nil {
			state.callbacks.TrailersComplete()
		}
		state.finishResponse()
		return
	}
	if !state.blockSawStatus {
		state.err = ErrInvalidResponse
		return
	}
	informational := state.blockStatus >= 100 && state.blockStatus < 200
	if informational {
		if state.blockStatus == 101 {
			state.err = ErrInvalidResponse
			return
		}
		if endStream || state.blockSawContent {
			state.err = ErrInvalidResponse
			return
		}
		if state.callbacks != nil && state.callbacks.HeadersComplete != nil {
			state.callbacks.HeadersComplete(Response{Status: state.blockStatus}, true)
		}
		return
	}
	state.finalHeaders = true
	state.noBody = state.noBody || state.blockStatus == 204 || state.blockStatus == 304
	if state.blockStatus == 204 && state.blockSawContent {
		state.err = ErrInvalidResponse
		return
	}
	state.response.Status = state.blockStatus
	state.response.HasContentLength = state.blockSawContent
	state.response.ContentLength = state.blockContentLen
	if state.callbacks != nil && state.callbacks.HeadersComplete != nil {
		state.callbacks.HeadersComplete(state.response, false)
	}
	if endStream {
		state.finishResponse()
	}
}

func (state *responseState) onData(streamID uint32, data []byte, _ bool) {
	if state.err != nil {
		return
	}
	if streamID != 1 || !state.finalHeaders || state.complete {
		state.err = ErrInvalidResponse
		return
	}
	limit := state.client.MaxResponseBodyBytes
	if limit == 0 {
		limit = defaultMaxResponseBody
	}
	if uint64(len(data)) > limit-state.response.BodyBytes {
		state.err = ErrResponseBodyTooLarge
		return
	}
	state.response.BodyBytes += uint64(len(data))
	state.dataInFrame += uint64(len(data))
	if state.callbacks != nil && state.callbacks.Body != nil {
		state.callbacks.Body(data)
	}
}

func (state *responseState) onFrameComplete(header h2.FrameHeader) {
	if state.err != nil {
		return
	}
	if header.Type == h2.FrameData && header.StreamID == 1 {
		if header.Length != 0 {
			if err := writeWindowUpdate(state.stream, 0, header.Length); err != nil {
				state.err = err
				return
			}
			if err := writeWindowUpdate(state.stream, 1, header.Length); err != nil {
				state.err = err
				return
			}
		}
		if header.Flags.Has(h2.FlagEndStream) {
			state.finishResponse()
		}
	}
}

func (state *responseState) onSettingsEnd(ack bool) {
	if state.err != nil || ack {
		return
	}
	state.err = writeFrame(state.stream, h2.FrameHeader{Type: h2.FrameSettings, Flags: h2.FlagACK}, nil)
}

func (state *responseState) onPing(data [8]byte, ack bool) {
	if state.err != nil || ack {
		return
	}
	state.err = writeFrame(state.stream, h2.FrameHeader{Type: h2.FramePing, Flags: h2.FlagACK}, data[:])
}

func (state *responseState) onRSTStream(streamID uint32, _ h2.ErrorCode) {
	if state.err == nil && streamID == 1 {
		state.err = ErrStreamReset
	}
}

func (state *responseState) onPushPromise(_, _ uint32) {
	if state.err == nil {
		state.err = ErrPushPromise
	}
}

func (state *responseState) onGoAway(lastStreamID uint32, _ h2.ErrorCode) {
	if state.err == nil && (!state.complete || lastStreamID < 1) {
		state.err = ErrGoAway
	}
}

func (state *responseState) finishResponse() {
	if state.complete || state.err != nil {
		return
	}
	if state.response.HasContentLength && state.response.ContentLength != state.response.BodyBytes {
		state.err = ErrInvalidResponse
		return
	}
	state.complete = true
	if state.callbacks != nil && state.callbacks.ResponseComplete != nil {
		state.callbacks.ResponseComplete(state.response)
	}
}

func requestHeaderLimits(limits h2.HeaderLimits) (maxField uint32, maxList uint64, maxHeaders uint32) {
	maxField = limits.MaxFieldBytes
	if maxField == 0 {
		maxField = 16 << 10
	}
	maxList = limits.MaxHeaderListBytes
	if maxList == 0 {
		maxList = 64 << 10
	}
	maxHeaders = limits.MaxHeaders
	if maxHeaders == 0 {
		maxHeaders = 256
	}
	return maxField, maxList, maxHeaders
}

func headerListEntrySize(nameBytes, valueBytes int) uint64 {
	return uint64(nameBytes) + uint64(valueBytes) + 32
}

func decimalLength(value uint64) int {
	length := 1
	for value >= 10 {
		value /= 10
		length++
	}
	return length
}

func headerDynamicTableLimit(limits h2.HeaderLimits) uint32 {
	if limits.MaxDynamicTableBytes == 0 {
		return 4 << 10
	}
	return limits.MaxDynamicTableBytes
}

func headerListLimit(limits h2.HeaderLimits) uint32 {
	if limits.MaxHeaderListBytes == 0 {
		return 64 << 10
	}
	if limits.MaxHeaderListBytes > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(limits.MaxHeaderListBytes)
}

func putSetting(dst []byte, id h2.SettingID, value uint32) {
	binary.BigEndian.PutUint16(dst[:2], uint16(id))
	binary.BigEndian.PutUint32(dst[2:6], value)
}

func writeWindowUpdate(writer io.Writer, streamID, increment uint32) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], increment&0x7fffffff)
	return writeFrame(writer, h2.FrameHeader{Type: h2.FrameWindowUpdate, StreamID: streamID}, payload[:])
}

func writeFrame(writer io.Writer, header h2.FrameHeader, payload []byte) error {
	wire, code := h2.AppendFrame(nil, header, payload)
	if code != h2.CodeNone {
		return &FrameError{Code: code}
	}
	return writeAll(writer, wire)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return io.ErrShortWrite
		}
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func validMethod(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, b := range value {
		if b <= 0x20 || b >= 0x7f {
			return false
		}
		switch b {
		case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
			return false
		}
	}
	return true
}

func validScheme(value []byte) bool {
	if len(value) == 0 || !isAlpha(value[0]) {
		return false
	}
	for _, b := range value[1:] {
		if !isAlpha(b) && (b < '0' || b > '9') && b != '+' && b != '-' && b != '.' {
			return false
		}
	}
	return true
}

func validLowerName(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, b := range value {
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

func validValue(value []byte) bool {
	if len(value) != 0 && (value[0] == ' ' || value[0] == '\t' || value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		return false
	}
	for _, b := range value {
		if b != '\t' && b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

func validPseudoValue(value []byte) bool {
	for _, b := range value {
		if b <= 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

func forbiddenRequestHeader(name, value []byte) bool {
	switch string(name) {
	case "host", "content-length", "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade":
		return true
	case "te":
		return string(value) != "trailers"
	default:
		return false
	}
}

func forbiddenResponseHeader(name string) bool {
	switch name {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade", "te":
		return true
	default:
		return false
	}
}

func validDecodedName(name string) bool {
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
	for i := start; i < len(name); i++ {
		b := name[i]
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

func validDecodedValue(value string) bool { return validValue([]byte(value)) }

func parseStatus(value string) (uint16, bool) {
	if len(value) != 3 || value[0] < '1' || value[0] > '9' || value[1] < '0' || value[1] > '9' || value[2] < '0' || value[2] > '9' {
		return 0, false
	}
	status := uint16(value[0]-'0')*100 + uint16(value[1]-'0')*10 + uint16(value[2]-'0')
	return status, status >= 100 && status <= 599
}

func parseContentLength(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	var result uint64
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
		digit := uint64(value[i] - '0')
		if result > (^uint64(0)-digit)/10 {
			return 0, false
		}
		result = result*10 + digit
	}
	return result, true
}

func equalASCII(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i, b := range left {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		other := right[i]
		if other >= 'A' && other <= 'Z' {
			other += 'a' - 'A'
		}
		if b != other {
			return false
		}
	}
	return true
}

func isAlpha(b byte) bool { return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }
