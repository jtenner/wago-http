package http2

import (
	"bytes"
	"testing"
)

func FuzzParserArbitraryFragmentation(f *testing.F) {
	seeds := [][]byte{
		nil,
		mustSeedFrame(FrameHeader{Type: FrameSettings}, nil),
		mustSeedFrame(FrameHeader{Type: FramePing}, []byte("12345678")),
		mustSeedFrame(FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders, StreamID: 1}, []byte{0x82, 0x84}),
		rawFrame(FrameHeader{Type: FrameData, Flags: FlagPadded, StreamID: 1}, []byte{255}),
		bytes.Repeat([]byte{0xff}, 64),
	}
	for _, seed := range seeds {
		f.Add(seed, uint8(1), uint16(16384), uint16(1024))
	}
	f.Fuzz(func(t *testing.T, data []byte, stride uint8, frameLimit uint16, headerLimit uint16) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		limits := Limits{
			MaxFrameSize:          uint32(frameLimit) + 1,
			MaxHeaderBlockBytes:   uint64(headerLimit) + 1,
			MaxContinuationFrames: 1 + uint32(stride%32),
		}
		parser := NewParser(nil, limits)
		step := 1 + int(stride%64)
		offset := 0
		for offset < len(data) {
			end := offset + step
			if end > len(data) {
				end = len(data)
			}
			n, code := parser.Parse(data[offset:end])
			if n < 0 || n > end-offset {
				t.Fatalf("consumed %d of %d", n, end-offset)
			}
			offset += n
			if code != CodeNone {
				if again, sticky := parser.Parse(data[offset:]); again != 0 || sticky != code {
					t.Fatalf("non-sticky failure: %d, %v; want 0, %v", again, sticky, code)
				}
				parser.Reset()
				return
			}
			if n == 0 && end != offset {
				t.Fatal("parser made no progress")
			}
		}
		_ = parser.Finish()
	})
}

func FuzzParserExtensionFrameRoundTrip(f *testing.F) {
	f.Add(uint32(1), uint8(0), []byte("payload"), uint8(1))
	f.Add(uint32(0x7fffffff), uint8(0xff), []byte{}, uint8(17))
	f.Fuzz(func(t *testing.T, streamID uint32, flags uint8, payload []byte, stride uint8) {
		if len(payload) > 1<<20 {
			t.Skip()
		}
		streamID &= 0x7fffffff
		header := FrameHeader{Type: 0xf0, Flags: Flags(flags), StreamID: streamID}
		wire, code := AppendFrame(nil, header, payload)
		if code != CodeNone {
			t.Fatalf("AppendFrame = %v", code)
		}
		var got []byte
		var completed int
		parser := NewParser(&Callbacks{
			Unknown:       func(_ FrameHeader, fragment []byte) { got = append(got, fragment...) },
			FrameComplete: func(FrameHeader) { completed++ },
		}, Limits{MaxFrameSize: uint32(len(payload))})
		step := 1 + int(stride%31)
		for offset := 0; offset < len(wire); {
			end := offset + step
			if end > len(wire) {
				end = len(wire)
			}
			n, parseCode := parser.Parse(wire[offset:end])
			if parseCode != CodeNone || n != end-offset {
				t.Fatalf("Parse = %d/%d, %v", n, end-offset, parseCode)
			}
			offset = end
		}
		if finish := parser.Finish(); finish != CodeNone {
			t.Fatal(finish)
		}
		if !bytes.Equal(got, payload) || completed != 1 {
			t.Fatalf("payload %x/%x, completed %d", got, payload, completed)
		}
	})
}

