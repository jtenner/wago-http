package http2

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"
)

func TestParserValidFrameCorpusEverySplit(t *testing.T) {
	settings := append(settingPayload(SettingHeaderTableSize, 4096), settingPayload(SettingMaxFrameSize, 32768)...)
	cases := []struct {
		name    string
		header  FrameHeader
		payload []byte
	}{
		{"empty-data", FrameHeader{Type: FrameData, Flags: FlagEndStream, StreamID: 1}, nil},
		{"data", FrameHeader{Type: FrameData, StreamID: 1}, []byte("payload")},
		{"padded-data", FrameHeader{Type: FrameData, Flags: FlagPadded | FlagEndStream, StreamID: 1}, append([]byte{3}, append([]byte("body"), 0, 0, 0)...)},
		{"headers", FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders, StreamID: 1}, []byte{0x82, 0x84}},
		{"headers-priority", FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders | FlagPriority, StreamID: 3}, append(priorityPayload(1, true, 42), 0x88)},
		{"headers-padded-priority", FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders | FlagPriority | FlagPadded, StreamID: 5}, append([]byte{2}, append(append(priorityPayload(1, false, 7), 0x88), 0, 0)...)},
		{"priority", FrameHeader{Type: FramePriority, StreamID: 3}, priorityPayload(1, true, 255)},
		{"rst-stream", FrameHeader{Type: FrameRSTStream, StreamID: 1}, uint32Payload(uint32(ErrCodeCancel))},
		{"settings", FrameHeader{Type: FrameSettings}, settings},
		{"settings-ack", FrameHeader{Type: FrameSettings, Flags: FlagACK}, nil},
		{"push-promise", FrameHeader{Type: FramePushPromise, Flags: FlagEndHeaders, StreamID: 1}, append(uint32Payload(2), 0x82)},
		{"push-promise-padded", FrameHeader{Type: FramePushPromise, Flags: FlagEndHeaders | FlagPadded, StreamID: 1}, append([]byte{1}, append(append(uint32Payload(2), 0x82), 0)...)},
		{"ping", FrameHeader{Type: FramePing}, []byte("12345678")},
		{"ping-ack", FrameHeader{Type: FramePing, Flags: FlagACK}, []byte("abcdefgh")},
		{"goaway", FrameHeader{Type: FrameGoAway}, append(append(uint32Payload(7), uint32Payload(uint32(ErrCodeEnhanceYourCalm))...), []byte("debug")...)},
		{"window-connection", FrameHeader{Type: FrameWindowUpdate}, uint32Payload(1)},
		{"window-stream", FrameHeader{Type: FrameWindowUpdate, StreamID: 9}, uint32Payload(0x7fffffff)},
		{"unknown-empty", FrameHeader{Type: 0xf0, StreamID: 11}, nil},
		{"unknown", FrameHeader{Type: 0xfe, Flags: 0xff, StreamID: 11}, []byte("extension")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			wire := mustFrame(t, test.header, test.payload)
			want := parseTrace(t, wire, []int{len(wire)})
			for split := 0; split <= len(wire); split++ {
				got := parseTrace(t, wire, []int{split, len(wire) - split})
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("split %d trace:\n got %q\nwant %q", split, got, want)
				}
			}
			got := parseTrace(t, wire, bytesToChunks(len(wire)))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("byte trace:\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestParserContinuationCorpus(t *testing.T) {
	sequence := joinFrames(t,
		frameCase{FrameHeader{Type: FrameHeaders, Flags: FlagEndStream, StreamID: 1}, []byte("abc")},
		frameCase{FrameHeader{Type: FrameContinuation, StreamID: 1}, []byte("def")},
		frameCase{FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: 1}, []byte("ghi")},
		frameCase{FrameHeader{Type: FrameData, Flags: FlagEndStream, StreamID: 3}, []byte("next")},
	)
	var blocks []string
	var ends []string
	parser := NewParser(&Callbacks{
		HeaderBlock: func(streamID uint32, fragment []byte) {
			blocks = append(blocks, fmt.Sprintf("%d:%s", streamID, fragment))
		},
		HeaderBlockEnd: func(streamID uint32, endStream bool) { ends = append(ends, fmt.Sprintf("%d:%t", streamID, endStream)) },
	}, Limits{})
	for _, b := range sequence {
		n, code := parser.Parse([]byte{b})
		if n != 1 || code != CodeNone {
			t.Fatalf("Parse byte = %d, %v", n, code)
		}
	}
	if code := parser.Finish(); code != CodeNone {
		t.Fatal(code)
	}
	if got, want := blocks, []string{"1:a", "1:b", "1:c", "1:d", "1:e", "1:f", "1:g", "1:h", "1:i"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks = %q, want %q", got, want)
	}
	if got, want := ends, []string{"1:true"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ends = %q, want %q", got, want)
	}
}

