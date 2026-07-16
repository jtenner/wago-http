package http2

import (
	"encoding/binary"
)

// ClientPreface is the connection preface sent by every HTTP/2 client before
// its first frame.
const ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// FrameType is the HTTP/2 frame type octet.
type FrameType uint8

const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FramePriority     FrameType = 0x2
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FramePushPromise  FrameType = 0x5
	FramePing         FrameType = 0x6
	FrameGoAway       FrameType = 0x7
	FrameWindowUpdate FrameType = 0x8
	FrameContinuation FrameType = 0x9
)

func (typ FrameType) String() string {
	switch typ {
	case FrameData:
		return "DATA"
	case FrameHeaders:
		return "HEADERS"
	case FramePriority:
		return "PRIORITY"
	case FrameRSTStream:
		return "RST_STREAM"
	case FrameSettings:
		return "SETTINGS"
	case FramePushPromise:
		return "PUSH_PROMISE"
	case FramePing:
		return "PING"
	case FrameGoAway:
		return "GOAWAY"
	case FrameWindowUpdate:
		return "WINDOW_UPDATE"
	case FrameContinuation:
		return "CONTINUATION"
	default:
		return "UNKNOWN"
	}
}

// Flags is the frame flags octet. Flag values are interpreted in the context
// of the frame type.
type Flags uint8

const (
	FlagEndStream  Flags = 0x1
	FlagACK        Flags = 0x1
	FlagEndHeaders Flags = 0x4
	FlagPadded     Flags = 0x8
	FlagPriority   Flags = 0x20
)

func (flags Flags) Has(flag Flags) bool { return flags&flag != 0 }

// FrameHeader is the decoded nine-byte HTTP/2 frame header. The reserved bit
// of StreamID is ignored as required by RFC 9113.
type FrameHeader struct {
	Length   uint32
	Type     FrameType
	Flags    Flags
	StreamID uint32
}

// SettingID identifies one SETTINGS parameter.
type SettingID uint16

const (
	SettingHeaderTableSize       SettingID = 0x1
	SettingEnablePush            SettingID = 0x2
	SettingMaxConcurrentStreams  SettingID = 0x3
	SettingInitialWindowSize     SettingID = 0x4
	SettingMaxFrameSize          SettingID = 0x5
	SettingMaxHeaderListSize     SettingID = 0x6
	SettingEnableConnectProtocol SettingID = 0x8
	SettingNoRFC7540Priorities   SettingID = 0x9
)

// Setting is one decoded SETTINGS parameter.
type Setting struct {
	ID    SettingID
	Value uint32
}

// PriorityParam is the five-byte priority section used by PRIORITY and
// priority-bearing HEADERS frames. Exclusive is the dependency reserved bit.
type PriorityParam struct {
	StreamDependency uint32
	Exclusive        bool
	Weight           uint8
}

// ErrorCode is an HTTP/2 wire error code.
type ErrorCode uint32

const (
	ErrCodeNo                 ErrorCode = 0x0
	ErrCodeProtocol           ErrorCode = 0x1
	ErrCodeInternal           ErrorCode = 0x2
	ErrCodeFlowControl        ErrorCode = 0x3
	ErrCodeSettingsTimeout    ErrorCode = 0x4
	ErrCodeStreamClosed       ErrorCode = 0x5
	ErrCodeFrameSize          ErrorCode = 0x6
	ErrCodeRefusedStream      ErrorCode = 0x7
	ErrCodeCancel             ErrorCode = 0x8
	ErrCodeCompression        ErrorCode = 0x9
	ErrCodeConnect            ErrorCode = 0xa
	ErrCodeEnhanceYourCalm    ErrorCode = 0xb
	ErrCodeInadequateSecurity ErrorCode = 0xc
	ErrCodeHTTP11Required     ErrorCode = 0xd
)

// Code is the result of incremental frame parsing. Failures are sticky until
// Reset or Init is called.
type Code uint8

const (
	CodeNone Code = iota
	CodeReentrantCall
	CodeFrameTooLarge
	CodeInvalidStreamID
	CodeInvalidFrameSize
	CodeInvalidPadding
	CodeInvalidContinuation
	CodeHeaderBlockTooLarge
	CodeTooManyContinuations
	CodeInvalidPriority
	CodeInvalidSetting
	CodeInvalidWindowUpdate
	CodeUnexpectedEOF
)

