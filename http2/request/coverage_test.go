package request

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	h2 "github.com/wago-org/http/http2"
)

func TestCoveragePublicErrorAndIOPaths(t *testing.T) {
	if got := (&ValidationError{Reason: "reason"}).Error(); got != ErrInvalidRequest.Error()+": reason" {
		t.Fatalf("reason error=%q", got)
	}
	streamErr := &StreamError{Code: h2.ErrCodeRefusedStream, Retryable: true}
	if !errors.Is(streamErr, ErrStreamReset) || streamErr.Error() == "" {
		t.Fatalf("stream error=%v", streamErr)
	}
	goAway := &GoAwayError{LastStreamID: 3, Code: h2.ErrCodeNo}
	if !errors.Is(goAway, ErrGoAway) || goAway.Error() == "" {
		t.Fatalf("goaway=%v", goAway)
	}

	invalid := basicRequest()
	invalid.Method = nil
	withReplay := basicRequest()
	withReplay.ReplayBody = func() (io.Reader, error) { return bytes.NewReader(nil), nil }
	if err := (Client{}).Validate(withReplay); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ReplayBody validation=%v", err)
	}
	if _, err := (Client{}).Encode(nil, invalid); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Encode invalid=%v", err)
	}
	wire, err := (Client{}).Append(nil, basicRequest())
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, len(wire))
	if n, err := (Client{}).Encode(dst, basicRequest()); err != nil || n != len(wire) || !bytes.Equal(dst, wire) {
		t.Fatalf("Encode=%d,%v", n, err)
	}
	if err := (Client{}).Write(nil, basicRequest()); !errors.Is(err, ErrInvalidStream) {
		t.Fatalf("Write nil=%v", err)
	}
	if err := (Client{}).Write(io.Discard, invalid); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Write invalid=%v", err)
	}
	if _, err := (Client{}).Do(nil, basicRequest(), nil); !errors.Is(err, ErrInvalidStream) {
		t.Fatalf("Do nil=%v", err)
	}
	if _, err := (Client{}).DoBuffer(&scriptedStream{}, invalid, nil, make([]byte, 1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("DoBuffer invalid request=%v", err)
	}

	for _, test := range []struct {
		name string
		read func([]byte) (int, error)
		want error
	}{
		{"negative-read", func([]byte) (int, error) { return -1, nil }, io.ErrShortBuffer},
		{"oversized-read", func(dst []byte) (int, error) { return len(dst) + 1, nil }, io.ErrShortBuffer},
		{"read-error", func([]byte) (int, error) { return 0, errors.New("read failure") }, errors.New("read failure")},
		{"eof-empty", func([]byte) (int, error) { return 0, io.EOF }, io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &functionStream{read: test.read}
			_, err := (Client{}).DoBuffer(stream, basicRequest(), nil, make([]byte, 8))
			if test.name == "read-error" {
				if err == nil || err.Error() != test.want.Error() {
					t.Fatalf("error=%v", err)
				}
			} else if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}

	partial := mustCoverageFrame(t, h2.FrameHeader{Type: h2.FrameSettings}, nil)
	partial = append(partial, 0, 0, 8)
	stream := &functionStream{read: oneRead(partial, io.EOF)}
	if _, err := (Client{}).DoBuffer(stream, basicRequest(), nil, make([]byte, len(partial))); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("truncated frame=%v", err)
	}
}

func TestCoverageValidationSpecificLimits(t *testing.T) {
	request := basicRequest()
	request.Method = []byte("LONG")
	if err := (Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 3}}).Validate(request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("pseudo field limit=%v", err)
	}
	request = basicRequest()
	request.Headers = []Header{{Name: []byte("x"), Value: []byte("long")}}
	if err := (Client{HeaderLimits: h2.HeaderLimits{MaxFieldBytes: 3}}).Validate(request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("regular field limit=%v", err)
	}
	request = basicRequest()
	request.Body = []byte("x")
	if err := (Client{HeaderLimits: h2.HeaderLimits{MaxHeaders: 4}}).Validate(request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("generated content-length count=%v", err)
	}
	if got := headerListLimit(h2.HeaderLimits{MaxHeaderListBytes: math.MaxUint64}); got != math.MaxUint32 {
		t.Fatalf("clamped header list=%d", got)
	}
}