func TestParserMalformedCorpus(t *testing.T) {
	cases := []struct {
		name   string
		wire   []byte
		limits Limits
		want   Code
	}{
		{"data-stream-zero", rawFrame(FrameHeader{Type: FrameData}, nil), Limits{}, CodeInvalidStreamID},
		{"headers-stream-zero", rawFrame(FrameHeader{Type: FrameHeaders}, nil), Limits{}, CodeInvalidStreamID},
		{"priority-stream-zero", rawFrame(FrameHeader{Type: FramePriority}, make([]byte, 5)), Limits{}, CodeInvalidStreamID},
		{"rst-stream-zero", rawFrame(FrameHeader{Type: FrameRSTStream}, make([]byte, 4)), Limits{}, CodeInvalidStreamID},
		{"push-stream-zero", rawFrame(FrameHeader{Type: FramePushPromise}, make([]byte, 4)), Limits{}, CodeInvalidStreamID},
		{"continuation-stream-zero", rawFrame(FrameHeader{Type: FrameContinuation}, nil), Limits{}, CodeInvalidContinuation},
		{"settings-stream", rawFrame(FrameHeader{Type: FrameSettings, StreamID: 1}, nil), Limits{}, CodeInvalidStreamID},
		{"ping-stream", rawFrame(FrameHeader{Type: FramePing, StreamID: 1}, make([]byte, 8)), Limits{}, CodeInvalidStreamID},
		{"goaway-stream", rawFrame(FrameHeader{Type: FrameGoAway, StreamID: 1}, make([]byte, 8)), Limits{}, CodeInvalidStreamID},
		{"priority-size", rawFrame(FrameHeader{Type: FramePriority, StreamID: 1}, make([]byte, 4)), Limits{}, CodeInvalidFrameSize},
		{"rst-size", rawFrame(FrameHeader{Type: FrameRSTStream, StreamID: 1}, make([]byte, 5)), Limits{}, CodeInvalidFrameSize},
		{"settings-size", rawFrame(FrameHeader{Type: FrameSettings}, make([]byte, 5)), Limits{}, CodeInvalidFrameSize},
		{"settings-ack-payload", rawFrame(FrameHeader{Type: FrameSettings, Flags: FlagACK}, make([]byte, 6)), Limits{}, CodeInvalidFrameSize},
		{"ping-size", rawFrame(FrameHeader{Type: FramePing}, make([]byte, 7)), Limits{}, CodeInvalidFrameSize},
		{"goaway-size", rawFrame(FrameHeader{Type: FrameGoAway}, make([]byte, 7)), Limits{}, CodeInvalidFrameSize},
		{"window-size", rawFrame(FrameHeader{Type: FrameWindowUpdate}, make([]byte, 3)), Limits{}, CodeInvalidFrameSize},
		{"data-padded-empty", rawFrame(FrameHeader{Type: FrameData, Flags: FlagPadded, StreamID: 1}, nil), Limits{}, CodeInvalidFrameSize},
		{"headers-short-priority", rawFrame(FrameHeader{Type: FrameHeaders, Flags: FlagPriority, StreamID: 1}, make([]byte, 4)), Limits{}, CodeInvalidFrameSize},
		{"push-short", rawFrame(FrameHeader{Type: FramePushPromise, StreamID: 1}, make([]byte, 3)), Limits{}, CodeInvalidFrameSize},
		{"data-padding-overflow", rawFrame(FrameHeader{Type: FrameData, Flags: FlagPadded, StreamID: 1}, []byte{2, 0}), Limits{}, CodeInvalidPadding},
		{"headers-padding-overflow", rawFrame(FrameHeader{Type: FrameHeaders, Flags: FlagPadded, StreamID: 1}, []byte{1}), Limits{}, CodeInvalidPadding},
		{"push-padding-overflow", rawFrame(FrameHeader{Type: FramePushPromise, Flags: FlagPadded, StreamID: 1}, append([]byte{1}, make([]byte, 4)...)), Limits{}, CodeInvalidPadding},
		{"priority-self", rawFrame(FrameHeader{Type: FramePriority, StreamID: 3}, priorityPayload(3, false, 0)), Limits{}, CodeInvalidPriority},
		{"headers-priority-self", rawFrame(FrameHeader{Type: FrameHeaders, Flags: FlagPriority, StreamID: 3}, priorityPayload(3, false, 0)), Limits{}, CodeInvalidPriority},
		{"push-promised-zero", rawFrame(FrameHeader{Type: FramePushPromise, Flags: FlagEndHeaders, StreamID: 1}, make([]byte, 4)), Limits{}, CodeInvalidStreamID},
		{"window-zero", rawFrame(FrameHeader{Type: FrameWindowUpdate}, make([]byte, 4)), Limits{}, CodeInvalidWindowUpdate},
		{"enable-push-two", rawFrame(FrameHeader{Type: FrameSettings}, settingPayload(SettingEnablePush, 2)), Limits{}, CodeInvalidSetting},
		{"initial-window-large", rawFrame(FrameHeader{Type: FrameSettings}, settingPayload(SettingInitialWindowSize, 0x80000000)), Limits{}, CodeInvalidSetting},
		{"max-frame-small", rawFrame(FrameHeader{Type: FrameSettings}, settingPayload(SettingMaxFrameSize, 16383)), Limits{}, CodeInvalidSetting},
		{"max-frame-large", rawFrame(FrameHeader{Type: FrameSettings}, settingPayload(SettingMaxFrameSize, 1<<24)), Limits{}, CodeInvalidSetting},
		{"connect-protocol-two", rawFrame(FrameHeader{Type: FrameSettings}, settingPayload(SettingEnableConnectProtocol, 2)), Limits{}, CodeInvalidSetting},
		{"priorities-two", rawFrame(FrameHeader{Type: FrameSettings}, settingPayload(SettingNoRFC7540Priorities, 2)), Limits{}, CodeInvalidSetting},
		{"unexpected-continuation", rawFrame(FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: 1}, nil), Limits{}, CodeInvalidContinuation},
		{"interleaved-data", joinFrames(t, frameCase{FrameHeader{Type: FrameHeaders, StreamID: 1}, nil}, frameCase{FrameHeader{Type: FrameData, StreamID: 1}, nil}), Limits{}, CodeInvalidContinuation},
		{"wrong-continuation-stream", joinFrames(t, frameCase{FrameHeader{Type: FrameHeaders, StreamID: 1}, nil}, frameCase{FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: 3}, nil}), Limits{}, CodeInvalidContinuation},
		{"frame-limit", rawFrame(FrameHeader{Type: 0xff}, bytes.Repeat([]byte{'x'}, 17)), Limits{MaxFrameSize: 16}, CodeFrameTooLarge},
		{"header-block-limit", rawFrame(FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders, StreamID: 1}, []byte("12345")), Limits{MaxHeaderBlockBytes: 4}, CodeHeaderBlockTooLarge},
		{"continuation-limit", joinFrames(t, frameCase{FrameHeader{Type: FrameHeaders, StreamID: 1}, nil}, frameCase{FrameHeader{Type: FrameContinuation, StreamID: 1}, nil}, frameCase{FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: 1}, nil}), Limits{MaxContinuationFrames: 1}, CodeTooManyContinuations},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			for _, chunks := range [][]int{{len(test.wire)}, bytesToChunks(len(test.wire)), {maxInt(0, len(test.wire)-1), 1}} {
				parser := NewParser(nil, test.limits)
				offset := 0
				var got Code
				for _, size := range chunks {
					if offset+size > len(test.wire) {
						size = len(test.wire) - offset
					}
					_, got = parser.Parse(test.wire[offset : offset+size])
					offset += size
					if got != CodeNone {
						break
					}
				}
				if got != test.want {
					t.Fatalf("chunks %v code = %v, want %v", chunks, got, test.want)
				}
			}
		})
	}
}

