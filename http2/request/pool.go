package request

import (
	"context"
	"errors"
	"io"
	"sync"
)

// DialFunc returns a new established HTTP/2 byte stream. TLS and ALPN, when
// required, are the dialer's responsibility.
type DialFunc func(context.Context) (Stream, error)

// Pool maintains a reusable connection and retries replayable idempotent
// requests rejected before response headers by GOAWAY or REFUSED_STREAM.
type Pool struct {
	Dial       DialFunc
	Options    TransportOptions
	MaxRetries uint32

	mu         sync.Mutex
	current    *Transport
	transports map[*Transport]struct{}
	closed     bool
}

// Do executes request, dialing lazily. The zero MaxRetries default is one.
func (pool *Pool) Do(ctx context.Context, request Request, callbacks *Callbacks) (Response, error) {
	maxRetries := pool.MaxRetries
	if maxRetries == 0 {
		maxRetries = 1
	}
	current := request
	for attempt := uint32(0); ; attempt++ {
		transport, err := pool.transport(ctx)
		if err != nil {
			return Response{}, err
		}
		response, err := transport.Do(ctx, current, callbacks)
		if err == nil {
			return response, nil
		}
		if attempt >= maxRetries || !retryableRequest(current, err) {
			return Response{}, err
		}
		pool.discard(transport)
		current, err = replayRequest(current)
		if err != nil {
			return Response{}, err
		}
	}
}

func (pool *Pool) transport(ctx context.Context) (*Transport, error) {
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return nil, io.ErrClosedPipe
	}
	if pool.current != nil {
		transport := pool.current
		pool.mu.Unlock()
		return transport, nil
	}
	dial := pool.Dial
	pool.mu.Unlock()
	if dial == nil {
		return nil, ErrInvalidStream
	}
	stream, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	transport, err := NewTransport(stream, pool.Options)
	if err != nil {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, err
	}
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		_ = transport.Close()
		return nil, io.ErrClosedPipe
	}
	if pool.transports == nil {
		pool.transports = make(map[*Transport]struct{})
	}
	pool.transports[transport] = struct{}{}
	if pool.current == nil {
		pool.current = transport
	}
	selected := pool.current
	pool.mu.Unlock()
	if selected != transport {
		_ = transport.Close()
	}
	return selected, nil
}

func (pool *Pool) discard(transport *Transport) {
	pool.mu.Lock()
	if pool.current == transport {
		pool.current = nil
	}
	pool.mu.Unlock()
}

// Close closes every connection created by the pool.
func (pool *Pool) Close() error {
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		return nil
	}
	pool.closed = true
	transports := pool.transports
	pool.transports = nil
	pool.current = nil
	pool.mu.Unlock()
	var first error
	for transport := range transports {
		if err := transport.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func retryableRequest(request Request, err error) bool {
	if !idempotentMethod(request.Method) {
		return false
	}
	if request.BodyReader != nil && request.ReplayBody == nil {
		return false
	}
	var streamErr *StreamError
	if errors.As(err, &streamErr) {
		return streamErr.Retryable
	}
	var goAway *GoAwayError
	return errors.As(err, &goAway) || errors.Is(err, ErrGoAway)
}

func replayRequest(request Request) (Request, error) {
	if request.BodyReader == nil {
		return request, nil
	}
	body, err := request.ReplayBody()
	if err != nil {
		return Request{}, err
	}
	request.BodyReader = body
	return request, nil
}

func idempotentMethod(method []byte) bool {
	switch string(method) {
	case "GET", "HEAD", "PUT", "DELETE", "OPTIONS", "TRACE":
		return true
	default:
		return false
	}
}
