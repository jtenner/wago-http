// Package server runs a bounded, streaming, non-TLS HTTP/2 server connection
// over a caller-owned byte stream.
package server

import (
	"errors"
	"io"
	"sync"

	h2 "github.com/wago-org/http/http2"
)

// Stream is the connected byte transport required by Conn.
type Stream interface {
	io.Reader
	io.Writer
}

// Options configures one server connection.
type Options struct {
	Session         h2.SessionLimits
	ReadBufferBytes uint32
}

// Handler receives committed stream events. Methods are called serially from
// the connection read loop; handlers that perform long-running work should hand
// off to their own bounded worker pool. ResponseWriter methods are concurrent-safe.
type Handler interface {
	Headers(*ResponseWriter, []h2.HeaderField, bool, bool)
	Data(*ResponseWriter, []byte)
	End(*ResponseWriter)
	Writable(*ResponseWriter, uint32)
	Reset(streamID uint32, code h2.ErrorCode)
}

// HandlerFuncs adapts functions to Handler.
type HandlerFuncs struct {
	OnHeaders  func(*ResponseWriter, []h2.HeaderField, bool, bool)
	OnData     func(*ResponseWriter, []byte)
	OnEnd      func(*ResponseWriter)
	OnWritable func(*ResponseWriter, uint32)
	OnReset    func(uint32, h2.ErrorCode)
}

func (handler HandlerFuncs) Headers(writer *ResponseWriter, fields []h2.HeaderField, endStream, trailer bool) {
	if handler.OnHeaders != nil {
		handler.OnHeaders(writer, fields, endStream, trailer)
	}
}
func (handler HandlerFuncs) Data(writer *ResponseWriter, data []byte) {
	if handler.OnData != nil {
		handler.OnData(writer, data)
	}
}
func (handler HandlerFuncs) End(writer *ResponseWriter) {
	if handler.OnEnd != nil {
		handler.OnEnd(writer)
	}
}
func (handler HandlerFuncs) Writable(writer *ResponseWriter, increment uint32) {
	if handler.OnWritable != nil {
		handler.OnWritable(writer, increment)
	}
}
func (handler HandlerFuncs) Reset(streamID uint32, code h2.ErrorCode) {
	if handler.OnReset != nil {
		handler.OnReset(streamID, code)
	}
}

// Conn owns one server-side HTTP/2 session.
type Conn struct {
	stream  Stream
	session *h2.Session
	handler Handler
	buffer  []byte

	mu      sync.Mutex
	writers map[uint32]*ResponseWriter
	closed  bool
	err     error
}

// ResponseWriter sends one stream's response headers, DATA, trailers, reset,
// and optional pushed responses.
type ResponseWriter struct {
	conn     *Conn
	streamID uint32
}

// New constructs a server connection and writes its initial SETTINGS frame.
func New(stream Stream, handler Handler, options Options) (*Conn, error) {
	if stream == nil || handler == nil {
		return nil, errors.New("wagohttp/http2/server: invalid stream or handler")
	}
	size := options.ReadBufferBytes
	if size == 0 {
		size = 16 << 10
	}
	if uint64(size) > uint64(^uint(0)>>1) {
		return nil, io.ErrShortBuffer
	}
	session, err := h2.NewSession(h2.RoleServer, options.Session)
	if err != nil {
		return nil, err
	}
	conn := &Conn{stream: stream, session: session, handler: handler, buffer: make([]byte, int(size)), writers: make(map[uint32]*ResponseWriter)}
	conn.mu.Lock()
	err = conn.flushLocked()
	conn.mu.Unlock()
	if err != nil {
		session.Close()
		return nil, err
	}
	return conn, nil
}

// Serve reads and handles frames until EOF, protocol failure, or Close.
func (conn *Conn) Serve() error {
	for {
		read, readErr := conn.stream.Read(conn.buffer)
		if read < 0 || read > len(conn.buffer) {
			return conn.fail(io.ErrShortBuffer)
		}
		if read != 0 {
			conn.mu.Lock()
			consumed, err := conn.session.Feed(conn.buffer[:read])
			if err == nil && consumed != read {
				err = io.ErrNoProgress
			}
			flushErr := conn.flushLocked()
			if err == nil {
				err = flushErr
			}
			events := conn.collectEventsLocked()
			conn.mu.Unlock()
			if err != nil {
				return conn.fail(err)
			}
			conn.dispatch(events)
		}
		if readErr != nil {
			if readErr == io.EOF {
				conn.mu.Lock()
				err := conn.session.Finish()
				conn.mu.Unlock()
				if err == nil {
					err = io.EOF
				}
				return conn.fail(err)
			}
			return conn.fail(readErr)
		}
		if read == 0 {
			return conn.fail(io.ErrNoProgress)
		}
	}
}