func (code Code) String() string {
	switch code {
	case CodeNone:
		return "ok"
	case CodeReentrantCall:
		return "reentrant parser call"
	case CodeFrameTooLarge:
		return "frame too large"
	case CodeInvalidStreamID:
		return "invalid stream identifier"
	case CodeInvalidFrameSize:
		return "invalid frame size"
	case CodeInvalidPadding:
		return "invalid padding"
	case CodeInvalidContinuation:
		return "invalid CONTINUATION sequence"
	case CodeHeaderBlockTooLarge:
		return "header block too large"
	case CodeTooManyContinuations:
		return "too many CONTINUATION frames"
	case CodeInvalidPriority:
		return "invalid priority dependency"
	case CodeInvalidSetting:
		return "invalid SETTINGS value"
	case CodeInvalidWindowUpdate:
		return "invalid WINDOW_UPDATE increment"
	case CodeUnexpectedEOF:
		return "unexpected EOF"
	default:
		return "unknown HTTP/2 parser result"
	}
}

// Limits bounds parser-controlled work. Zero values select finite defaults.
type Limits struct {
	MaxFrameSize          uint32
	MaxHeaderBlockBytes   uint64
	MaxContinuationFrames uint32
}

const (
	defaultMaxFrameSize          = 16 << 10
	defaultMaxHeaderBlockBytes   = 1 << 20
	defaultMaxContinuationFrames = 1024
	maxFrameSize                 = 1<<24 - 1
)

func (limits Limits) normalized() Limits {
	if limits.MaxFrameSize == 0 {
		limits.MaxFrameSize = defaultMaxFrameSize
	}
	if limits.MaxHeaderBlockBytes == 0 {
		limits.MaxHeaderBlockBytes = defaultMaxHeaderBlockBytes
	}
	if limits.MaxContinuationFrames == 0 {
		limits.MaxContinuationFrames = defaultMaxContinuationFrames
	}
	if limits.MaxFrameSize > maxFrameSize {
		limits.MaxFrameSize = maxFrameSize
	}
	return limits
}

// Callbacks receives validated, borrowed frame payload spans. Byte slices alias
// the input passed to Parse, are cap-limited, and must not be retained. A
// callback panic is propagated; Reset is required before parser reuse.
type Callbacks struct {
	FrameBegin     func(FrameHeader)
	Data           func(streamID uint32, data []byte, endStream bool)
	HeaderBlock    func(streamID uint32, fragment []byte)
	HeaderBlockEnd func(streamID uint32, endStream bool)
	Priority       func(streamID uint32, priority PriorityParam)
	RSTStream      func(streamID uint32, code ErrorCode)
	Setting        func(Setting)
	SettingsEnd    func(ack bool)
	PushPromise    func(streamID, promisedStreamID uint32)
	Ping           func(data [8]byte, ack bool)
	GoAway         func(lastStreamID uint32, code ErrorCode)
	GoAwayDebug    func([]byte)
	WindowUpdate   func(streamID, increment uint32)
	Unknown        func(FrameHeader, []byte)
	FrameComplete  func(FrameHeader)
}

type payloadState uint8

const (
	payloadStart payloadState = iota
	payloadPrefix
	payloadContent
	payloadPadding
)

// Parser incrementally validates and dispatches HTTP/2 frames. It retains only
// the current frame header and fixed-size frame prefixes, never payload data.
// Parser is not safe for concurrent use.
type Parser struct {
	callbacks       *Callbacks
	limits          Limits
	code            Code
	parsing         bool
	pauseAfterFrame bool
	paused          bool

	header   [9]byte
	headerN  uint8
	current  FrameHeader
	payloadN uint32
	state    payloadState

	prefix     [8]byte
	prefixN    uint8
	prefixNeed uint8
	padLength  uint32
	contentN   uint32

	continuationStream   uint32
	continuationCount    uint32
	headerBlockBytes     uint64
	headerBlockEndStream bool
}

