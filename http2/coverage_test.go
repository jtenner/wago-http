package http2

import (
	"bytes"
	"errors"
	"testing"

	wagohttp "github.com/wago-org/http"
	wago "github.com/wago-org/wago"
	"golang.org/x/net/http2/hpack"
)

func TestCoverageParserControlPaths(t *testing.T) {
	parser := NewParser(nil, Limits{MaxFrameSize: maxFrameSize + 1})
	if parser.limits.MaxFrameSize != maxFrameSize || parser.Finish() != CodeNone {
		t.Fatalf("normalized limits=%+v finish=%v", parser.limits, parser.Finish())
	}

	wire := joinFrames(t,
		frameCase{FrameHeader{Type: FramePing}, []byte("12345678")},
		frameCase{FrameHeader{Type: FramePing, Flags: FlagACK}, []byte("abcdefgh")},
	)
	n, complete, code := parser.ParseOne(wire)
	if n != 17 || !complete || code != CodeNone {
		t.Fatalf("ParseOne=%d,%t,%v", n, complete, code)
	}
	if n, complete, code = parser.ParseOne(wire[n:]); n != 17 || !complete || code != CodeNone {
		t.Fatalf("second ParseOne=%d,%t,%v", n, complete, code)
	}

	var reentrant Parser
	reentrant.Init(&Callbacks{FrameBegin: func(FrameHeader) { _ = reentrant.Finish() }}, Limits{})
	if _, code := reentrant.Parse(mustFrame(t, FrameHeader{Type: FrameSettings}, nil)); code != CodeReentrantCall {
		t.Fatalf("frame-begin callback reentrancy=%v", code)
	}
	reentrant.Init(&Callbacks{SettingsEnd: func(bool) { _, _ = reentrant.Parse(nil) }}, Limits{})
	if _, code := reentrant.Parse(mustFrame(t, FrameHeader{Type: FrameSettings}, nil)); code != CodeReentrantCall {
		t.Fatalf("zero-payload completion reentrancy=%v", code)
	}

	reentrant.Init(&Callbacks{Data: func(uint32, []byte, bool) { _ = reentrant.Finish() }}, Limits{})
	if _, code := reentrant.Parse(mustFrame(t, FrameHeader{Type: FrameData, StreamID: 1}, []byte("x"))); code != CodeReentrantCall {
		t.Fatalf("payload callback reentrancy=%v", code)
	}

	parser.Init(nil, Limits{})
	parser.fail(CodeInvalidPadding)
	if parser.Finish() != CodeInvalidPadding {
		t.Fatal("Finish did not preserve sticky error")
	}
	parser.Init(nil, Limits{})
	parser.current = FrameHeader{Type: FrameHeaders, StreamID: 1, Length: 1}
	parser.state = payloadContent
	parser.contentN = 1
	if n, done, code := parser.parseHeaderContentAndPadding([]byte{0x82}, 1); n != 1 || done || code != CodeNone {
		t.Fatalf("empty available header content=%d,%t,%v", n, done, code)
	}
	parser.contentN = 0
	if n, done, code := parser.parseHeaderContentAndPadding(nil, 0); n != 0 || done || code != CodeNone || parser.state != payloadPadding {
		t.Fatalf("empty header content transition=%d,%t,%v,state=%d", n, done, code, parser.state)
	}
	if n, done, code := parser.parseHeaderFragment(nil, 0, 0); n != 0 || !done || code != CodeNone {
		t.Fatalf("empty header fragment=%d,%t,%v", n, done, code)
	}

	prefix := []byte("prefix")
	if got, code := AppendFrame(prefix, FrameHeader{Length: 1, Type: FrameData, StreamID: 1}, []byte("xx")); code != CodeInvalidFrameSize || !bytes.Equal(got, prefix) {
		t.Fatalf("length mismatch=%q,%v", got, code)
	}
	if got, code := AppendFrame(prefix, FrameHeader{Type: 0xff, StreamID: 0x80000000}, nil); code != CodeInvalidFrameSize || !bytes.Equal(got, prefix) {
		t.Fatalf("large stream ID=%q,%v", got, code)
	}
}

func TestCoverageHeaderDecoderStickyOutputError(t *testing.T) {
	decoder := NewHeaderDecoder(HeaderLimits{MaxHeaders: 1}, nil)
	if err := decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Write([]byte{0x82, 0x84}); !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("first limit error=%v", err)
	}
	if n, err := decoder.Write([]byte{0x86}); n != 0 || !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("sticky limit error=%d,%v", n, err)
	}
	decoder.emitField(hpack.HeaderField{Name: "ignored", Value: "ignored"})
	if !errors.Is(decoder.err, ErrTooManyHeaders) {
		t.Fatalf("emit changed sticky error=%v", decoder.err)
	}
}

func TestCoverageHTTP2Registration(t *testing.T) {
	network := wagohttp.New()
	if err := Register(network); err != nil {
		t.Fatal(err)
	}
	runtime := wago.NewRuntime()
	if err := runtime.Use(network); err != nil {
		t.Fatal(err)
	}
	if err := Register(network); !errors.Is(err, wagohttp.ErrProtocolRegistrationFrozen) {
		t.Fatalf("frozen registration=%v", err)
	}
}
