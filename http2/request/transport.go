package request

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"

	h2 "github.com/wago-org/http/http2"
)

// TransportOptions configures one persistent multiplexed HTTP/2 connection.
type TransportOptions struct {
	Session              h2.SessionLimits
	ReadBufferBytes      uint32
	EventBuffer          uint32
	MaxResponseBodyBytes uint64
	PushHandler          func(PushRequest) (*Callbacks, bool)
}

// PushRequest describes one promised server-push request field section.
type PushRequest struct {
	StreamID uint32
	Headers  []h2.HeaderField
}

// Transport owns one established non-TLS HTTP/2 byte stream. It supports
// concurrent Do calls, persistent HPACK state, multiplexed streams, streaming
// request bodies, flow-control waits, and connection-wide control handling.
type Transport struct {
	stream          Stream
	session         *h2.Session
	readBuffer      []byte
	eventBuffer     uint32
	maxResponseBody uint64
	pushHandler     func(PushRequest) (*Callbacks, bool)
	bodyBuffers     sync.Pool

	mu        sync.Mutex
	pending   map[uint32]*transportExchange
	window    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	err       error
	goAway    *GoAwayError
}

type transportExchange struct {
	events       chan h2.Event
	callbacks    *Callbacks
	response     Response
	maxBody      uint64
	finalHeaders bool
	complete     bool
}

// NewTransport starts one client HTTP/2 session over stream. The caller must
// already have performed any TCP connection setup and optional TLS/ALPN.
func NewTransport(stream Stream, options TransportOptions) (*Transport, error) {
	if stream == nil {
		return nil, ErrInvalidStream
	}
	size := options.ReadBufferBytes
	if size == 0 {
		size = DefaultReadBufferBytes
	}
	if uint64(size) > uint64(^uint(0)>>1) {
		return nil, ErrReadBufferTooLarge
	}
	eventBuffer := options.EventBuffer
	if eventBuffer == 0 {
		eventBuffer = 16
	}
	if eventBuffer > 1<<16 {
		return nil, ErrInvalidRequest
	}
	maxResponseBody := options.MaxResponseBodyBytes
	if maxResponseBody == 0 {
		maxResponseBody = defaultMaxResponseBody
	}
	session, err := h2.NewSession(h2.RoleClient, options.Session)
	if err != nil {
		return nil, err
	}
	transport := &Transport{
		stream:          stream,
		session:         session,
		readBuffer:      make([]byte, int(size)),
		eventBuffer:     eventBuffer,
		maxResponseBody: maxResponseBody,
		pushHandler:     options.PushHandler,
		pending:         make(map[uint32]*transportExchange),
		window:          make(chan struct{}, 1),
		done:            make(chan struct{}),
	}
	transport.bodyBuffers.New = func() any { return make([]byte, 16<<10) }
	transport.mu.Lock()
	err = transport.flushLocked()
	transport.mu.Unlock()
	if err != nil {
		session.Close()
		return nil, err
	}
	go transport.readLoop()
	return transport, nil
}

// Do performs one multiplexed request. BodyReader, when non-nil, is streamed
// under peer flow control. HasBodyLength emits BodyLength for BodyReader.
// Request trailers are sent in a final HEADERS block.
func (transport *Transport) Do(ctx context.Context, request Request, callbacks *Callbacks) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	headers, body, length, err := transportRequest(request)
	if err != nil {
		return Response{}, err
	}
	exchange := &transportExchange{events: make(chan h2.Event, int(transport.eventBuffer)), callbacks: callbacks, maxBody: transport.maxResponseBody}

	transport.mu.Lock()
	if transport.err != nil {
		err = transport.err
		transport.mu.Unlock()
		return Response{}, err
	}
	if transport.goAway != nil {
		err = transport.goAway
		transport.mu.Unlock()
		return Response{}, err
	}
	endHeaders := body == nil && len(request.Trailers) == 0
	streamID, err := transport.session.OpenStream(headers, endHeaders)
	if err == nil {
		transport.pending[streamID] = exchange
		err = transport.flushLocked()
	}
	transport.mu.Unlock()
	if err != nil {
		return Response{}, err
	}

	if body != nil {
		if err := transport.sendBody(ctx, streamID, body, request.Trailers, exchange); err != nil {
			transport.cancelStream(streamID)
			return Response{}, err
		}
	} else if len(request.Trailers) != 0 {
		if err := transport.sendTrailers(streamID, request.Trailers); err != nil {
			transport.cancelStream(streamID)
			return Response{}, err
		}
	}
	_ = length
	if exchange.complete {
		transport.mu.Lock()
		delete(transport.pending, streamID)
		transport.mu.Unlock()
		return exchange.response, nil
	}

	for {
		select {
		case event, ok := <-exchange.events:
			if !ok {
				transport.mu.Lock()
				err := transport.err
				transport.mu.Unlock()
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return Response{}, err
			}
			if err := exchange.consume(event); err != nil {
				transport.cancelStream(streamID)
				return Response{}, err
			}
			if exchange.complete {
				transport.mu.Lock()
				delete(transport.pending, streamID)
				transport.mu.Unlock()
				return exchange.response, nil
			}
		case <-ctx.Done():
			transport.cancelStream(streamID)
			return Response{}, ctx.Err()
		case <-transport.done:
			transport.mu.Lock()
			err := transport.err
			transport.mu.Unlock()
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return Response{}, err
		}
	}
}