// NewParser constructs a bounded frame parser.
func NewParser(callbacks *Callbacks, limits Limits) Parser {
	var parser Parser
	parser.Init(callbacks, limits)
	return parser
}

// Init discards all state and installs callbacks and limits.
func (parser *Parser) Init(callbacks *Callbacks, limits Limits) {
	if parser.parsing {
		parser.fail(CodeReentrantCall)
		return
	}
	*parser = Parser{callbacks: callbacks, limits: limits.normalized()}
}

// Reset discards parsing state while preserving callbacks and limits.
func (parser *Parser) Reset() {
	if parser.parsing {
		parser.fail(CodeReentrantCall)
		return
	}
	callbacks, limits := parser.callbacks, parser.limits
	parser.Init(callbacks, limits)
}

// Parse consumes as many complete or partial frames as src contains. All input
// is consumed on CodeNone. On failure, n identifies the first rejected byte or
// the end of a just-completed invalid fixed field.
func (parser *Parser) Parse(src []byte) (n int, code Code) {
	return parser.runParse(src, false)
}

// ParseOne consumes at most one frame. complete reports that a frame boundary
// was reached; n then excludes bytes belonging to the next frame.
func (parser *Parser) ParseOne(src []byte) (n int, complete bool, code Code) {
	n, code = parser.runParse(src, true)
	return n, parser.paused, code
}

func (parser *Parser) runParse(src []byte, pauseAfterFrame bool) (n int, code Code) {
	if parser.parsing {
		return 0, parser.fail(CodeReentrantCall)
	}
	if parser.code != CodeNone {
		return 0, parser.code
	}
	parser.parsing = true
	parser.pauseAfterFrame = pauseAfterFrame
	parser.paused = false
	defer func() {
		parser.parsing = false
		parser.pauseAfterFrame = false
	}()

	for n < len(src) {
		if parser.paused {
			return n, CodeNone
		}
		if parser.headerN < 9 {
			copied := copy(parser.header[parser.headerN:], src[n:])
			parser.headerN += uint8(copied)
			n += copied
			if parser.headerN < 9 {
				return n, CodeNone
			}
			if code := parser.beginFrame(); code != CodeNone {
				return n, code
			}
			if parser.current.Length == 0 {
				if code := parser.finishFrame(); code != CodeNone {
					return n, code
				}
				continue
			}
		}

		consumed, frameDone, code := parser.parsePayload(src[n:])
		n += consumed
		if code != CodeNone {
			return n, code
		}
		if parser.code != CodeNone {
			return n, parser.code
		}
		if !frameDone {
			return n, CodeNone
		}
		if code := parser.finishFrame(); code != CodeNone {
			return n, code
		}
	}
	return n, CodeNone
}

// Finish reports truncation of a frame or header-block continuation sequence.
func (parser *Parser) Finish() Code {
	if parser.parsing {
		return parser.fail(CodeReentrantCall)
	}
	if parser.code != CodeNone {
		return parser.code
	}
	if parser.headerN != 0 || parser.continuationStream != 0 {
		return parser.fail(CodeUnexpectedEOF)
	}
	return CodeNone
}

func (parser *Parser) beginFrame() Code {
	length := uint32(parser.header[0])<<16 | uint32(parser.header[1])<<8 | uint32(parser.header[2])
	parser.current = FrameHeader{
		Length:   length,
		Type:     FrameType(parser.header[3]),
		Flags:    Flags(parser.header[4]),
		StreamID: binary.BigEndian.Uint32(parser.header[5:9]) & 0x7fffffff,
	}
	parser.payloadN = 0
	parser.state = payloadStart
	parser.prefixN = 0
	parser.prefixNeed = 0
	parser.padLength = 0
	parser.contentN = 0

	if length > parser.limits.MaxFrameSize {
		return parser.fail(CodeFrameTooLarge)
	}
	if parser.continuationStream != 0 {
		if parser.current.Type != FrameContinuation || parser.current.StreamID != parser.continuationStream {
			return parser.fail(CodeInvalidContinuation)
		}
		parser.continuationCount++
		if parser.continuationCount > parser.limits.MaxContinuationFrames {
			return parser.fail(CodeTooManyContinuations)
		}
	} else if parser.current.Type == FrameContinuation {
		return parser.fail(CodeInvalidContinuation)
	}
	if parser.current.Type == FrameHeaders && parser.continuationStream == 0 {
		parser.headerBlockEndStream = parser.current.Flags.Has(FlagEndStream)
	}
	if code := validateFrameHeader(parser.current); code != CodeNone {
		return parser.fail(code)
	}
	if callbacks := parser.callbacks; callbacks != nil && callbacks.FrameBegin != nil {
		callbacks.FrameBegin(parser.current)
	}
	return parser.code
}

