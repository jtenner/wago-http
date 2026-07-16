// Package request writes bounded HTTP/1.1 requests and performs one synchronous
// exchange over a caller-owned byte stream. It owns no sockets: callers can
// supply any connected transport, including a future wago-org/net stream
// adapter, while retaining authority over dialing, deadlines, and teardown.
package request

import (
	"errors"
	"io"
	"strconv"

	http1 "github.com/wago-org/http/http"
)

const DefaultReadBufferBytes = 16 << 10

var (
	ErrInvalidRequest  = errors.New("wagohttp/request: invalid request")
	ErrInvalidResponse = errors.New("wagohttp/request: invalid response")
	ErrInvalidStream   = errors.New("wagohttp/request: invalid stream")
	ErrReservedHeader  = errors.New("wagohttp/request: Host, Content-Length, and Transfer-Encoding are managed by the request writer")
	ErrShortBuffer     = errors.New("wagohttp/request: destination buffer is too small")
	ErrEmptyReadBuffer = errors.New("wagohttp/request: read buffer is empty")
)

// Header is one borrowed HTTP field. Name and Value are read only for the
// duration of validation or encoding and are never retained.
type Header struct {
	Name  []byte
	Value []byte
}

// Request is one fixed-length HTTP/1.1 request. Method, Target, Host, Headers,
// and Body are borrowed and never retained. Host, Content-Length, and
// Transfer-Encoding are generated or controlled by the writer to prevent
// ambiguous framing. ContentLength requests an explicit Content-Length: 0 for
// an empty body; non-empty bodies always receive Content-Length.
type Request struct {
	Method        []byte
	Target        []byte
	Host          []byte
	Headers       []Header
	Body          []byte
	ContentLength bool
}

// Stream is the connected transport required by Client. Client does not close
// it and does not alter transport deadlines.
type Stream interface {
	io.Reader
	io.Writer
}

// Response describes the final response in an exchange. Buffered aliases the
// supplied Client.DoBuffer scratch buffer and contains bytes read after the
// final response boundary, such as upgraded-protocol or pipelined bytes.
type Response struct {
	Message  http1.Message
	Buffered []byte
	Upgraded bool
}

// ValidationError reports a request rejected by the strict HTTP/1 parser.
type ValidationError struct {
	Code http1.Code
}

func (e *ValidationError) Error() string {
	return "wagohttp/request: invalid request: " + e.Code.String()
}

func (e *ValidationError) Unwrap() error { return ErrInvalidRequest }

// ParseError reports a rejected HTTP response.
type ParseError struct {
	Code http1.Code
}

func (e *ParseError) Error() string {
	return "wagohttp/request: invalid response: " + e.Code.String()
}

func (e *ParseError) Unwrap() error { return ErrInvalidResponse }

// Client performs one request and reads through the final response. Zero
// parser limits select the finite defaults from package http. ReadBufferBytes
// defaults to DefaultReadBufferBytes. A Client must not be used concurrently
// when its callbacks mutate shared caller state.
type Client struct {
	RequestLimits   http1.Limits
	ResponseLimits  http1.Limits
	ReadBufferBytes uint32
}

// Validate checks the request with the same strict parser used for inbound
// HTTP/1 messages. The body is not scanned; its declared length and configured
// body limit are checked when the generated headers complete.
func Validate(request Request, limits http1.Limits) error {
	if !validToken(request.Method) || len(request.Target) == 0 || len(request.Host) == 0 || !validFieldValue(request.Host) {
		return ErrInvalidRequest
	}
	for _, header := range request.Headers {
		if !validToken(header.Name) || !validFieldValue(header.Value) {
			return ErrInvalidRequest
		}
		if reservedHeader(header.Name) {
			return ErrReservedHeader
		}
	}

	parser := http1.NewParser(http1.Request, nil, limits)
	parts := [...][]byte{
		request.Method,
		space,
		request.Target,
		http11HostPrefix,
		request.Host,
		crlf,
	}
	for _, part := range parts {
		if err := parsePart(&parser, part); err != nil {
			return err
		}
	}
	for _, header := range request.Headers {
		if err := parsePart(&parser, header.Name); err != nil {
			return err
		}
		if err := parsePart(&parser, colonSpace); err != nil {
			return err
		}
		if err := parsePart(&parser, header.Value); err != nil {
			return err
		}
		if err := parsePart(&parser, crlf); err != nil {
			return err
		}
	}
	if len(request.Body) != 0 || request.ContentLength {
		if err := parsePart(&parser, contentLengthPrefix); err != nil {
			return err
		}
		if err := parseDecimal(&parser, uint64(len(request.Body))); err != nil {
			return err
		}
		if err := parsePart(&parser, crlf); err != nil {
			return err
		}
	}
	if err := parsePart(&parser, crlf); err != nil {
		return err
	}
	return nil
}