func (transport *Transport) sendBody(ctx context.Context, streamID uint32, body io.Reader, trailers []Header, exchange *transportExchange) error {
	buffer, _ := transport.bodyBuffers.Get().([]byte)
	if cap(buffer) < 16<<10 {
		buffer = make([]byte, 16<<10)
	} else {
		buffer = buffer[:16<<10]
	}
	defer transport.bodyBuffers.Put(buffer)
	for {
		read, readErr := body.Read(buffer)
		if read < 0 || read > len(buffer) {
			return io.ErrShortBuffer
		}
		if err := transport.consumeAvailable(exchange); err != nil {
			return err
		}
		if exchange.complete {
			transport.cancelStream(streamID)
			return nil
		}
		offset := 0
		for offset < read {
			transport.mu.Lock()
			if transport.err != nil {
				err := transport.err
				transport.mu.Unlock()
				return err
			}
			n, err := transport.session.SendData(streamID, buffer[offset:read], false)
			if n > 0 {
				offset += n
				if flushErr := transport.flushLocked(); flushErr != nil {
					err = flushErr
				}
			}
			transport.mu.Unlock()
			if err != nil && !errors.Is(err, h2.ErrWouldBlock) {
				return err
			}
			if offset < read && (n == 0 || errors.Is(err, h2.ErrWouldBlock)) {
				select {
				case event, ok := <-exchange.events:
					if !ok {
						return transport.connectionError()
					}
					if err := exchange.consume(event); err != nil {
						return err
					}
					if exchange.complete {
						transport.cancelStream(streamID)
						return nil
					}
				case <-transport.window:
				case <-ctx.Done():
					return ctx.Err()
				case <-transport.done:
					return transport.connectionError()
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			if len(trailers) != 0 {
				return transport.sendTrailers(streamID, trailers)
			}
			transport.mu.Lock()
			_, err := transport.session.SendData(streamID, nil, true)
			if err == nil {
				err = transport.flushLocked()
			}
			transport.mu.Unlock()
			return err
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
}

func (transport *Transport) consumeAvailable(exchange *transportExchange) error {
	for {
		select {
		case event, ok := <-exchange.events:
			if !ok {
				return transport.connectionError()
			}
			if err := exchange.consume(event); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (transport *Transport) sendTrailers(streamID uint32, trailers []Header) error {
	fields := make([]h2.HeaderField, len(trailers))
	for index, trailer := range trailers {
		if !validLowerName(trailer.Name) || !validValue(trailer.Value) || reservedTrailer(trailer.Name) {
			return ErrInvalidRequest
		}
		fields[index] = h2.HeaderField{Name: string(trailer.Name), Value: string(trailer.Value), Sensitive: trailer.Sensitive}
	}
	transport.mu.Lock()
	err := transport.session.SendHeaders(streamID, fields, true)
	if err == nil {
		err = transport.flushLocked()
	}
	transport.mu.Unlock()
	return err
}

func (transport *Transport) cancelStream(streamID uint32) {
	transport.mu.Lock()
	_ = transport.session.ResetStream(streamID, h2.ErrCodeCancel)
	_ = transport.flushLocked()
	delete(transport.pending, streamID)
	transport.mu.Unlock()
}

func (transport *Transport) readLoop() {
	for {
		read, readErr := transport.stream.Read(transport.readBuffer)
		if read < 0 || read > len(transport.readBuffer) {
			transport.fail(io.ErrShortBuffer)
			return
		}
		if read != 0 {
			transport.mu.Lock()
			consumed, err := transport.session.Feed(transport.readBuffer[:read])
			if err == nil && consumed != read {
				err = io.ErrNoProgress
			}
			if err == nil {
				err = transport.flushLocked()
			}
			events := transport.collectEventsLocked()
			transport.mu.Unlock()
			if err != nil {
				transport.fail(err)
				return
			}
			transport.dispatch(events)
			select {
			case transport.window <- struct{}{}:
			default:
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				transport.mu.Lock()
				finishErr := transport.session.Finish()
				transport.mu.Unlock()
				if finishErr == nil {
					finishErr = io.EOF
				}
				transport.fail(finishErr)
			} else {
				transport.fail(readErr)
			}
			return
		}
		if read == 0 {
			transport.fail(io.ErrNoProgress)
			return
		}
	}
}

func (transport *Transport) collectEventsLocked() []h2.Event {
	var events []h2.Event
	for {
		event, ok := transport.session.NextEvent()
		if !ok {
			return events
		}
		events = append(events, event)
	}
}

func (transport *Transport) dispatch(events []h2.Event) {
	for _, event := range events {
		if event.Type == h2.EventPushPromise {
			transport.acceptPush(event)
			continue
		}
		if event.StreamID == 0 {
			if event.Type == h2.EventGoAway {
				transport.mu.Lock()
				transport.goAway = &GoAwayError{LastStreamID: event.LastStreamID, Code: event.ErrorCode}
				transport.mu.Unlock()
			}
			continue
		}
		transport.mu.Lock()
		exchange := transport.pending[event.StreamID]
		transport.mu.Unlock()
		if exchange == nil {
			continue
		}
		select {
		case exchange.events <- event:
		case <-transport.done:
			return
		}
	}
}

func (transport *Transport) acceptPush(event h2.Event) {
	if transport.pushHandler == nil {
		transport.cancelStream(event.StreamID)
		return
	}
	callbacks, accepted := transport.pushHandler(PushRequest{StreamID: event.StreamID, Headers: event.Headers})
	if !accepted {
		transport.cancelStream(event.StreamID)
		return
	}
	exchange := &transportExchange{
		events:    make(chan h2.Event, int(transport.eventBuffer)),
		callbacks: callbacks,
		maxBody:   transport.maxResponseBody,
	}
	transport.mu.Lock()
	if transport.err != nil {
		transport.mu.Unlock()
		return
	}
	transport.pending[event.StreamID] = exchange
	transport.mu.Unlock()
	go transport.consumePush(event.StreamID, exchange)
}

func (transport *Transport) consumePush(streamID uint32, exchange *transportExchange) {
	for event := range exchange.events {
		if err := exchange.consume(event); err != nil || exchange.complete {
			if err != nil {
				transport.cancelStream(streamID)
			}
			transport.mu.Lock()
			delete(transport.pending, streamID)
			transport.mu.Unlock()
			return
		}
	}
}

func (transport *Transport) flushLocked() error {
	for len(transport.session.Output()) != 0 {
		output := transport.session.Output()
		written, err := transport.stream.Write(output)
		if written < 0 || written > len(output) {
			return io.ErrShortWrite
		}
		if written > 0 {
			if consumeErr := transport.session.ConsumeOutput(written); consumeErr != nil {
				return consumeErr
			}
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

func (transport *Transport) fail(err error) {
	transport.closeOnce.Do(func() {
		transport.mu.Lock()
		transport.err = err
		for _, exchange := range transport.pending {
			close(exchange.events)
		}
		clear(transport.pending)
		transport.session.Close()
		transport.mu.Unlock()
		close(transport.done)
	})
}

func (transport *Transport) connectionError() error {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.err != nil {
		return transport.err
	}
	return io.ErrUnexpectedEOF
}

// Close stops the transport and closes the underlying stream when it implements
// io.Closer.
func (transport *Transport) Close() error {
	if transport == nil {
		return nil
	}
	var closeErr error
	if closer, ok := transport.stream.(io.Closer); ok {
		closeErr = closer.Close()
	}
	transport.fail(h2.ErrSessionClosed)
	return closeErr
}

func (exchange *transportExchange) consume(event h2.Event) error {
	switch event.Type {
	case h2.EventHeaders:
		if event.Trailer {
			for _, field := range event.Headers {
				if exchange.callbacks != nil && exchange.callbacks.Header != nil {
					exchange.callbacks.Header(field, true)
				}
			}
			if exchange.callbacks != nil && exchange.callbacks.TrailersComplete != nil {
				exchange.callbacks.TrailersComplete()
			}
			return nil
		}
		status := uint16(0)
		for _, field := range event.Headers {
			if field.Name == ":status" {
				parsed, err := strconv.ParseUint(field.Value, 10, 16)
				if err != nil {
					return ErrInvalidResponse
				}
				status = uint16(parsed)
			} else if field.Name == "content-length" {
				parsed, err := strconv.ParseUint(field.Value, 10, 64)
				if err != nil {
					return ErrInvalidResponse
				}
				exchange.response.HasContentLength = true
				exchange.response.ContentLength = parsed
			}
			if exchange.callbacks != nil && exchange.callbacks.Header != nil {
				exchange.callbacks.Header(field, false)
			}
		}
		if status == 0 {
			return ErrInvalidResponse
		}
		informational := status < 200
		if !informational {
			exchange.finalHeaders = true
			exchange.response.Status = status
		}
		if exchange.callbacks != nil && exchange.callbacks.HeadersComplete != nil {
			exchange.callbacks.HeadersComplete(Response{Status: status, ContentLength: exchange.response.ContentLength, HasContentLength: exchange.response.HasContentLength}, informational)
		}
	case h2.EventData:
		if !exchange.finalHeaders {
			return ErrInvalidResponse
		}
		if uint64(len(event.Data)) > exchange.maxBody-exchange.response.BodyBytes {
			return ErrResponseBodyTooLarge
		}
		exchange.response.BodyBytes += uint64(len(event.Data))
		if exchange.callbacks != nil && exchange.callbacks.Body != nil {
			exchange.callbacks.Body(event.Data)
		}
	case h2.EventStreamEnd:
		if !exchange.finalHeaders {
			return ErrInvalidResponse
		}
		exchange.complete = true
		if exchange.callbacks != nil && exchange.callbacks.ResponseComplete != nil {
			exchange.callbacks.ResponseComplete(exchange.response)
		}
	case h2.EventStreamReset:
		return &StreamError{Code: event.ErrorCode, Retryable: event.ErrorCode == h2.ErrCodeRefusedStream && !exchange.finalHeaders}
	}
	return nil
}

func transportRequest(request Request) ([]h2.HeaderField, io.Reader, int64, error) {
	if len(request.Body) != 0 && request.BodyReader != nil {
		return nil, nil, 0, ErrInvalidRequest
	}
	method := string(request.Method)
	if !validMethod(request.Method) || len(request.Authority) == 0 || !validPseudoValue(request.Authority) {
		return nil, nil, 0, ErrInvalidRequest
	}
	fields := []h2.HeaderField{{Name: ":method", Value: method}}
	if method == "CONNECT" && len(request.Protocol) == 0 {
		fields = append(fields, h2.HeaderField{Name: ":authority", Value: string(request.Authority)})
	} else {
		if !validScheme(request.Scheme) || len(request.Path) == 0 || !validPseudoValue(request.Path) {
			return nil, nil, 0, ErrInvalidRequest
		}
		fields = append(fields,
			h2.HeaderField{Name: ":scheme", Value: string(request.Scheme)},
			h2.HeaderField{Name: ":authority", Value: string(request.Authority)},
			h2.HeaderField{Name: ":path", Value: string(request.Path)},
		)
		if len(request.Protocol) != 0 {
			if method != "CONNECT" || !validLowerName(request.Protocol) {
				return nil, nil, 0, ErrInvalidRequest
			}
			fields = append(fields, h2.HeaderField{Name: ":protocol", Value: string(request.Protocol)})
		}
	}
	for _, header := range request.Headers {
		if !validLowerName(header.Name) || !validValue(header.Value) || forbiddenRequestHeader(header.Name, header.Value) {
			return nil, nil, 0, ErrInvalidRequest
		}
		fields = append(fields, h2.HeaderField{Name: string(header.Name), Value: string(header.Value), Sensitive: header.Sensitive})
	}
	var body io.Reader
	length := int64(-1)
	if request.BodyReader != nil {
		body = request.BodyReader
		if request.HasBodyLength {
			if request.BodyLength < 0 {
				return nil, nil, 0, ErrInvalidRequest
			}
			length = request.BodyLength
		}
	} else if len(request.Body) != 0 {
		body = bytesReader(request.Body)
		length = int64(len(request.Body))
	}
	if length >= 0 && body != nil {
		fields = append(fields, h2.HeaderField{Name: "content-length", Value: strconv.FormatInt(length, 10)})
	}
	return fields, body, length, nil
}

type sliceReader struct {
	data   []byte
	offset int
}

func bytesReader(data []byte) io.Reader { return &sliceReader{data: data} }
func (reader *sliceReader) Read(dst []byte) (int, error) {
	if reader.offset == len(reader.data) {
		return 0, io.EOF
	}
	n := copy(dst, reader.data[reader.offset:])
	reader.offset += n
	return n, nil
}

func reservedTrailer(name []byte) bool {
	return reservedHeaderName(name) || equalASCII(name, []byte("te"))
}

func reservedHeaderName(name []byte) bool {
	return equalASCII(name, []byte("host")) || equalASCII(name, []byte("content-length")) ||
		equalASCII(name, []byte("connection")) || equalASCII(name, []byte("proxy-connection")) ||
		equalASCII(name, []byte("keep-alive")) || equalASCII(name, []byte("transfer-encoding")) ||
		equalASCII(name, []byte("upgrade"))
}