func TestParserTruncationStickyResetAndReservedBit(t *testing.T) {
	wire := mustFrame(t, FrameHeader{Type: FramePing}, []byte("12345678"))
	for end := 1; end < len(wire); end++ {
		parser := NewParser(nil, Limits{})
		if _, code := parser.Parse(wire[:end]); code != CodeNone {
			t.Fatalf("end %d Parse = %v", end, code)
		}
		if code := parser.Finish(); code != CodeUnexpectedEOF {
			t.Fatalf("end %d Finish = %v", end, code)
		}
		if n, code := parser.Parse(wire); n != 0 || code != CodeUnexpectedEOF {
			t.Fatalf("sticky = %d, %v", n, code)
		}
		parser.Reset()
		if n, code := parser.Parse(wire); n != len(wire) || code != CodeNone {
			t.Fatalf("after reset = %d, %v", n, code)
		}
	}

	reserved := append([]byte(nil), wire...)
	reserved[5] |= 0x80
	parser := NewParser(nil, Limits{})
	if n, code := parser.Parse(reserved); n != len(reserved) || code != CodeNone {
		t.Fatalf("reserved stream bit = %d, %v", n, code)
	}
}

func TestParserParseOneAndCallbackSafety(t *testing.T) {
	wire := joinFrames(t,
		frameCase{FrameHeader{Type: FramePing}, []byte("12345678")},
		frameCase{FrameHeader{Type: FramePing, Flags: FlagACK}, []byte("abcdefgh")},
	)
	var parser Parser
	var capsOK = true
	callbacks := Callbacks{Ping: func(_ [8]byte, _ bool) {
		if n, code := parser.Parse(nil); n != 0 || code != CodeReentrantCall {
			capsOK = false
		}
	}}
	parser.Init(&callbacks, Limits{})
	n, complete, code := parser.ParseOne(wire)
	if code != CodeReentrantCall || complete || n != 17 || !capsOK {
		t.Fatalf("ParseOne = %d, %t, %v, callback=%t", n, complete, code, capsOK)
	}
	parser.Init(nil, Limits{})
	if n, code := parser.Parse(wire); n != len(wire) || code != CodeNone {
		t.Fatalf("Parse = %d, %v", n, code)
	}
}