func validateFrameHeader(header FrameHeader) Code {
	streamRequired := header.Type == FrameData || header.Type == FrameHeaders || header.Type == FramePriority ||
		header.Type == FrameRSTStream || header.Type == FramePushPromise || header.Type == FrameContinuation
	streamForbidden := header.Type == FrameSettings || header.Type == FramePing || header.Type == FrameGoAway
	if streamRequired && header.StreamID == 0 || streamForbidden && header.StreamID != 0 {
		return CodeInvalidStreamID
	}
	switch header.Type {
	case FrameData:
		if header.Flags.Has(FlagPadded) && header.Length < 1 {
			return CodeInvalidFrameSize
		}
	case FrameHeaders:
		minimum := uint32(0)
		if header.Flags.Has(FlagPadded) {
			minimum++
		}
		if header.Flags.Has(FlagPriority) {
			minimum += 5
		}
		if header.Length < minimum {
			return CodeInvalidFrameSize
		}
	case FramePriority:
		if header.Length != 5 {
			return CodeInvalidFrameSize
		}
	case FrameRSTStream:
		if header.Length != 4 {
			return CodeInvalidFrameSize
		}
	case FrameSettings:
		if header.Flags.Has(FlagACK) {
			if header.Length != 0 {
				return CodeInvalidFrameSize
			}
		} else if header.Length%6 != 0 {
			return CodeInvalidFrameSize
		}
	case FramePushPromise:
		minimum := uint32(4)
		if header.Flags.Has(FlagPadded) {
			minimum++
		}
		if header.Length < minimum {
			return CodeInvalidFrameSize
		}
	case FramePing:
		if header.Length != 8 {
			return CodeInvalidFrameSize
		}
	case FrameGoAway:
		if header.Length < 8 {
			return CodeInvalidFrameSize
		}
	case FrameWindowUpdate:
		if header.Length != 4 {
			return CodeInvalidFrameSize
		}
	}
	return CodeNone
}

func (parser *Parser) parsePayload(src []byte) (n int, done bool, code Code) {
	for n < len(src) && parser.payloadN < parser.current.Length {
		switch parser.current.Type {
		case FrameData:
			n, done, code = parser.parseData(src, n)
		case FrameHeaders:
			n, done, code = parser.parseHeaders(src, n)
		case FramePriority:
			n, done, code = parser.parseFixed(src, n, 5)
		case FrameRSTStream:
			n, done, code = parser.parseFixed(src, n, 4)
		case FrameSettings:
			n, done, code = parser.parseSettings(src, n)
		case FramePushPromise:
			n, done, code = parser.parsePushPromise(src, n)
		case FramePing:
			n, done, code = parser.parseFixed(src, n, 8)
		case FrameGoAway:
			n, done, code = parser.parseGoAway(src, n)
		case FrameWindowUpdate:
			n, done, code = parser.parseFixed(src, n, 4)
		case FrameContinuation:
			n, done, code = parser.parseHeaderFragment(src, n, parser.current.Length-parser.payloadN)
		default:
			remaining := int(parser.current.Length - parser.payloadN)
			available := len(src) - n
			if remaining > available {
				remaining = available
			}
			fragment := src[n : n+remaining : n+remaining]
			if parser.callbacks != nil && parser.callbacks.Unknown != nil {
				parser.callbacks.Unknown(parser.current, fragment)
			}
			parser.payloadN += uint32(remaining)
			n += remaining
			done = parser.payloadN == parser.current.Length
		}
		if code != CodeNone || done {
			return n, done, code
		}
	}
	return n, parser.payloadN == parser.current.Length, CodeNone
}

