package http2

import (
	"bytes"
	"testing"

	"golang.org/x/net/http2/hpack"
)

func BenchmarkParser(b *testing.B) {
	settingsPayload := make([]byte, 0, 600)
	for i := 0; i < 100; i++ {
		settingsPayload = append(settingsPayload, settingPayload(SettingMaxConcurrentStreams, uint32(i+1))...)
	}
	continuations := joinBenchmarkFrames(
		benchmarkFrame{FrameHeader{Type: FrameHeaders, StreamID: 1}, bytes.Repeat([]byte{0x82}, 4096)},
		benchmarkFrame{FrameHeader{Type: FrameContinuation, StreamID: 1}, bytes.Repeat([]byte{0x84}, 4096)},
		benchmarkFrame{FrameHeader{Type: FrameContinuation, Flags: FlagEndHeaders, StreamID: 1}, bytes.Repeat([]byte{0x86}, 4096)},
	)
	cases := []struct {
		name string
		wire []byte
	}{
		{"data-16k", mustBenchmarkFrame(FrameHeader{Type: FrameData, StreamID: 1}, bytes.Repeat([]byte("x"), 16<<10))},
		{"padded-data-16k", mustBenchmarkFrame(FrameHeader{Type: FrameData, Flags: FlagPadded, StreamID: 1}, append([]byte{255}, bytes.Repeat([]byte("x"), 16<<10-1)...))},
		{"headers-continuations-12k", continuations},
		{"settings-100", mustBenchmarkFrame(FrameHeader{Type: FrameSettings}, settingsPayload)},
		{"unknown-16k", mustBenchmarkFrame(FrameHeader{Type: 0xfe}, bytes.Repeat([]byte("x"), 16<<10))},
		{"ping-pipeline-100", bytes.Repeat(mustBenchmarkFrame(FrameHeader{Type: FramePing}, []byte("12345678")), 100)},
	}
	chunkSizes := []int{1 << 30, 1024, 64, 16, 1}
	for _, test := range cases {
		for _, chunkSize := range chunkSizes {
			name := test.name + "/chunk-" + benchmarkInt(chunkSize)
			b.Run(name, func(b *testing.B) {
				parser := NewParser(nil, Limits{MaxFrameSize: 16 << 10, MaxHeaderBlockBytes: 1 << 20})
				b.ReportAllocs()
				b.SetBytes(int64(len(test.wire)))
				for i := 0; i < b.N; i++ {
					parser.Reset()
					for offset := 0; offset < len(test.wire); {
						end := offset + chunkSize
						if end > len(test.wire) {
							end = len(test.wire)
						}
						if _, code := parser.Parse(test.wire[offset:end]); code != CodeNone {
							b.Fatal(code)
						}
						offset = end
					}
					if code := parser.Finish(); code != CodeNone {
						b.Fatal(code)
					}
				}
			})
		}
	}
}

func BenchmarkParserCallbacks(b *testing.B) {
	wire := mustBenchmarkFrame(FrameHeader{Type: FrameData, StreamID: 1}, bytes.Repeat([]byte("x"), 16<<10))
	for _, chunkSize := range []int{1 << 30, 64, 1} {
		b.Run("chunk-"+benchmarkInt(chunkSize), func(b *testing.B) {
			var sink int
			callbacks := Callbacks{Data: func(_ uint32, fragment []byte, _ bool) { sink += len(fragment) }, FrameComplete: func(FrameHeader) { sink++ }}
			parser := NewParser(&callbacks, Limits{})
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for i := 0; i < b.N; i++ {
				parser.Reset()
				for offset := 0; offset < len(wire); {
					end := offset + chunkSize
					if end > len(wire) {
						end = len(wire)
					}
					if _, code := parser.Parse(wire[offset:end]); code != CodeNone {
						b.Fatal(code)
					}
					offset = end
				}
			}
			_ = sink
		})
	}
}

func BenchmarkParserParseOnePipeline(b *testing.B) {
	frame := mustBenchmarkFrame(FrameHeader{Type: FramePing}, []byte("12345678"))
	wire := bytes.Repeat(frame, 100)
	parser := NewParser(nil, Limits{})
	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	for i := 0; i < b.N; i++ {
		parser.Reset()
		for offset := 0; offset < len(wire); {
			n, complete, code := parser.ParseOne(wire[offset:])
			if code != CodeNone || !complete || n != len(frame) {
				b.Fatalf("ParseOne=%d,%t,%v", n, complete, code)
			}
			offset += n
		}
	}
}