func TestParserLifecycleReentrancyAndCallbackPanic(t *testing.T) {
	wire := mustFrame(t, FrameHeader{Type: FramePing}, []byte("12345678"))
	actions := []struct {
		name string
		run  func(*Parser) Code
	}{
		{"parse", func(parser *Parser) Code { _, code := parser.Parse(nil); return code }},
		{"finish", func(parser *Parser) Code { return parser.Finish() }},
		{"reset", func(parser *Parser) Code { parser.Reset(); return parser.code }},
		{"init", func(parser *Parser) Code { parser.Init(nil, Limits{}); return parser.code }},
	}
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			var parser Parser
			var inner Code
			parser.Init(&Callbacks{Ping: func([8]byte, bool) { inner = action.run(&parser) }}, Limits{})
			if _, code := parser.Parse(wire); code != CodeReentrantCall || inner != CodeReentrantCall {
				t.Fatalf("outer=%v inner=%v", code, inner)
			}
			parser.Init(nil, Limits{})
			if _, code := parser.Parse(wire); code != CodeNone {
				t.Fatalf("reuse after Init = %v", code)
			}
		})
	}

	parser := NewParser(&Callbacks{Ping: func([8]byte, bool) { panic("callback") }}, Limits{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("callback panic was not propagated")
			}
		}()
		_, _ = parser.Parse(wire)
	}()
	parser.Init(nil, Limits{})
	if _, code := parser.Parse(wire); code != CodeNone {
		t.Fatalf("reuse after panic and Init = %v", code)
	}
}