func FuzzParserStructuredSequence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5}, uint8(3))
	f.Add([]byte{9, 9, 1, 0, 9, 4}, uint8(1))
	f.Fuzz(func(t *testing.T, controls []byte, stride uint8) {
		if len(controls) > 4096 {
			t.Skip()
		}
		var wire []byte
		openStream := uint32(0)
		for i, control := range controls {
			if openStream != 0 && control%10 < 8 {
				var code Code
				wire, code = AppendFrame(wire, FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: openStream}, nil)
				if code != CodeNone {
					t.Fatalf("generated closing CONTINUATION: %v", code)
				}
				openStream = 0
			}
			streamID := uint32(2*(i%31) + 1)
			var header FrameHeader
			var payload []byte
			switch control % 10 {
			case 0:
				header = FrameHeader{Type: FrameSettings}
			case 1:
				header = FrameHeader{Type: FramePing}
				payload = []byte("12345678")
			case 2:
				header = FrameHeader{Type: FrameWindowUpdate, StreamID: streamID}
				payload = uint32Payload(uint32(control) + 1)
			case 3:
				header = FrameHeader{Type: FrameData, StreamID: streamID}
				payload = []byte{control}
			case 4:
				header = FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders, StreamID: streamID}
				payload = []byte{0x82}
			case 5:
				header = FrameHeader{Type: FrameRSTStream, StreamID: streamID}
				payload = uint32Payload(uint32(ErrCodeCancel))
			case 6:
				header = FrameHeader{Type: 0xee, StreamID: streamID}
				payload = []byte{control, byte(i)}
			case 7:
				header = FrameHeader{Type: FrameHeaders, StreamID: streamID}
				payload = []byte{0x82}
				openStream = streamID
			case 8:
				if openStream == 0 {
					continue
				}
				header = FrameHeader{Type: FrameContinuation, StreamID: openStream}
				payload = []byte{0x84}
			case 9:
				if openStream == 0 {
					continue
				}
				header = FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: openStream}
				openStream = 0
			}
			var code Code
			wire, code = AppendFrame(wire, header, payload)
			if code != CodeNone {
				t.Fatalf("generated invalid frame: %v", code)
			}
		}
		parser := NewParser(nil, Limits{MaxFrameSize: 1 << 20, MaxHeaderBlockBytes: 1 << 20, MaxContinuationFrames: 4096})
		step := 1 + int(stride%23)
		for offset := 0; offset < len(wire); {
			end := offset + step
			if end > len(wire) {
				end = len(wire)
			}
			n, code := parser.Parse(wire[offset:end])
			if code != CodeNone || n != end-offset {
				t.Fatalf("generated sequence rejected at %d: %d, %v", offset, n, code)
			}
			offset = end
		}
		if openStream == 0 && parser.Finish() != CodeNone {
			t.Fatal("complete generated sequence reported truncation")
		}
	})
}

func FuzzHeaderDecoderFragmentation(f *testing.F) {
	f.Add([]byte{0x82, 0x86, 0x84}, uint8(1), uint16(4096), uint16(64))
	f.Add([]byte{0xff}, uint8(7), uint16(1), uint16(1))
	f.Add([]byte{0x40, 0x00}, uint8(2), uint16(4096), uint16(128))
	f.Fuzz(func(t *testing.T, block []byte, stride uint8, tableLimit uint16, listLimit uint16) {
		if len(block) > 1<<20 {
			t.Skip()
		}
		decoder := NewHeaderDecoder(HeaderLimits{
			MaxDynamicTableBytes: uint32(tableLimit),
			MaxFieldBytes:        uint32(listLimit) + 1,
			MaxHeaderListBytes:   uint64(listLimit) + 1,
			MaxHeaders:           1 + uint32(stride%64),
		}, nil)
		if err := decoder.BeginBlock(); err != nil {
			t.Fatal(err)
		}
		step := 1 + int(stride%29)
		for offset := 0; offset < len(block); {
			end := offset + step
			if end > len(block) {
				end = len(block)
			}
			n, err := decoder.Write(block[offset:end])
			if n < 0 || n > end-offset {
				t.Fatalf("wrote %d/%d", n, end-offset)
			}
			offset += n
			if err != nil {
				break
			}
			if n == 0 && end > offset {
				t.Fatal("decoder made no progress")
			}
		}
		_ = decoder.EndBlock()
	})
}

func mustSeedFrame(header FrameHeader, payload []byte) []byte {
	wire, code := AppendFrame(nil, header, payload)
	if code != CodeNone {
		panic(code.String())
	}
	return wire
}