func BenchmarkParserErrors(b *testing.B) {
	cases := []struct {
		name   string
		wire   []byte
		limits Limits
		want   Code
	}{
		{"header-stream-zero", rawFrame(FrameHeader{Type: FrameData}, nil), Limits{}, CodeInvalidStreamID},
		{"payload-padding", rawFrame(FrameHeader{Type: FrameData, Flags: FlagPadded, StreamID: 1}, []byte{255}), Limits{}, CodeInvalidPadding},
		{"late-continuation", joinBenchmarkFrames(benchmarkFrame{FrameHeader{Type: FrameHeaders, StreamID: 1}, []byte{0x82}}, benchmarkFrame{FrameHeader{Type: FramePing}, []byte("12345678")}), Limits{}, CodeInvalidContinuation},
		{"header-block-limit", rawFrame(FrameHeader{Type: FrameHeaders, Flags: FlagEndHeaders, StreamID: 1}, bytes.Repeat([]byte{0x82}, 1024)), Limits{MaxHeaderBlockBytes: 1023}, CodeHeaderBlockTooLarge},
		{"invalid-setting-late", rawFrame(FrameHeader{Type: FrameSettings}, append(settingPayload(SettingMaxConcurrentStreams, 1), settingPayload(SettingEnablePush, 2)...)), Limits{}, CodeInvalidSetting},
	}
	for _, test := range cases {
		b.Run(test.name, func(b *testing.B) {
			parser := NewParser(nil, test.limits)
			b.ReportAllocs()
			b.SetBytes(int64(len(test.wire)))
			for i := 0; i < b.N; i++ {
				parser.Reset()
				if _, code := parser.Parse(test.wire); code != test.want {
					b.Fatalf("code=%v want=%v", code, test.want)
				}
			}
		})
	}
}

func BenchmarkAppendFrame(b *testing.B) {
	for _, size := range []int{0, 64, 1024, 16 << 10} {
		payload := bytes.Repeat([]byte("x"), size)
		dst := make([]byte, 0, size+9)
		b.Run("payload-"+benchmarkInt(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size + 9))
			for i := 0; i < b.N; i++ {
				out, code := AppendFrame(dst[:0], FrameHeader{Type: 0xfe, StreamID: 1}, payload)
				if code != CodeNone || len(out) != size+9 {
					b.Fatal(code)
				}
			}
		})
	}
}

func BenchmarkHeaderDecoder(b *testing.B) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	for i := 0; i < 100; i++ {
		if err := encoder.WriteField(hpack.HeaderField{Name: "x-benchmark-" + benchmarkInt(i), Value: stringsRepeat("value", 20)}); err != nil {
			b.Fatal(err)
		}
	}
	wire := encoded.Bytes()
	for _, chunkSize := range []int{1 << 30, 64, 8, 1} {
		b.Run("chunk-"+benchmarkInt(chunkSize), func(b *testing.B) {
			var fields int
			decoder := NewHeaderDecoder(HeaderLimits{MaxHeaderListBytes: 1 << 20}, func(HeaderField) { fields++ })
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for i := 0; i < b.N; i++ {
				if err := decoder.BeginBlock(); err != nil {
					b.Fatal(err)
				}
				for offset := 0; offset < len(wire); {
					end := offset + chunkSize
					if end > len(wire) {
						end = len(wire)
					}
					if _, err := decoder.Write(wire[offset:end]); err != nil {
						b.Fatal(err)
					}
					offset = end
				}
				if err := decoder.EndBlock(); err != nil {
					b.Fatal(err)
				}
			}
			_ = fields
		})
	}
}

type benchmarkFrame struct {
	header  FrameHeader
	payload []byte
}

func mustBenchmarkFrame(header FrameHeader, payload []byte) []byte {
	wire, code := AppendFrame(nil, header, payload)
	if code != CodeNone {
		panic(code.String())
	}
	return wire
}
func joinBenchmarkFrames(frames ...benchmarkFrame) []byte {
	var wire []byte
	for _, frame := range frames {
		var code Code
		wire, code = AppendFrame(wire, frame.header, frame.payload)
		if code != CodeNone {
			panic(code.String())
		}
	}
	return wire
}
func benchmarkInt(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	i := len(buffer)
	for value != 0 {
		i--
		buffer[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[i:])
}
func stringsRepeat(value string, count int) string {
	var buffer bytes.Buffer
	buffer.Grow(len(value) * count)
	for i := 0; i < count; i++ {
		buffer.WriteString(value)
	}
	return buffer.String()
}