func TestCoverageResponseStateDefensiveBranches(t *testing.T) {
	sentinel := errors.New("sentinel")
	state := responseState{err: sentinel}
	state.onFrameBegin(h2.FrameHeader{})
	state.onHeaderBlock(1, nil)
	state.onHeader(h2.HeaderField{Name: "x", Value: "y"})
	state.onHeaderBlockEnd(1, true)
	state.onData(1, nil, false)
	state.onFrameComplete(h2.FrameHeader{Type: h2.FrameData, StreamID: 1})
	state.onPing([8]byte{}, false)
	state.finishResponse()
	if !errors.Is(state.err, sentinel) {
		t.Fatalf("sticky response error=%v", state.err)
	}

	state = responseState{decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	if err := state.decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	state.onFrameBegin(h2.FrameHeader{Type: h2.FrameHeaders, StreamID: 1})
	if state.err == nil {
		t.Fatal("active decoder BeginBlock was accepted")
	}

	state = responseState{decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	state.onHeaderBlock(3, nil)
	if !errors.Is(state.err, ErrInvalidResponse) {
		t.Fatalf("wrong header stream=%v", state.err)
	}

	state = responseState{blockActive: true, decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	if err := state.decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	state.onHeaderBlock(1, []byte{0x80})
	if state.err == nil {
		t.Fatal("invalid HPACK representation was accepted")
	}

	state = responseState{blockActive: true, decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	if err := state.decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := state.decoder.Write([]byte{0x40}); err != nil {
		t.Fatal(err)
	}
	state.onHeaderBlockEnd(1, true)
	if state.err == nil {
		t.Fatal("truncated HPACK block accepted")
	}

	state = responseState{decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	state.onHeaderBlockEnd(3, true)
	if !errors.Is(state.err, ErrInvalidResponse) {
		t.Fatalf("wrong header-block end stream=%v", state.err)
	}

	state = responseState{blockActive: true, blockTrailer: true, decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	if err := state.decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	state.onHeaderBlockEnd(1, false)
	if !errors.Is(state.err, ErrInvalidResponse) {
		t.Fatalf("non-ending trailer=%v", state.err)
	}

	state = responseState{blockActive: true, blockStatus: 103, blockSawStatus: true, decoder: h2.NewHeaderDecoder(h2.HeaderLimits{}, nil)}
	if err := state.decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	state.onHeaderBlockEnd(1, true)
	if !errors.Is(state.err, ErrInvalidResponse) {
		t.Fatalf("ending informational response=%v", state.err)
	}

	state = responseState{finalHeaders: true}
	state.onData(3, []byte("x"), false)
	if !errors.Is(state.err, ErrInvalidResponse) {
		t.Fatalf("wrong DATA stream=%v", state.err)
	}

	for failCall := 1; failCall <= 2; failCall++ {
		writer := &callFailWriter{failCall: failCall}
		state = responseState{stream: &writerStream{Writer: writer}}
		state.onFrameComplete(h2.FrameHeader{Type: h2.FrameData, StreamID: 1, Length: 1})
		if state.err == nil {
			t.Fatalf("WINDOW_UPDATE write %d succeeded", failCall)
		}
	}

	state = responseState{}
	state.onPing([8]byte{}, true)
	if state.err != nil {
		t.Fatalf("ACK PING produced error=%v", state.err)
	}
	state = responseState{complete: true}
	state.finishResponse()
	if !state.complete || state.err != nil {
		t.Fatalf("idempotent finish=%+v", state)
	}
}

func TestCoverageHelpersAndWriters(t *testing.T) {
	if err := writeFrame(io.Discard, h2.FrameHeader{Type: h2.FramePing}, []byte("short")); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("invalid writeFrame=%v", err)
	}
	for _, writer := range []io.Writer{
		invalidCountWriter(-1), invalidCountWriter(2),
		&zeroWriter{}, &errorWriter{write: 1}, &errorWriter{write: 0},
	} {
		if err := writeAll(writer, []byte("x")); err == nil {
			t.Fatalf("writer %T unexpectedly succeeded", writer)
		}
	}

	if validMethod([]byte{'('}) || validMethod([]byte{0x7f}) {
		t.Fatal("invalid methods accepted")
	}
	if validScheme([]byte("a_")) {
		t.Fatal("invalid scheme accepted")
	}
	if validDecodedName("") || validDecodedName(":") || validDecodedName("x(") {
		t.Fatal("invalid decoded names accepted")
	}
	for _, value := range []string{"", "x", "18446744073709551616"} {
		if _, ok := parseContentLength(value); ok {
			t.Fatalf("invalid content length %q accepted", value)
		}
	}
	if equalASCII([]byte("x"), []byte("xy")) || equalASCII([]byte("x"), []byte("y")) || !equalASCII([]byte("HeAd"), []byte("hEaD")) {
		t.Fatal("equalASCII mismatch")
	}
}

func TestCoverageControlWriteFailures(t *testing.T) {
	settings := mustCoverageFrame(t, h2.FrameHeader{Type: h2.FrameSettings}, nil)
	ping := mustCoverageFrame(t, h2.FrameHeader{Type: h2.FramePing}, []byte("12345678"))
	for _, test := range []struct {
		wire     []byte
		failCall int
	}{
		{settings, 2},
		{append(append([]byte(nil), settings...), ping...), 3},
	} {
		stream := &failAfterRequestStream{reads: [][]byte{test.wire}, failCall: test.failCall}
		_, err := (Client{}).DoBuffer(stream, basicRequest(), nil, make([]byte, len(test.wire)))
		if err == nil {
			t.Fatal("control write failure was ignored")
		}
	}
}

type functionStream struct {
	writes bytes.Buffer
	read   func([]byte) (int, error)
}

func (stream *functionStream) Write(src []byte) (int, error) { return stream.writes.Write(src) }
func (stream *functionStream) Read(dst []byte) (int, error)  { return stream.read(dst) }

func oneRead(data []byte, final error) func([]byte) (int, error) {
	read := false
	return func(dst []byte) (int, error) {
		if read {
			return 0, final
		}
		read = true
		return copy(dst, data), final
	}
}

type invalidCountWriter int

func (writer invalidCountWriter) Write([]byte) (int, error) { return int(writer), nil }

type zeroWriter struct{}

func (*zeroWriter) Write([]byte) (int, error) { return 0, nil }

type errorWriter struct{ write int }

func (writer *errorWriter) Write([]byte) (int, error) {
	return writer.write, errors.New("write failure")
}

type callFailWriter struct {
	calls    int
	failCall int
}

func (writer *callFailWriter) Write(src []byte) (int, error) {
	writer.calls++
	if writer.calls == writer.failCall {
		return 0, errors.New("write failure")
	}
	return len(src), nil
}

type writerStream struct{ io.Writer }

func (*writerStream) Read([]byte) (int, error) { return 0, io.EOF }

type failAfterRequestStream struct {
	writes   int
	failCall int
	reads    [][]byte
	index    int
}

func (stream *failAfterRequestStream) Write(src []byte) (int, error) {
	stream.writes++
	if stream.writes == stream.failCall {
		return 0, errors.New("control write failure")
	}
	return len(src), nil
}

func (stream *failAfterRequestStream) Read(dst []byte) (int, error) {
	if stream.index == len(stream.reads) {
		return 0, io.EOF
	}
	n := copy(dst, stream.reads[stream.index])
	stream.index++
	return n, nil
}

func mustCoverageFrame(t *testing.T, header h2.FrameHeader, payload []byte) []byte {
	t.Helper()
	wire, code := h2.AppendFrame(nil, header, payload)
	if code != h2.CodeNone {
		t.Fatal(code)
	}
	return wire
}