// EncodedLen returns the exact wire length after validating request.
func EncodedLen(request Request, limits http1.Limits) (int, error) {
	if err := Validate(request, limits); err != nil {
		return 0, err
	}
	return encodedLen(request), nil
}

// Encode writes one complete request into dst. It returns ErrShortBuffer
// without modifying dst when the destination is too small.
func Encode(dst []byte, request Request, limits http1.Limits) (int, error) {
	if err := Validate(request, limits); err != nil {
		return 0, err
	}
	size := encodedLen(request)
	if len(dst) < size {
		return 0, ErrShortBuffer
	}
	encoded := appendRequest(dst[:0], request)
	return len(encoded), nil
}

// Append validates and appends one complete request to dst. It may allocate
// only when dst lacks sufficient capacity.
func Append(dst []byte, request Request, limits http1.Limits) ([]byte, error) {
	if err := Validate(request, limits); err != nil {
		return dst, err
	}
	return appendRequest(dst, request), nil
}

// Write validates and writes one complete request. Short writes are retried;
// the first transport error is returned.
func Write(writer io.Writer, request Request, limits http1.Limits) error {
	if writer == nil {
		return ErrInvalidRequest
	}
	if err := Validate(request, limits); err != nil {
		return err
	}
	if err := writeAll(writer, request.Method); err != nil {
		return err
	}
	if err := writeAll(writer, space); err != nil {
		return err
	}
	if err := writeAll(writer, request.Target); err != nil {
		return err
	}
	if err := writeAll(writer, http11HostPrefix); err != nil {
		return err
	}
	if err := writeAll(writer, request.Host); err != nil {
		return err
	}
	if err := writeAll(writer, crlf); err != nil {
		return err
	}
	for _, header := range request.Headers {
		if err := writeAll(writer, header.Name); err != nil {
			return err
		}
		if err := writeAll(writer, colonSpace); err != nil {
			return err
		}
		if err := writeAll(writer, header.Value); err != nil {
			return err
		}
		if err := writeAll(writer, crlf); err != nil {
			return err
		}
	}
	if len(request.Body) != 0 || request.ContentLength {
		if err := writeAll(writer, contentLengthPrefix); err != nil {
			return err
		}
		if err := writeDecimal(writer, uint64(len(request.Body))); err != nil {
			return err
		}
		if err := writeAll(writer, crlf); err != nil {
			return err
		}
	}
	if err := writeAll(writer, crlf); err != nil {
		return err
	}
	return writeAll(writer, request.Body)
}

// Do allocates a finite read buffer and performs one synchronous exchange.
func (client Client) Do(stream Stream, request Request, callbacks *http1.Callbacks) (Response, error) {
	size := client.ReadBufferBytes
	if size == 0 {
		size = DefaultReadBufferBytes
	}
	return client.DoBuffer(stream, request, callbacks, make([]byte, int(size)))
}

// DoBuffer performs one synchronous exchange using caller-owned scratch space.
// Informational responses are delivered to callbacks and skipped until the
// final response. The method returns exactly at that final message boundary.
func (client Client) DoBuffer(stream Stream, request Request, callbacks *http1.Callbacks, buffer []byte) (Response, error) {
	if stream == nil {
		return Response{}, ErrInvalidStream
	}
	if len(buffer) == 0 {
		return Response{}, ErrEmptyReadBuffer
	}
	if err := Write(stream, request, client.RequestLimits); err != nil {
		return Response{}, err
	}

	var completed http1.Message
	var sawMessage bool
	wrapped := http1.Callbacks{}
	if callbacks != nil {
		wrapped = *callbacks
	}
	originalComplete := wrapped.MessageComplete
	wrapped.ResponseContext = func(uint64) (bool, bool) {
		return equalFold(request.Method, []byte("HEAD")), equalFold(request.Method, []byte("CONNECT"))
	}
	wrapped.MessageComplete = func(message http1.Message) {
		completed = message
		sawMessage = true
		if originalComplete != nil {
			originalComplete(message)
		}
	}
	parser := http1.NewParser(http1.Response, &wrapped, client.ResponseLimits)

	for {
		read, readErr := stream.Read(buffer)
		if read < 0 || read > len(buffer) {
			return Response{}, io.ErrShortBuffer
		}
		offset := 0
		for offset < read {
			consumed, complete, code := parser.ParseOne(buffer[offset:read])
			offset += consumed
			if code != http1.CodeNone && code != http1.CodeUpgrade {
				return Response{}, &ParseError{Code: code}
			}
			if complete && sawMessage && finalResponse(completed.Status) {
				return Response{
					Message:  completed,
					Buffered: buffer[offset:read:read],
					Upgraded: code == http1.CodeUpgrade,
				}, nil
			}
			if code == http1.CodeUpgrade {
				return Response{}, &ParseError{Code: code}
			}
			if consumed == 0 && !complete {
				return Response{}, io.ErrNoProgress
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				return Response{}, readErr
			}
			code := parser.Finish()
			if code != http1.CodeNone && code != http1.CodeUpgrade {
				return Response{}, &ParseError{Code: code}
			}
			if sawMessage && finalResponse(completed.Status) {
				return Response{Message: completed, Upgraded: code == http1.CodeUpgrade}, nil
			}
			return Response{}, io.ErrUnexpectedEOF
		}
		if read == 0 {
			return Response{}, io.ErrNoProgress
		}
	}
}