func TestCodeAndFrameTypeStrings(t *testing.T) {
	for code := CodeNone; code <= CodeUnexpectedEOF; code++ {
		if code.String() == "unknown HTTP/2 parser result" {
			t.Fatalf("missing string for code %d", code)
		}
	}
	if Code(255).String() != "unknown HTTP/2 parser result" {
		t.Fatal("unknown Code string")
	}
	for typ := FrameData; typ <= FrameContinuation; typ++ {
		if typ.String() == "UNKNOWN" {
			t.Fatalf("missing string for frame type %d", typ)
		}
	}
	if FrameType(255).String() != "UNKNOWN" {
		t.Fatal("unknown FrameType string")
	}
}

func TestParserBorrowedSpansAreCapLimited(t *testing.T) {
	wire := mustFrame(t, FrameHeader{Type: FrameData, StreamID: 1}, []byte("abcdef"))
	var spans int
	parser := NewParser(&Callbacks{Data: func(_ uint32, data []byte, _ bool) {
		spans++
		if cap(data) != len(data) {
			t.Fatalf("cap = %d, len = %d", cap(data), len(data))
		}
	}}, Limits{})
	for _, chunk := range [][]byte{wire[:10], wire[10:12], wire[12:]} {
		if _, code := parser.Parse(chunk); code != CodeNone {
			t.Fatal(code)
		}
	}
	if spans != 3 {
		t.Fatalf("spans = %d", spans)
	}
}

func TestAppendFrameValidationAndNoAllocParser(t *testing.T) {
	original := []byte("prefix")
	if got, code := AppendFrame(original, FrameHeader{Type: FramePing}, []byte("short")); code != CodeInvalidFrameSize || string(got) != "prefix" {
		t.Fatalf("invalid append = %q, %v", got, code)
	}
	wire := mustFrame(t, FrameHeader{Type: FrameData, StreamID: 1}, bytes.Repeat([]byte("x"), 1024))
	parser := NewParser(nil, Limits{})
	if allocations := testing.AllocsPerRun(1000, func() {
		parser.Reset()
		if n, code := parser.Parse(wire); n != len(wire) || code != CodeNone {
			panic(code)
		}
	}); allocations != 0 {
		t.Fatalf("allocations = %v, want 0", allocations)
	}
}

type frameCase struct {
	header  FrameHeader
	payload []byte
}