func (parser *Parser) parseData(src []byte, n int) (int, bool, Code) {
	if parser.state == payloadStart {
		if parser.current.Flags.Has(FlagPadded) {
			parser.padLength = uint32(src[n])
			parser.payloadN++
			n++
			if parser.padLength > parser.current.Length-parser.payloadN {
				return n, false, parser.fail(CodeInvalidPadding)
			}
		}
		parser.contentN = parser.current.Length - parser.payloadN - parser.padLength
		parser.state = payloadContent
	}
	if parser.state == payloadContent && parser.contentN != 0 && n < len(src) {
		count := minInt(int(parser.contentN), len(src)-n)
		fragment := src[n : n+count : n+count]
		if parser.callbacks != nil && parser.callbacks.Data != nil {
			parser.callbacks.Data(parser.current.StreamID, fragment, parser.current.Flags.Has(FlagEndStream))
		}
		parser.contentN -= uint32(count)
		parser.payloadN += uint32(count)
		n += count
	}
	if parser.state == payloadContent && parser.contentN == 0 {
		parser.state = payloadPadding
	}
	if parser.state == payloadPadding && n < len(src) {
		count := minInt(int(parser.current.Length-parser.payloadN), len(src)-n)
		parser.payloadN += uint32(count)
		n += count
	}
	return n, parser.payloadN == parser.current.Length, CodeNone
}

func (parser *Parser) parseHeaders(src []byte, n int) (int, bool, Code) {
	if parser.state == payloadStart {
		if parser.current.Flags.Has(FlagPadded) {
			parser.padLength = uint32(src[n])
			parser.payloadN++
			n++
			minimum := uint32(0)
			if parser.current.Flags.Has(FlagPriority) {
				minimum = 5
			}
			if parser.padLength > parser.current.Length-parser.payloadN-minimum {
				return n, false, parser.fail(CodeInvalidPadding)
			}
		}
		if parser.current.Flags.Has(FlagPriority) {
			parser.prefixNeed = 5
			parser.state = payloadPrefix
		} else {
			parser.contentN = parser.current.Length - parser.payloadN - parser.padLength
			parser.state = payloadContent
		}
	}
	if parser.state == payloadPrefix {
		n = parser.copyPrefix(src, n)
		if parser.prefixN < parser.prefixNeed {
			return n, false, CodeNone
		}
		priority := decodePriority(parser.prefix[:5])
		if priority.StreamDependency == parser.current.StreamID {
			return n, false, parser.fail(CodeInvalidPriority)
		}
		if parser.callbacks != nil && parser.callbacks.Priority != nil {
			parser.callbacks.Priority(parser.current.StreamID, priority)
		}
		parser.contentN = parser.current.Length - parser.payloadN - parser.padLength
		parser.state = payloadContent
	}
	return parser.parseHeaderContentAndPadding(src, n)
}

func (parser *Parser) parsePushPromise(src []byte, n int) (int, bool, Code) {
	if parser.state == payloadStart {
		if parser.current.Flags.Has(FlagPadded) {
			parser.padLength = uint32(src[n])
			parser.payloadN++
			n++
			if parser.padLength > parser.current.Length-parser.payloadN-4 {
				return n, false, parser.fail(CodeInvalidPadding)
			}
		}
		parser.prefixNeed = 4
		parser.state = payloadPrefix
	}
	if parser.state == payloadPrefix {
		n = parser.copyPrefix(src, n)
		if parser.prefixN < parser.prefixNeed {
			return n, false, CodeNone
		}
		promised := binary.BigEndian.Uint32(parser.prefix[:4]) & 0x7fffffff
		if promised == 0 {
			return n, false, parser.fail(CodeInvalidStreamID)
		}
		if parser.callbacks != nil && parser.callbacks.PushPromise != nil {
			parser.callbacks.PushPromise(parser.current.StreamID, promised)
		}
		parser.contentN = parser.current.Length - parser.payloadN - parser.padLength
		parser.state = payloadContent
	}
	return parser.parseHeaderContentAndPadding(src, n)
}