var (
	space               = []byte(" ")
	crlf                = []byte("\r\n")
	colonSpace          = []byte(": ")
	http11HostPrefix    = []byte(" HTTP/1.1\r\nHost: ")
	contentLengthPrefix = []byte("Content-Length: ")
	digitData           = []byte("0123456789")
)

func parsePart(parser *http1.Parser, part []byte) error {
	consumed, code := parser.Parse(part)
	if code != http1.CodeNone || consumed != len(part) {
		if code == http1.CodeNone {
			return ErrInvalidRequest
		}
		return &ValidationError{Code: code}
	}
	return nil
}

func parseDecimal(parser *http1.Parser, value uint64) error {
	divisor := uint64(1)
	for quotient := value / 10; quotient != 0; quotient /= 10 {
		divisor *= 10
	}
	for {
		digit := value / divisor
		if err := parsePart(parser, digitData[digit:digit+1]); err != nil {
			return err
		}
		if divisor == 1 {
			return nil
		}
		value %= divisor
		divisor /= 10
	}
}

func encodedLen(request Request) int {
	size := len(request.Method) + 1 + len(request.Target) + len(http11HostPrefix) + len(request.Host) + len(crlf)
	for _, header := range request.Headers {
		size += len(header.Name) + len(colonSpace) + len(header.Value) + len(crlf)
	}
	if len(request.Body) != 0 || request.ContentLength {
		size += len(contentLengthPrefix) + decimalLen(uint64(len(request.Body))) + len(crlf)
	}
	return size + len(crlf) + len(request.Body)
}

func appendRequest(dst []byte, request Request) []byte {
	dst = append(dst, request.Method...)
	dst = append(dst, ' ')
	dst = append(dst, request.Target...)
	dst = append(dst, http11HostPrefix...)
	dst = append(dst, request.Host...)
	dst = append(dst, crlf...)
	for _, header := range request.Headers {
		dst = append(dst, header.Name...)
		dst = append(dst, colonSpace...)
		dst = append(dst, header.Value...)
		dst = append(dst, crlf...)
	}
	if len(request.Body) != 0 || request.ContentLength {
		dst = append(dst, contentLengthPrefix...)
		dst = strconv.AppendUint(dst, uint64(len(request.Body)), 10)
		dst = append(dst, crlf...)
	}
	dst = append(dst, crlf...)
	return append(dst, request.Body...)
}

func writeDecimal(writer io.Writer, value uint64) error {
	divisor := uint64(1)
	for quotient := value / 10; quotient != 0; quotient /= 10 {
		divisor *= 10
	}
	for {
		digit := value / divisor
		if err := writeAll(writer, digitData[digit:digit+1]); err != nil {
			return err
		}
		if divisor == 1 {
			return nil
		}
		value %= divisor
		divisor /= 10
	}
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

func reservedHeader(name []byte) bool {
	return equalFold(name, []byte("host")) || equalFold(name, []byte("content-length")) || equalFold(name, []byte("transfer-encoding"))
}

func validToken(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, b := range value {
		if b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' {
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

func validFieldValue(value []byte) bool {
	for _, b := range value {
		if b != '\t' && (b < 0x20 || b == 0x7f) {
			return false
		}
	}
	return true
}

func equalFold(left, right []byte) bool {
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

func decimalLen(value uint64) int {
	length := 1
	for value >= 10 {
		value /= 10
		length++
	}
	return length
}

func finalResponse(status uint16) bool {
	return status == 101 || status >= 200
}