func (conn *Conn) dispatch(events []h2.Event) {
	for _, event := range events {
		switch event.Type {
		case h2.EventHeaders:
			writer := conn.writer(event.StreamID)
			conn.handler.Headers(writer, event.Headers, event.EndStream, event.Trailer)
		case h2.EventData:
			writer := conn.writer(event.StreamID)
			conn.handler.Data(writer, event.Data)
		case h2.EventStreamEnd:
			writer := conn.writer(event.StreamID)
			conn.handler.End(writer)
		case h2.EventWindowUpdate:
			if event.StreamID != 0 {
				conn.handler.Writable(conn.writer(event.StreamID), event.WindowIncrement)
			} else {
				conn.mu.Lock()
				writers := make([]*ResponseWriter, 0, len(conn.writers))
				for _, writer := range conn.writers {
					writers = append(writers, writer)
				}
				conn.mu.Unlock()
				for _, writer := range writers {
					conn.handler.Writable(writer, event.WindowIncrement)
				}
			}
		case h2.EventStreamReset:
			conn.handler.Reset(event.StreamID, event.ErrorCode)
			conn.mu.Lock()
			delete(conn.writers, event.StreamID)
			conn.mu.Unlock()
		}
	}
}

func (conn *Conn) writer(streamID uint32) *ResponseWriter {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	writer := conn.writers[streamID]
	if writer == nil {
		writer = &ResponseWriter{conn: conn, streamID: streamID}
		conn.writers[streamID] = writer
	}
	return writer
}

// StreamID returns the associated HTTP/2 stream identifier.
func (writer *ResponseWriter) StreamID() uint32 { return writer.streamID }

// Headers sends informational/final response headers or trailers.
func (writer *ResponseWriter) Headers(fields []h2.HeaderField, endStream bool) error {
	writer.conn.mu.Lock()
	defer writer.conn.mu.Unlock()
	if writer.conn.closed {
		return h2.ErrSessionClosed
	}
	if err := writer.conn.session.SendHeaders(writer.streamID, fields, endStream); err != nil {
		return err
	}
	if err := writer.conn.flushLocked(); err != nil {
		return err
	}
	if endStream {
		delete(writer.conn.writers, writer.streamID)
	}
	return nil
}

// Write sends as much response DATA as flow control currently permits.
func (writer *ResponseWriter) Write(data []byte, endStream bool) (int, error) {
	writer.conn.mu.Lock()
	defer writer.conn.mu.Unlock()
	if writer.conn.closed {
		return 0, h2.ErrSessionClosed
	}
	n, err := writer.conn.session.SendData(writer.streamID, data, endStream)
	if n > 0 || err == nil {
		if flushErr := writer.conn.flushLocked(); flushErr != nil {
			return n, flushErr
		}
	}
	if err == nil && endStream && n == len(data) {
		delete(writer.conn.writers, writer.streamID)
	}
	return n, err
}

// Reset aborts the stream.
func (writer *ResponseWriter) Reset(code h2.ErrorCode) error {
	writer.conn.mu.Lock()
	defer writer.conn.mu.Unlock()
	if err := writer.conn.session.ResetStream(writer.streamID, code); err != nil {
		return err
	}
	delete(writer.conn.writers, writer.streamID)
	return writer.conn.flushLocked()
}

// PushPromise reserves and returns one pushed response stream.
func (writer *ResponseWriter) PushPromise(requestHeaders []h2.HeaderField) (*ResponseWriter, error) {
	writer.conn.mu.Lock()
	defer writer.conn.mu.Unlock()
	streamID, err := writer.conn.session.PushPromise(writer.streamID, requestHeaders)
	if err != nil {
		return nil, err
	}
	if err := writer.conn.flushLocked(); err != nil {
		return nil, err
	}
	pushed := &ResponseWriter{conn: writer.conn, streamID: streamID}
	writer.conn.writers[streamID] = pushed
	return pushed, nil
}

// PriorityUpdate sends RFC 9218 extensible priority metadata.
func (writer *ResponseWriter) PriorityUpdate(value []byte) error {
	writer.conn.mu.Lock()
	defer writer.conn.mu.Unlock()
	if err := writer.conn.session.PriorityUpdate(writer.streamID, value); err != nil {
		return err
	}
	return writer.conn.flushLocked()
}

func (conn *Conn) collectEventsLocked() []h2.Event {
	var events []h2.Event
	for {
		event, ok := conn.session.NextEvent()
		if !ok {
			return events
		}
		events = append(events, event)
	}
}

func (conn *Conn) flushLocked() error {
	for len(conn.session.Output()) != 0 {
		output := conn.session.Output()
		written, err := conn.stream.Write(output)
		if written < 0 || written > len(output) {
			return io.ErrShortWrite
		}
		if written > 0 {
			if consumeErr := conn.session.ConsumeOutput(written); consumeErr != nil {
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

func (conn *Conn) fail(err error) error {
	conn.mu.Lock()
	if !conn.closed {
		conn.closed = true
		conn.err = err
		conn.session.Close()
	}
	conn.mu.Unlock()
	return err
}

// Close gracefully sends GOAWAY and closes the underlying stream when possible.
func (conn *Conn) Close() error {
	if conn == nil {
		return nil
	}
	conn.mu.Lock()
	if conn.closed {
		err := conn.err
		conn.mu.Unlock()
		return err
	}
	_ = conn.session.GoAway(h2.ErrCodeNo, nil)
	_ = conn.flushLocked()
	conn.closed = true
	conn.session.Close()
	conn.mu.Unlock()
	if closer, ok := conn.stream.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