func (parser *Parser) parseHeaderContentAndPadding(src []byte, n int) (int, bool, Code) {
	if parser.state == payloadContent && parser.contentN != 0 && n < len(src) {
		var done bool
		var code Code
		n, done, code = parser.parseHeaderFragment(src, n, parser.contentN)
		if code != CodeNone {
			return n, false, code
		}
		if !done {
			return n, false, CodeNone
		}
		parser.contentN = 0
		parser.state = payloadPadding
	}
	if parser.state == payloadContent && parser.contentN == 0 {
		parser.state = payloadPadding
	}
	if parser.state == payloadPadding && n < len(src) {
		count := minInt(int(parser.current.Length-parser.payloadN), len(src)-n)
		parser.payloadN += uint32(count)
		n += count
	}
	return n, parser.payloadN == parser.current.Length, CodeNone
}

func (parser *Parser) parseHeaderFragment(src []byte, n int, remaining uint32) (int, bool, Code) {
	count := minInt(int(remaining), len(src)-n)
	if count == 0 {
		return n, remaining == 0, CodeNone
	}
	if parser.headerBlockBytes+uint64(count) > parser.limits.MaxHeaderBlockBytes {
		return n, false, parser.fail(CodeHeaderBlockTooLarge)
	}
	fragment := src[n : n+count : n+count]
	if parser.callbacks != nil && parser.callbacks.HeaderBlock != nil {
		parser.callbacks.HeaderBlock(parser.current.StreamID, fragment)
	}
	parser.headerBlockBytes += uint64(count)
	parser.payloadN += uint32(count)
	if parser.state == payloadContent {
		parser.contentN -= uint32(count)
	}
	n += count
	return n, uint32(count) == remaining, CodeNone
}

func (parser *Parser) parseFixed(src []byte, n int, need uint8) (int, bool, Code) {
	parser.prefixNeed = need
	n = parser.copyPrefix(src, n)
	return n, parser.payloadN == parser.current.Length, CodeNone
}

func (parser *Parser) parseSettings(src []byte, n int) (int, bool, Code) {
	parser.prefixNeed = 6
	for n < len(src) && parser.payloadN < parser.current.Length {
		n = parser.copyPrefix(src, n)
		if parser.prefixN < 6 {
			break
		}
		setting := Setting{ID: SettingID(binary.BigEndian.Uint16(parser.prefix[:2])), Value: binary.BigEndian.Uint32(parser.prefix[2:6])}
		if !validSetting(setting) {
			return n, false, parser.fail(CodeInvalidSetting)
		}
		if parser.callbacks != nil && parser.callbacks.Setting != nil {
			parser.callbacks.Setting(setting)
		}
		parser.prefixN = 0
	}
	return n, parser.payloadN == parser.current.Length, CodeNone
}

func validSetting(setting Setting) bool {
	switch setting.ID {
	case SettingEnablePush, SettingEnableConnectProtocol, SettingNoRFC7540Priorities:
		return setting.Value <= 1
	case SettingInitialWindowSize:
		return setting.Value <= 0x7fffffff
	case SettingMaxFrameSize:
		return setting.Value >= defaultMaxFrameSize && setting.Value <= maxFrameSize
	default:
		return true
	}
}

func (parser *Parser) parseGoAway(src []byte, n int) (int, bool, Code) {
	if parser.payloadN < 8 {
		parser.prefixNeed = 8
		n = parser.copyPrefix(src, n)
		if parser.prefixN < 8 {
			return n, false, CodeNone
		}
	}
	if parser.payloadN < parser.current.Length && n < len(src) {
		count := minInt(int(parser.current.Length-parser.payloadN), len(src)-n)
		fragment := src[n : n+count : n+count]
		if parser.callbacks != nil && parser.callbacks.GoAwayDebug != nil {
			parser.callbacks.GoAwayDebug(fragment)
		}
		parser.payloadN += uint32(count)
		n += count
	}
	return n, parser.payloadN == parser.current.Length, CodeNone
}

func (parser *Parser) copyPrefix(src []byte, n int) int {
	need := int(parser.prefixNeed - parser.prefixN)
	count := minInt(need, len(src)-n)
	copy(parser.prefix[parser.prefixN:], src[n:n+count])
	parser.prefixN += uint8(count)
	parser.payloadN += uint32(count)
	return n + count
}