func parseTrace(t *testing.T, wire []byte, chunks []int) []string {
	t.Helper()
	var trace []string
	appendSpan := func(prefix string, data []byte) {
		encoded := fmt.Sprintf("%x", data)
		if len(trace) != 0 && len(trace[len(trace)-1]) >= len(prefix) && trace[len(trace)-1][:len(prefix)] == prefix {
			trace[len(trace)-1] += encoded
		} else {
			trace = append(trace, prefix+encoded)
		}
	}
	callbacks := Callbacks{
		FrameBegin: func(header FrameHeader) {
			trace = append(trace, fmt.Sprintf("begin:%s:%d:%d:%x", header.Type, header.StreamID, header.Length, header.Flags))
		},
		Data:           func(id uint32, data []byte, end bool) { appendSpan(fmt.Sprintf("data:%d:%t:", id, end), data) },
		HeaderBlock:    func(id uint32, data []byte) { appendSpan(fmt.Sprintf("header:%d:", id), data) },
		HeaderBlockEnd: func(id uint32, end bool) { trace = append(trace, fmt.Sprintf("header-end:%d:%t", id, end)) },
		Priority: func(id uint32, p PriorityParam) {
			trace = append(trace, fmt.Sprintf("priority:%d:%d:%t:%d", id, p.StreamDependency, p.Exclusive, p.Weight))
		},
		RSTStream:    func(id uint32, code ErrorCode) { trace = append(trace, fmt.Sprintf("rst:%d:%d", id, code)) },
		Setting:      func(setting Setting) { trace = append(trace, fmt.Sprintf("setting:%d:%d", setting.ID, setting.Value)) },
		SettingsEnd:  func(ack bool) { trace = append(trace, fmt.Sprintf("settings-end:%t", ack)) },
		PushPromise:  func(id, promised uint32) { trace = append(trace, fmt.Sprintf("push:%d:%d", id, promised)) },
		Ping:         func(data [8]byte, ack bool) { trace = append(trace, fmt.Sprintf("ping:%q:%t", data, ack)) },
		GoAway:       func(last uint32, code ErrorCode) { trace = append(trace, fmt.Sprintf("goaway:%d:%d", last, code)) },
		GoAwayDebug:  func(data []byte) { appendSpan("debug:", data) },
		WindowUpdate: func(id, inc uint32) { trace = append(trace, fmt.Sprintf("window:%d:%d", id, inc)) },
		Unknown: func(header FrameHeader, data []byte) {
			appendSpan(fmt.Sprintf("unknown:%x:", uint8(header.Type)), data)
		},
		FrameComplete: func(header FrameHeader) { trace = append(trace, "complete:"+header.Type.String()) },
	}
	parser := NewParser(&callbacks, Limits{})
	offset := 0
	for _, size := range chunks {
		if size < 0 || offset+size > len(wire) {
			t.Fatalf("bad chunks %v", chunks)
		}
		n, code := parser.Parse(wire[offset : offset+size])
		if code != CodeNone || n != size {
			t.Fatalf("Parse chunk %d = %d, %v", size, n, code)
		}
		offset += size
	}
	if offset != len(wire) {
		t.Fatalf("chunks consumed %d/%d", offset, len(wire))
	}
	if code := parser.Finish(); code != CodeNone {
		t.Fatal(code)
	}
	return trace
}

func mustFrame(t *testing.T, header FrameHeader, payload []byte) []byte {
	t.Helper()
	wire, code := AppendFrame(nil, header, payload)
	if code != CodeNone {
		t.Fatalf("AppendFrame: %v", code)
	}
	return wire
}

func rawFrame(header FrameHeader, payload []byte) []byte {
	header.Length = uint32(len(payload))
	wire := make([]byte, 9+len(payload))
	wire[0] = byte(header.Length >> 16)
	wire[1] = byte(header.Length >> 8)
	wire[2] = byte(header.Length)
	wire[3] = byte(header.Type)
	wire[4] = byte(header.Flags)
	binary.BigEndian.PutUint32(wire[5:9], header.StreamID)
	copy(wire[9:], payload)
	return wire
}

func joinFrames(t *testing.T, frames ...frameCase) []byte {
	t.Helper()
	var wire []byte
	for _, frame := range frames {
		var code Code
		wire, code = AppendFrame(wire, frame.header, frame.payload)
		if code != CodeNone {
			t.Fatalf("AppendFrame: %v", code)
		}
	}
	return wire
}

func settingPayload(id SettingID, value uint32) []byte {
	payload := make([]byte, 6)
	binary.BigEndian.PutUint16(payload[:2], uint16(id))
	binary.BigEndian.PutUint32(payload[2:], value)
	return payload
}

func priorityPayload(dependency uint32, exclusive bool, weight uint8) []byte {
	if exclusive {
		dependency |= 1 << 31
	}
	payload := make([]byte, 5)
	binary.BigEndian.PutUint32(payload[:4], dependency)
	payload[4] = weight
	return payload
}

func uint32Payload(value uint32) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, value)
	return payload
}

func bytesToChunks(length int) []int {
	chunks := make([]int, length)
	for i := range chunks {
		chunks[i] = 1
	}
	return chunks
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