func (parser *Parser) finishFrame() Code {
	header := parser.current
	callbacks := parser.callbacks
	switch header.Type {
	case FramePriority:
		priority := decodePriority(parser.prefix[:5])
		if priority.StreamDependency == header.StreamID {
			return parser.fail(CodeInvalidPriority)
		}
		if callbacks != nil && callbacks.Priority != nil {
			callbacks.Priority(header.StreamID, priority)
		}
	case FrameRSTStream:
		if callbacks != nil && callbacks.RSTStream != nil {
			callbacks.RSTStream(header.StreamID, ErrorCode(binary.BigEndian.Uint32(parser.prefix[:4])))
		}
	case FrameSettings:
		if callbacks != nil && callbacks.SettingsEnd != nil {
			callbacks.SettingsEnd(header.Flags.Has(FlagACK))
		}
	case FramePing:
		if callbacks != nil && callbacks.Ping != nil {
			callbacks.Ping(parser.prefix, header.Flags.Has(FlagACK))
		}
	case FrameGoAway:
		if callbacks != nil && callbacks.GoAway != nil {
			callbacks.GoAway(binary.BigEndian.Uint32(parser.prefix[:4])&0x7fffffff, ErrorCode(binary.BigEndian.Uint32(parser.prefix[4:8])))
		}
	case FrameWindowUpdate:
		increment := binary.BigEndian.Uint32(parser.prefix[:4]) & 0x7fffffff
		if increment == 0 {
			return parser.fail(CodeInvalidWindowUpdate)
		}
		if callbacks != nil && callbacks.WindowUpdate != nil {
			callbacks.WindowUpdate(header.StreamID, increment)
		}
	}

	if header.Type == FrameHeaders || header.Type == FramePushPromise || header.Type == FrameContinuation {
		if header.Flags.Has(FlagEndHeaders) {
			if callbacks != nil && callbacks.HeaderBlockEnd != nil {
				callbacks.HeaderBlockEnd(header.StreamID, parser.headerBlockEndStream)
			}
			parser.continuationStream = 0
			parser.continuationCount = 0
			parser.headerBlockBytes = 0
			parser.headerBlockEndStream = false
		} else if parser.continuationStream == 0 {
			parser.continuationStream = header.StreamID
			parser.continuationCount = 0
		}
	}
	if callbacks != nil && callbacks.FrameComplete != nil {
		callbacks.FrameComplete(header)
	}
	if parser.code != CodeNone {
		return parser.code
	}
	parser.headerN = 0
	parser.current = FrameHeader{}
	parser.payloadN = 0
	parser.prefixN = 0
	parser.prefixNeed = 0
	parser.state = payloadStart
	parser.paused = parser.pauseAfterFrame
	return CodeNone
}

func decodePriority(src []byte) PriorityParam {
	dependency := binary.BigEndian.Uint32(src[:4])
	return PriorityParam{
		StreamDependency: dependency & 0x7fffffff,
		Exclusive:        dependency>>31 != 0,
		Weight:           src[4],
	}
}

func (parser *Parser) fail(code Code) Code {
	if parser.code == CodeNone {
		parser.code = code
	}
	return parser.code
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// AppendFrame appends one complete frame after validating the wire header.
// Unknown extension frame types are permitted.
func AppendFrame(dst []byte, header FrameHeader, payload []byte) ([]byte, Code) {
	if len(payload) > maxFrameSize || header.Length != 0 && header.Length != uint32(len(payload)) || header.StreamID > 0x7fffffff {
		return dst, CodeInvalidFrameSize
	}
	header.Length = uint32(len(payload))
	if code := validateFrameHeader(header); code != CodeNone {
		return dst, code
	}
	start := len(dst)
	dst = append(dst, make([]byte, 9)...)
	dst[start] = byte(header.Length >> 16)
	dst[start+1] = byte(header.Length >> 8)
	dst[start+2] = byte(header.Length)
	dst[start+3] = byte(header.Type)
	dst[start+4] = byte(header.Flags)
	binary.BigEndian.PutUint32(dst[start+5:start+9], header.StreamID)
	return append(dst, payload...), CodeNone
}
