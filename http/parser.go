package http

import (
	"math"
	"math/bits"
	"net/netip"
)

// Kind selects which HTTP/1 message grammar a Parser accepts.
type Kind uint8

const (
	Request Kind = iota + 1
	Response
)

// Limits bounds parser-controlled work and accepted message sizes. Zero values
// select conservative defaults; there is intentionally no unbounded mode.
type Limits struct {
	MaxStartLineBytes     uint32
	MaxHeaderBytes        uint32
	MaxChunkLineBytes     uint32
	MaxChunks             uint32 // non-final chunks per message
	MaxChunkMetadataBytes uint64 // cumulative chunk-size and extension lines
	MaxHeaderNameBytes    uint16
	MaxHeaders            uint16
	MaxBodyBytes          uint64
}

const maxIPLiteralBytes = 64

const (
	defaultMaxStartLineBytes     = 8 << 10
	defaultMaxHeaderBytes        = 32 << 10
	defaultMaxChunkLineBytes     = 8 << 10
	defaultMaxChunks             = 1 << 16
	defaultMaxChunkMetadataBytes = 1 << 20
	defaultMaxHeaderNameBytes    = 256
	defaultMaxHeaders            = 100
	defaultMaxBodyBytes          = 64 << 20
)

func (l Limits) normalized() Limits {
	if l.MaxStartLineBytes == 0 {
		l.MaxStartLineBytes = defaultMaxStartLineBytes
	}
	if l.MaxHeaderBytes == 0 {
		l.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if l.MaxChunkLineBytes == 0 {
		l.MaxChunkLineBytes = defaultMaxChunkLineBytes
	}
	if l.MaxChunks == 0 {
		l.MaxChunks = defaultMaxChunks
	}
	if l.MaxChunkMetadataBytes == 0 {
		l.MaxChunkMetadataBytes = defaultMaxChunkMetadataBytes
	}
	if l.MaxHeaderNameBytes == 0 {
		l.MaxHeaderNameBytes = defaultMaxHeaderNameBytes
	}
	if l.MaxHeaders == 0 {
		l.MaxHeaders = defaultMaxHeaders
	}
	if l.MaxBodyBytes == 0 {
		l.MaxBodyBytes = defaultMaxBodyBytes
	}
	return l
}

// Code is the parser result. CodeNone means all supplied bytes were consumed.
// CodeUpgrade is a successful terminal result: bytes after the HTTP message
// belong to the upgraded protocol and remain unconsumed. Other values are
// sticky parse failures until Init or Reset is called.
type Code uint8

const (
	CodeNone Code = iota
	CodeUpgrade
	CodeReentrantCall
	CodeInvalidKind
	CodeInvalidStartLine
	CodeInvalidMethod
	CodeInvalidTarget
	CodeInvalidVersion
	CodeInvalidStatus
	CodeInvalidHeaderName
	CodeInvalidHeaderValue
	CodeInvalidLineEnding
	CodeStartLineTooLarge
	CodeHeadersTooLarge
	CodeHeaderNameTooLarge
	CodeChunkLineTooLarge
	CodeChunkMetadataTooLarge
	CodeTooManyChunks
	CodeTooManyHeaders
	CodeInvalidContentLength
	CodeContentLengthConflict
	CodeInvalidTransferEncoding
	CodeMissingHost
	CodeDuplicateHost
	CodeBodyTooLarge
	CodeInvalidChunkSize
	CodeInvalidChunkExtension
	CodeUnexpectedEOF
)

func (c Code) String() string {
	switch c {
	case CodeNone:
		return "ok"
	case CodeUpgrade:
		return "protocol upgrade"
	case CodeReentrantCall:
		return "reentrant parser call"
	case CodeInvalidKind:
		return "invalid parser kind"
	case CodeInvalidStartLine:
		return "invalid start line"
	case CodeInvalidMethod:
		return "invalid method"
	case CodeInvalidTarget:
		return "invalid request target"
	case CodeInvalidVersion:
		return "invalid HTTP version"
	case CodeInvalidStatus:
		return "invalid response status"
	case CodeInvalidHeaderName:
		return "invalid header name"
	case CodeInvalidHeaderValue:
		return "invalid header value"
	case CodeInvalidLineEnding:
		return "invalid line ending"
	case CodeStartLineTooLarge:
		return "start line too large"
	case CodeHeadersTooLarge:
		return "headers too large"
	case CodeHeaderNameTooLarge:
		return "header name too large"
	case CodeChunkLineTooLarge:
		return "chunk line too large"
	case CodeChunkMetadataTooLarge:
		return "chunk metadata too large"
	case CodeTooManyChunks:
		return "too many chunks"
	case CodeTooManyHeaders:
		return "too many headers"
	case CodeInvalidContentLength:
		return "invalid Content-Length"
	case CodeContentLengthConflict:
		return "conflicting message framing"
	case CodeInvalidTransferEncoding:
		return "invalid Transfer-Encoding"
	case CodeMissingHost:
		return "missing Host header"
	case CodeDuplicateHost:
		return "duplicate Host header"
	case CodeBodyTooLarge:
		return "body too large"
	case CodeInvalidChunkSize:
		return "invalid chunk size"
	case CodeInvalidChunkExtension:
		return "invalid chunk extension"
	case CodeUnexpectedEOF:
		return "unexpected EOF"
	default:
		return "unknown HTTP parser result"
	}
}

// Method identifies common request methods without retaining or allocating the
// method text. Unknown extension methods are reported as MethodOther.
type Method uint8

const (
	MethodOther Method = iota
	MethodGET
	MethodHEAD
	MethodPOST
	MethodPUT
	MethodDELETE
	MethodCONNECT
	MethodOPTIONS
	MethodTRACE
	MethodPATCH
)

var methodNames = [...]string{
	MethodGET:     "GET",
	MethodHEAD:    "HEAD",
	MethodPOST:    "POST",
	MethodPUT:     "PUT",
	MethodDELETE:  "DELETE",
	MethodCONNECT: "CONNECT",
	MethodOPTIONS: "OPTIONS",
	MethodTRACE:   "TRACE",
	MethodPATCH:   "PATCH",
}

func (m Method) String() string {
	if int(m) < len(methodNames) && methodNames[m] != "" {
		return methodNames[m]
	}
	return "OTHER"
}

// Message is an allocation-free snapshot supplied at message boundaries.
type Message struct {
	Kind             Kind
	Method           Method
	Status           uint16
	Major            uint16
	Minor            uint16
	ContentLength    uint64
	HasContentLength bool
	KeepAlive        bool
	MessageNumber    uint64
	ExchangeNumber   uint64
	UpgradeRequested bool
	ConnectRequested bool
}

// Callbacks receives validated, borrowed spans. Spans alias the input passed to
// Parse and are valid only until the caller reuses that input. A token can be
// delivered in multiple spans when it crosses Parse calls. Spans are read-only,
// cap-limited to their length, and must not be retained. Header values omit
// leading optional whitespace but preserve trailing optional whitespace.
// Nil callbacks are skipped. Callbacks intentionally do not receive the Parser,
// reducing accidental reentrant mutation and allowing the parser to remain on
// stack. A callback must not call Parse, Finish, Reset, or Init on its parser.
// Because validation is streaming, callbacks can precede a later message error;
// consumers must commit side effects only from MessageComplete. A callback
// panic is propagated; callers that recover must Reset the parser before reuse.
type Callbacks struct {
	// ResponseContext supplies request method context by exchange number. Any
	// informational responses and the final response for one request receive the
	// same number; it advances only after a final response.
	ResponseContext func(exchangeNumber uint64) (head, connect bool)
	MessageBegin    func()
	Method          func([]byte)
	Target          func([]byte)
	Reason          func([]byte)
	StartLine       func(Message)
	HeaderName      func([]byte)
	HeaderValue     func([]byte)
	HeaderEnd       func(trailer bool)
	HeadersComplete func(Message)
	ChunkHeader     func(uint64)
	Body            func([]byte)
	ChunkComplete   func()
	MessageComplete func(Message)
}

type targetForm uint8

const (
	targetUnknown targetForm = iota
	targetOrigin
	targetAsterisk
	targetScheme
	targetAbsolute
	targetAuthority
)

type parserState uint8

const (
	stateStart parserState = iota
	stateLeadingLF
	stateReqMethod
	stateReqTarget
	stateReqHTTP
	stateVersionMajor
	stateVersionDot
	stateVersionMinor
	stateReqLineCR
	stateReqLineLF
	stateResHTTP
	stateResSpaceBeforeStatus
	stateResStatus
	stateResReasonStart
	stateResReason
	stateResLineLF
	stateHeaderStart
	stateHeaderName
	stateHeaderValueStart
	stateHeaderValue
	stateHeaderValueLF
	stateHeadersLF
	stateFixedBody
	stateCloseBody
	stateChunkSizeStart
	stateChunkSize
	stateChunkExt
	stateChunkSizeLF
	stateChunkBody
	stateChunkBodyCR
	stateChunkBodyLF
	stateUpgrade
	stateDead
)

type fieldKind uint8

const (
	fieldOther fieldKind = iota
	fieldContentLength
	fieldTransferEncoding
	fieldConnection
	fieldUpgrade
	fieldHost
	fieldTrailer
)

var fieldNames = [...]string{
	fieldContentLength:    "content-length",
	fieldTransferEncoding: "transfer-encoding",
	fieldConnection:       "connection",
	fieldUpgrade:          "upgrade",
	fieldHost:             "host",
	fieldTrailer:          "trailer",
}

type transferState uint8

const (
	transferBeforeToken transferState = iota
	transferToken
	transferAfterToken
	transferParamBeforeName
	transferParamName
	transferParamAfterName
	transferParamBeforeValue
	transferParamTokenValue
	transferParamQuoted
	transferParamQuotedEscape
	transferParamAfterValue
)

type connectionState uint8

const (
	connectionBeforeToken connectionState = iota
	connectionToken
	connectionAfterToken
)

type hostState uint8

const (
	hostStart hostState = iota
	hostRegName
	hostPercentFirst
	hostPercentSecond
	hostIPLiteral
	hostAfterLiteral
	hostPortStart
	hostPort
	hostTrailing
)

type upgradeState uint8

const (
	upgradeBeforeProtocol upgradeState = iota
	upgradeName
	upgradeAfterName
	upgradeVersionStart
	upgradeVersion
	upgradeAfterProtocol
)

type chunkExtState uint8

const (
	chunkExtBeforeName chunkExtState = iota
	chunkExtName
	chunkExtAfterName
	chunkExtBeforeValue
	chunkExtTokenValue
	chunkExtQuotedValue
	chunkExtQuotedEscape
	chunkExtAfterValue
)

// Parser is an incremental, strict HTTP/1.0 and HTTP/1.1 message parser with a
// zero-allocation ordinary hot path. It does not retain input buffers and is
// safe to reuse for a sequence of pipelined messages. A Parser must not be used
// concurrently.
type Parser struct {
	callbacks *Callbacks
	limits    Limits
	kind      Kind
	state     parserState
	code      Code

	major       uint16
	minor       uint16
	status      uint16
	method      Method
	messageNum  uint64
	exchangeNum uint64

	startBytes         uint32
	prefixBytes        uint32
	headerBytes        uint32
	chunkLineBytes     uint32
	chunkCount         uint32
	chunkMetadataBytes uint64
	headerCount        uint16
	headerNameBytes    uint16
	bodyBytes          uint64
	remaining          uint64
	chunkSize          uint64

	literalPos             uint8
	statusDigits           uint8
	methodPos              uint8
	methodMask             uint16
	targetBytes            uint32
	targetPercent          uint8
	targetForm             targetForm
	targetAuthorityBracket bool
	targetAuthorityClosed  bool
	targetAuthorityColon   bool
	targetAuthorityPort    bool
	ipLiteral              [maxIPLiteralBytes]byte
	ipLiteralLen           uint16
	fieldPos               uint16
	fieldMask              uint16
	field                  fieldKind

	messageActive       bool
	trailers            bool
	hasContentLength    bool
	hasTransferEncoding bool
	hasHost             bool
	hostNonEmpty        bool
	hostLiteralNonEmpty bool
	hostState           hostState
	hasUpgrade          bool
	upgradeState        upgradeState
	upgradeSawProtocol  bool
	connectionClose     bool
	connectionUpgrade   bool
	connectionKeepAlive bool
	responseToHEAD      bool
	responseToCONNECT   bool
	parsing             bool
	pauseAfterMessage   bool
	paused              bool

	contentLengthDigits   bool
	contentLengthTrailing bool

	transferState          transferState
	transferTokenPos       uint8
	transferChunked        bool
	transferCurrentChunked bool
	transferSawToken       bool
	transferSawChunked     bool

	connectionState          connectionState
	connectionTokenPos       uint8
	connectionCloseMatch     bool
	connectionUpgradeMatch   bool
	connectionKeepAliveMatch bool

	chunkDigits   bool
	chunkExtState chunkExtState
}

// NewParser constructs a parser value without allocating.
func NewParser(kind Kind, callbacks *Callbacks, limits Limits) Parser {
	var p Parser
	p.Init(kind, callbacks, limits)
	return p
}

// Init resets all parser state and replaces its kind, callbacks, and limits.
func (p *Parser) Init(kind Kind, callbacks *Callbacks, limits Limits) {
	if p.parsing {
		p.fail(CodeReentrantCall)
		return
	}
	*p = Parser{kind: kind, callbacks: callbacks, limits: limits.normalized(), state: stateStart}
	if kind != Request && kind != Response {
		p.fail(CodeInvalidKind)
	}
}

// Reset discards current parsing state while preserving configuration.
func (p *Parser) Reset() {
	if p.parsing {
		p.fail(CodeReentrantCall)
		return
	}
	kind, callbacks, limits := p.kind, p.callbacks, p.limits
	p.Init(kind, callbacks, limits)
}

// SetResponseContext supplies static request semantics needed to frame a
// response. Use Callbacks.ResponseContext when one Parse call can contain
// pipelined responses with different request methods.
func (p *Parser) SetResponseContext(head, connect bool) {
	if p.parsing {
		p.fail(CodeReentrantCall)
		return
	}
	p.responseToHEAD = head
	p.responseToCONNECT = connect
}

func (p *Parser) Kind() Kind                    { return p.kind }
func (p *Parser) Code() Code                    { return p.code }
func (p *Parser) Method() Method                { return p.method }
func (p *Parser) Status() uint16                { return p.status }
func (p *Parser) Version() (uint16, uint16)     { return p.major, p.minor }
func (p *Parser) ContentLength() (uint64, bool) { return p.remaining + p.bodyBytes, p.hasContentLength }
func (p *Parser) BodyBytes() uint64             { return p.bodyBytes }
func (p *Parser) MessageNumber() uint64         { return p.messageNum }
func (p *Parser) ExchangeNumber() uint64        { return p.exchangeNum }
func (p *Parser) Trailers() bool                { return p.trailers }
func (p *Parser) Upgraded() bool                { return p.state == stateUpgrade }

// KeepAlive reports the persistence semantics of the current or last message.
func (p *Parser) KeepAlive() bool {
	if p.connectionClose || p.connectionUpgrade && p.hasUpgrade || p.kind == Request && p.method == MethodCONNECT {
		return false
	}
	if p.kind == Response {
		noBody := p.responseToHEAD || p.status >= 100 && p.status < 200 || p.status == 204 || p.status == 304 ||
			p.responseToCONNECT && p.status >= 200 && p.status < 300
		if !noBody && ((!p.hasContentLength && !p.hasTransferEncoding) || p.hasTransferEncoding && !p.transferChunked) {
			return false
		}
	}
	if p.major > 1 || (p.major == 1 && p.minor >= 1) {
		return true
	}
	return p.connectionKeepAlive
}

func (p *Parser) message() Message {
	contentLength := uint64(0)
	if p.hasContentLength {
		contentLength = p.remaining + p.bodyBytes
	}
	return Message{
		Kind: p.kind, Method: p.method, Status: p.status,
		Major: p.major, Minor: p.minor,
		ContentLength: contentLength, HasContentLength: p.hasContentLength,
		KeepAlive: p.KeepAlive(), MessageNumber: p.messageNum, ExchangeNumber: p.exchangeNum,
		UpgradeRequested: p.kind == Request && p.connectionUpgrade && p.hasUpgrade,
		ConnectRequested: p.kind == Request && p.method == MethodCONNECT,
	}
}

// Parse consumes as much of src as possible. CodeNone means all bytes were
// consumed and more input may be supplied. On CodeUpgrade, n excludes bytes of
// the upgraded protocol. Parse failures are sticky.
func (p *Parser) Parse(src []byte) (n int, code Code) {
	return p.runParse(src, false)
}

// ParseOne consumes at most one complete message from src. complete reports
// that a message boundary was reached; n then excludes bytes belonging to the
// next pipelined message. Informational and final responses each count as one
// message. CodeUpgrade remains the successful terminal result for a validated
// protocol switch. Parse failures are sticky.
func (p *Parser) ParseOne(src []byte) (n int, complete bool, code Code) {
	n, code = p.runParse(src, true)
	return n, p.paused, code
}

func (p *Parser) runParse(src []byte, pauseAfterMessage bool) (n int, code Code) {
	if p.parsing {
		return 0, p.fail(CodeReentrantCall)
	}
	p.parsing = true
	p.pauseAfterMessage = pauseAfterMessage
	p.paused = false
	if p.callbacks != nil {
		defer func() {
			p.pauseAfterMessage = false
			p.parsing = false
		}()
		return p.parse(src)
	}
	n, code = p.parse(src)
	p.pauseAfterMessage = false
	p.parsing = false
	return n, code
}

func (p *Parser) parse(src []byte) (n int, code Code) {
	if p.code != CodeNone {
		return 0, p.code
	}
	if p.state == stateUpgrade {
		return 0, CodeUpgrade
	}

	for n < len(src) {
		if p.paused {
			return n, CodeNone
		}
		switch p.state {
		case stateStart:
			if p.kind == Request && src[n] == '\r' {
				if !p.takePrefixByte() {
					return n, p.code
				}
				p.state = stateLeadingLF
				n++
				continue
			}
			p.beginMessage()
			if p.code != CodeNone {
				return n, p.code
			}
			if p.kind == Request {
				p.state = stateReqMethod
			} else {
				p.state = stateResHTTP
			}

		case stateLeadingLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !p.takePrefixByte() {
				return n, p.code
			}
			n++
			p.state = stateStart

		case stateReqMethod:
			start := n
			for n < len(src) && isTChar(src[n]) {
				if !p.takeStartByte() {
					return n, p.code
				}
				p.matchMethod(src[n])
				n++
			}
			if n > start && p.callbacks != nil && p.callbacks.Method != nil {
				p.callbacks.Method(src[start:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if n == len(src) {
				continue
			}
			if p.methodPos == 0 || src[n] != ' ' {
				return n, p.fail(CodeInvalidMethod)
			}
			p.finishMethod()
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.state = stateReqTarget

		case stateReqTarget:
			start := n
			for n < len(src) && validTargetByte(src[n]) {
				b := src[n]
				if !p.consumeTargetForm(b) {
					return n, p.code
				}
				if p.targetPercent != 0 {
					if !isHexByte(b) {
						return n, p.fail(CodeInvalidTarget)
					}
					p.targetPercent++
					if p.targetPercent == 3 {
						p.targetPercent = 0
					}
				} else if b == '%' {
					p.targetPercent = 1
				}
				if !p.takeStartByte() {
					return n, p.code
				}
				p.targetBytes++
				n++
			}
			if n > start && p.callbacks != nil && p.callbacks.Target != nil {
				p.callbacks.Target(src[start:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if n == len(src) {
				continue
			}
			if !p.finishTarget() || src[n] != ' ' {
				return n, p.fail(CodeInvalidTarget)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.literalPos = 0
			p.state = stateReqHTTP

		case stateReqHTTP, stateResHTTP:
			for n < len(src) && p.literalPos < 5 {
				if src[n] != "HTTP/"[p.literalPos] {
					return n, p.fail(CodeInvalidStartLine)
				}
				if !p.takeStartByte() {
					return n, p.code
				}
				p.literalPos++
				n++
			}
			if p.literalPos == 5 {
				p.major = 0
				p.minor = 0
				p.state = stateVersionMajor
			}

		case stateVersionMajor:
			if src[n] < '0' || src[n] > '9' {
				return n, p.fail(CodeInvalidVersion)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			p.major = uint16(src[n] - '0')
			n++
			p.state = stateVersionDot

		case stateVersionDot:
			if src[n] != '.' {
				return n, p.fail(CodeInvalidVersion)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.state = stateVersionMinor

		case stateVersionMinor:
			if src[n] < '0' || src[n] > '9' {
				return n, p.fail(CodeInvalidVersion)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			p.minor = uint16(src[n] - '0')
			n++
			if p.kind == Request {
				p.state = stateReqLineCR
			} else {
				p.state = stateResSpaceBeforeStatus
			}

		case stateReqLineCR:
			if src[n] != '\r' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !validHTTPVersion(p.major, p.minor) {
				return n, p.fail(CodeInvalidVersion)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.state = stateReqLineLF

		case stateReqLineLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.startLineComplete()
			if p.code != CodeNone {
				return n, p.code
			}

		case stateResSpaceBeforeStatus:
			if src[n] != ' ' || !validHTTPVersion(p.major, p.minor) {
				return n, p.fail(CodeInvalidStartLine)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.status = 0
			p.statusDigits = 0
			p.state = stateResStatus

		case stateResStatus:
			for n < len(src) && p.statusDigits < 3 {
				b := src[n]
				if b < '0' || b > '9' {
					return n, p.fail(CodeInvalidStatus)
				}
				if !p.takeStartByte() {
					return n, p.code
				}
				p.status = p.status*10 + uint16(b-'0')
				p.statusDigits++
				n++
			}
			if p.statusDigits == 3 {
				if p.status < 100 {
					return n, p.fail(CodeInvalidStatus)
				}
				p.state = stateResReasonStart
			}

		case stateResReasonStart:
			if src[n] != ' ' {
				return n, p.fail(CodeInvalidStatus)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.state = stateResReason

		case stateResReason:
			start := n
			for n < len(src) && src[n] != '\r' {
				if !validReasonByte(src[n]) {
					return n, p.fail(CodeInvalidStatus)
				}
				if !p.takeStartByte() {
					return n, p.code
				}
				n++
			}
			if n > start && p.callbacks != nil && p.callbacks.Reason != nil {
				p.callbacks.Reason(src[start:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if n < len(src) {
				if !p.takeStartByte() {
					return n, p.code
				}
				n++
				p.state = stateResLineLF
			}

		case stateResLineLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !p.takeStartByte() {
				return n, p.code
			}
			n++
			p.startLineComplete()
			if p.code != CodeNone {
				return n, p.code
			}

		case stateHeaderStart:
			if src[n] == '\r' {
				if !p.takeHeaderByte() {
					return n, p.code
				}
				n++
				p.state = stateHeadersLF
				continue
			}
			if src[n] == ' ' || src[n] == '\t' {
				return n, p.fail(CodeInvalidHeaderName)
			}
			p.beginHeader()
			if p.code != CodeNone {
				return n, p.code
			}
			p.state = stateHeaderName

		case stateHeaderName:
			start := n
			for n < len(src) && isTChar(src[n]) {
				if !p.takeHeaderByte() {
					return n, p.code
				}
				if p.headerNameBytes == p.limits.MaxHeaderNameBytes {
					return n, p.fail(CodeHeaderNameTooLarge)
				}
				p.headerNameBytes++
				p.matchField(src[n])
				n++
			}
			if n > start && p.callbacks != nil && p.callbacks.HeaderName != nil {
				p.callbacks.HeaderName(src[start:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if n == len(src) {
				continue
			}
			if p.headerNameBytes == 0 || src[n] != ':' {
				return n, p.fail(CodeInvalidHeaderName)
			}
			if !p.takeHeaderByte() {
				return n, p.code
			}
			n++
			p.finishFieldName()
			if p.code != CodeNone {
				return n, p.code
			}
			p.state = stateHeaderValueStart

		case stateHeaderValueStart:
			for n < len(src) && (src[n] == ' ' || src[n] == '\t') {
				if !p.takeHeaderByte() {
					return n, p.code
				}
				n++
			}
			if n < len(src) {
				p.state = stateHeaderValue
			}

		case stateHeaderValue:
			start := n
			parseValue := p.field != fieldOther && p.field != fieldTrailer
			for n < len(src) && src[n] != '\r' {
				if !validFieldValueByte(src[n]) {
					return n, p.fail(CodeInvalidHeaderValue)
				}
				if !p.takeHeaderByte() {
					return n, p.code
				}
				if parseValue && !p.consumeFieldValue(src[n]) {
					return n, p.code
				}
				n++
			}
			if n > start && p.callbacks != nil && p.callbacks.HeaderValue != nil {
				p.callbacks.HeaderValue(src[start:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if n < len(src) {
				if !p.finishFieldValue() {
					return n, p.code
				}
				if !p.takeHeaderByte() {
					return n, p.code
				}
				n++
				p.state = stateHeaderValueLF
			}

		case stateHeaderValueLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !p.takeHeaderByte() {
				return n, p.code
			}
			n++
			if p.callbacks != nil && p.callbacks.HeaderEnd != nil {
				p.callbacks.HeaderEnd(p.trailers)
				if p.code != CodeNone {
					return n, p.code
				}
			}
			p.state = stateHeaderStart

		case stateHeadersLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !p.takeHeaderByte() {
				return n, p.code
			}
			n++
			if p.trailers {
				if p.callbacks != nil && p.callbacks.ChunkComplete != nil {
					p.callbacks.ChunkComplete()
					if p.code != CodeNone {
						return n, p.code
					}
				}
				p.completeMessage()
				if p.code != CodeNone {
					return n, p.code
				}
				continue
			}
			if !p.headersComplete() {
				return n, p.code
			}
			if p.state == stateUpgrade {
				return n, CodeUpgrade
			}

		case stateFixedBody:
			available := uint64(len(src) - n)
			take := available
			if take > p.remaining {
				take = p.remaining
			}
			if !p.addBody(take) {
				return n, p.code
			}
			bodyStart := n
			n += int(take)
			p.remaining -= take
			if take != 0 && p.callbacks != nil && p.callbacks.Body != nil {
				p.callbacks.Body(src[bodyStart:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if p.remaining == 0 {
				p.completeMessage()
				if p.code != CodeNone {
					return n, p.code
				}
			}

		case stateCloseBody:
			available := uint64(len(src) - n)
			take := available
			bodyBudget := p.limits.MaxBodyBytes - p.bodyBytes
			if take > bodyBudget {
				take = bodyBudget
			}
			p.bodyBytes += take
			bodyStart := n
			n += int(take)
			if take != 0 && p.callbacks != nil && p.callbacks.Body != nil {
				p.callbacks.Body(src[bodyStart:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if take != available {
				return n, p.fail(CodeBodyTooLarge)
			}

		case stateChunkSizeStart:
			p.chunkSize = 0
			p.chunkDigits = false
			p.chunkLineBytes = 0
			p.state = stateChunkSize

		case stateChunkSize:
			for n < len(src) {
				b := src[n]
				digit, ok := hexValue(b)
				if !ok {
					break
				}
				p.chunkDigits = true
				if !p.takeChunkLineByte() {
					return n, p.code
				}
				if p.chunkSize > (math.MaxUint64-uint64(digit))/16 {
					return n, p.fail(CodeInvalidChunkSize)
				}
				p.chunkSize = p.chunkSize*16 + uint64(digit)
				n++
			}
			if n == len(src) {
				continue
			}
			if !p.chunkDigits {
				return n, p.fail(CodeInvalidChunkSize)
			}
			if src[n] == ';' {
				if !p.takeChunkLineByte() {
					return n, p.code
				}
				n++
				p.chunkExtState = chunkExtBeforeName
				p.state = stateChunkExt
				continue
			}
			if src[n] != '\r' {
				return n, p.fail(CodeInvalidChunkSize)
			}
			if !p.takeChunkLineByte() {
				return n, p.code
			}
			n++
			p.state = stateChunkSizeLF

		case stateChunkExt:
			for n < len(src) && src[n] != '\r' {
				if !p.takeChunkLineByte() {
					return n, p.code
				}
				if !p.consumeChunkExtension(src[n]) {
					return n, p.code
				}
				n++
			}
			if n < len(src) {
				if !p.finishChunkExtension() {
					return n, p.code
				}
				if !p.takeChunkLineByte() {
					return n, p.code
				}
				n++
				p.state = stateChunkSizeLF
			}

		case stateChunkSizeLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			if !p.takeChunkLineByte() {
				return n, p.code
			}
			n++
			if p.chunkSize > p.limits.MaxBodyBytes-p.bodyBytes {
				return n, p.fail(CodeBodyTooLarge)
			}
			if p.chunkSize != 0 {
				if p.chunkCount >= p.limits.MaxChunks {
					return n, p.fail(CodeTooManyChunks)
				}
				p.chunkCount++
			}
			if p.callbacks != nil && p.callbacks.ChunkHeader != nil {
				p.callbacks.ChunkHeader(p.chunkSize)
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if p.chunkSize == 0 {
				p.trailers = true
				p.state = stateHeaderStart
			} else {
				p.remaining = p.chunkSize
				p.state = stateChunkBody
			}

		case stateChunkBody:
			available := uint64(len(src) - n)
			take := available
			if take > p.remaining {
				take = p.remaining
			}
			if !p.addBody(take) {
				return n, p.code
			}
			bodyStart := n
			n += int(take)
			p.remaining -= take
			if take != 0 && p.callbacks != nil && p.callbacks.Body != nil {
				p.callbacks.Body(src[bodyStart:n:n])
				if p.code != CodeNone {
					return n, p.code
				}
			}
			if p.remaining == 0 {
				p.state = stateChunkBodyCR
			}

		case stateChunkBodyCR:
			if src[n] != '\r' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			n++
			p.state = stateChunkBodyLF

		case stateChunkBodyLF:
			if src[n] != '\n' {
				return n, p.fail(CodeInvalidLineEnding)
			}
			n++
			if p.callbacks != nil && p.callbacks.ChunkComplete != nil {
				p.callbacks.ChunkComplete()
				if p.code != CodeNone {
					return n, p.code
				}
			}
			p.state = stateChunkSizeStart

		default:
			return n, p.fail(CodeInvalidStartLine)
		}
	}
	return n, CodeNone
}

// Finish signals end of input. It completes a close-delimited response body or
// reports truncation for every incomplete framed message.
func (p *Parser) Finish() Code {
	if p.parsing {
		return p.fail(CodeReentrantCall)
	}
	if p.code != CodeNone {
		return p.code
	}
	p.parsing = true
	defer func() { p.parsing = false }()
	if p.state == stateUpgrade {
		return CodeUpgrade
	}
	if p.state == stateCloseBody {
		p.completeMessage()
		return p.code
	}
	if p.state == stateStart && !p.messageActive {
		return CodeNone
	}
	return p.fail(CodeUnexpectedEOF)
}

func (p *Parser) beginMessage() {
	if p.kind == Response && p.callbacks != nil && p.callbacks.ResponseContext != nil {
		p.responseToHEAD, p.responseToCONNECT = p.callbacks.ResponseContext(p.exchangeNum)
		if p.code != CodeNone {
			return
		}
	}
	p.major = 0
	p.minor = 0
	p.status = 0
	p.method = MethodOther
	p.startBytes = 0
	p.prefixBytes = 0
	p.headerBytes = 0
	p.chunkLineBytes = 0
	p.chunkCount = 0
	p.chunkMetadataBytes = 0
	p.headerCount = 0
	p.bodyBytes = 0
	p.remaining = 0
	p.chunkSize = 0
	p.literalPos = 0
	p.statusDigits = 0
	p.methodPos = 0
	p.methodMask = (1 << (len(methodNames) - 1)) - 1
	p.targetBytes = 0
	p.targetPercent = 0
	p.targetForm = targetUnknown
	p.targetAuthorityBracket = false
	p.targetAuthorityClosed = false
	p.targetAuthorityColon = false
	p.targetAuthorityPort = false
	p.ipLiteralLen = 0
	p.hasContentLength = false
	p.hasTransferEncoding = false
	p.hasHost = false
	p.hostNonEmpty = false
	p.hostLiteralNonEmpty = false
	p.hostState = hostStart
	p.hasUpgrade = false
	p.upgradeState = upgradeBeforeProtocol
	p.upgradeSawProtocol = false
	p.connectionClose = false
	p.connectionUpgrade = false
	p.connectionKeepAlive = false
	p.trailers = false
	p.messageActive = true
	if p.callbacks != nil && p.callbacks.MessageBegin != nil {
		p.callbacks.MessageBegin()
		if p.code != CodeNone {
			return
		}
	}
}

func (p *Parser) startLineComplete() {
	if p.callbacks != nil && p.callbacks.StartLine != nil {
		p.callbacks.StartLine(p.message())
		if p.code != CodeNone {
			return
		}
	}
	p.state = stateHeaderStart
}

func (p *Parser) beginHeader() {
	if p.headerCount >= p.limits.MaxHeaders {
		p.fail(CodeTooManyHeaders)
		return
	}
	p.headerCount++
	p.headerNameBytes = 0
	p.fieldPos = 0
	p.fieldMask = (1 << (len(fieldNames) - 1)) - 1
	p.field = fieldOther
}

func (p *Parser) finishFieldName() {
	p.field = fieldOther
	for candidate := fieldKind(1); int(candidate) < len(fieldNames); candidate++ {
		if p.fieldMask&(1<<(candidate-1)) != 0 && int(p.fieldPos) == len(fieldNames[candidate]) {
			p.field = candidate
			break
		}
	}
	if p.kind == Response && !p.trailers && p.field == fieldHost {
		p.field = fieldOther
	}
	if p.trailers && (p.field == fieldContentLength || p.field == fieldTransferEncoding || p.field == fieldConnection || p.field == fieldUpgrade || p.field == fieldHost || p.field == fieldTrailer) {
		p.fail(CodeInvalidHeaderName)
		return
	}
	switch p.field {
	case fieldContentLength:
		if p.hasContentLength {
			p.fail(CodeInvalidContentLength)
			return
		}
		if p.hasTransferEncoding {
			p.fail(CodeContentLengthConflict)
			return
		}
		p.hasContentLength = true
		p.contentLengthDigits = false
		p.contentLengthTrailing = false
		p.remaining = 0
	case fieldTransferEncoding:
		if p.hasTransferEncoding {
			p.fail(CodeInvalidTransferEncoding)
			return
		}
		if p.hasContentLength {
			p.fail(CodeContentLengthConflict)
			return
		}
		p.hasTransferEncoding = true
		p.transferState = transferBeforeToken
		p.transferSawToken = false
		p.transferSawChunked = false
		p.transferCurrentChunked = false
		p.transferChunked = false
	case fieldConnection:
		p.connectionState = connectionBeforeToken
	case fieldHost:
		if p.hasHost {
			p.fail(CodeDuplicateHost)
			return
		}
		p.hasHost = true
		p.hostNonEmpty = false
		p.hostLiteralNonEmpty = false
		p.hostState = hostStart
	case fieldUpgrade:
		p.upgradeState = upgradeBeforeProtocol
		p.upgradeSawProtocol = false
	}
}

func (p *Parser) finishFieldValue() bool {
	switch p.field {
	case fieldContentLength:
		if !p.contentLengthDigits {
			return p.fail(CodeInvalidContentLength) == CodeNone
		}
	case fieldTransferEncoding:
		if !p.finishTransferValue() {
			return false
		}
	case fieldConnection:
		if !p.finishConnectionValue() {
			return false
		}
	case fieldHost:
		if !p.finishHostValue() {
			return false
		}
	case fieldUpgrade:
		if !p.finishUpgradeValue() {
			return false
		}
	}
	return true
}

func (p *Parser) headersComplete() bool {
	if p.kind == Request && p.major == 1 && p.minor == 1 && !p.hasHost {
		p.fail(CodeMissingHost)
		return false
	}
	if p.hasTransferEncoding && (p.major != 1 || p.minor != 1) {
		p.fail(CodeInvalidTransferEncoding)
		return false
	}
	if p.kind == Response {
		if p.status == 101 && (!p.connectionUpgrade || !p.hasUpgrade) {
			p.fail(CodeInvalidHeaderValue)
			return false
		}
		forbidsFraming := p.status >= 100 && p.status < 200 || p.status == 204 ||
			p.responseToCONNECT && p.status >= 200 && p.status < 300
		if forbidsFraming && (p.hasContentLength || p.hasTransferEncoding) {
			p.fail(CodeContentLengthConflict)
			return false
		}
	}
	if p.hasTransferEncoding {
		if p.kind == Request && !p.transferChunked {
			p.fail(CodeInvalidTransferEncoding)
			return false
		}
	}
	if p.callbacks != nil && p.callbacks.HeadersComplete != nil {
		p.callbacks.HeadersComplete(p.message())
		if p.code != CodeNone {
			return false
		}
	}

	upgrade := p.kind == Response && p.status == 101 && p.connectionUpgrade && p.hasUpgrade
	if p.kind == Response && p.responseToCONNECT && p.status >= 200 && p.status < 300 {
		upgrade = true
	}
	noResponseBody := p.kind == Response && (p.responseToHEAD || (p.status >= 100 && p.status < 200) || p.status == 204 || p.status == 304 || (p.responseToCONNECT && p.status >= 200 && p.status < 300))
	if noResponseBody {
		p.completeMessage()
		if p.code != CodeNone {
			return false
		}
		if upgrade {
			p.state = stateUpgrade
		}
		return true
	}
	if p.hasTransferEncoding {
		if p.transferChunked {
			p.state = stateChunkSizeStart
			return true
		}
		// A non-chunked final transfer coding on a response is close-delimited.
		p.state = stateCloseBody
		return true
	}
	if p.hasContentLength {
		if p.remaining > p.limits.MaxBodyBytes {
			p.fail(CodeBodyTooLarge)
			return false
		}
		if p.remaining == 0 {
			p.completeMessage()
			if p.code != CodeNone {
				return false
			}
		} else {
			p.state = stateFixedBody
		}
		return true
	}
	if p.kind == Request {
		p.completeMessage()
		if p.code != CodeNone {
			return false
		}
		return true
	}
	p.state = stateCloseBody
	return true
}

func (p *Parser) completeMessage() {
	if p.callbacks != nil && p.callbacks.MessageComplete != nil {
		p.callbacks.MessageComplete(p.message())
		if p.code != CodeNone {
			return
		}
	}
	p.messageNum++
	if p.kind == Response && (p.status == 101 || p.status >= 200) {
		p.exchangeNum++
	}
	p.messageActive = false
	p.trailers = false
	p.state = stateStart
	p.paused = p.pauseAfterMessage
}

func (p *Parser) fail(code Code) Code {
	if p.code == CodeNone {
		p.code = code
		p.state = stateDead
	}
	return p.code
}

func (p *Parser) takeStartByte() bool {
	if p.startBytes == p.limits.MaxStartLineBytes {
		p.fail(CodeStartLineTooLarge)
		return false
	}
	p.startBytes++
	return true
}

func (p *Parser) takePrefixByte() bool {
	if p.prefixBytes == p.limits.MaxStartLineBytes {
		p.fail(CodeStartLineTooLarge)
		return false
	}
	p.prefixBytes++
	return true
}

func (p *Parser) takeHeaderByte() bool {
	if p.headerBytes == p.limits.MaxHeaderBytes {
		p.fail(CodeHeadersTooLarge)
		return false
	}
	p.headerBytes++
	return true
}

func (p *Parser) takeChunkLineByte() bool {
	if p.chunkLineBytes == p.limits.MaxChunkLineBytes {
		p.fail(CodeChunkLineTooLarge)
		return false
	}
	if p.chunkMetadataBytes >= p.limits.MaxChunkMetadataBytes {
		p.fail(CodeChunkMetadataTooLarge)
		return false
	}
	p.chunkLineBytes++
	p.chunkMetadataBytes++
	return true
}

func (p *Parser) addBody(n uint64) bool {
	if n > p.limits.MaxBodyBytes-p.bodyBytes {
		p.fail(CodeBodyTooLarge)
		return false
	}
	p.bodyBytes += n
	return true
}

func (p *Parser) matchMethod(b byte) {
	const (
		getBit     = uint16(1 << (MethodGET - 1))
		headBit    = uint16(1 << (MethodHEAD - 1))
		postBit    = uint16(1 << (MethodPOST - 1))
		putBit     = uint16(1 << (MethodPUT - 1))
		deleteBit  = uint16(1 << (MethodDELETE - 1))
		connectBit = uint16(1 << (MethodCONNECT - 1))
		optionsBit = uint16(1 << (MethodOPTIONS - 1))
		traceBit   = uint16(1 << (MethodTRACE - 1))
		patchBit   = uint16(1 << (MethodPATCH - 1))
	)
	if p.methodPos == 0 {
		switch b {
		case 'G':
			p.methodMask = getBit
		case 'H':
			p.methodMask = headBit
		case 'P':
			p.methodMask = postBit | putBit | patchBit
		case 'D':
			p.methodMask = deleteBit
		case 'C':
			p.methodMask = connectBit
		case 'O':
			p.methodMask = optionsBit
		case 'T':
			p.methodMask = traceBit
		default:
			p.methodMask = 0
		}
	} else {
		mask := p.methodMask
		if mask&(mask-1) == 0 && mask != 0 {
			method := Method(bits.TrailingZeros16(mask) + 1)
			name := methodNames[method]
			if int(p.methodPos) >= len(name) || b != name[p.methodPos] {
				p.methodMask = 0
			}
		} else if mask != 0 {
			if mask&postBit != 0 && (int(p.methodPos) >= len("POST") || b != "POST"[p.methodPos]) {
				p.methodMask &^= postBit
			}
			if mask&putBit != 0 && (int(p.methodPos) >= len("PUT") || b != "PUT"[p.methodPos]) {
				p.methodMask &^= putBit
			}
			if mask&patchBit != 0 && (int(p.methodPos) >= len("PATCH") || b != "PATCH"[p.methodPos]) {
				p.methodMask &^= patchBit
			}
		}
	}
	if p.methodPos != math.MaxUint8 {
		p.methodPos++
	}
}

func (p *Parser) finishMethod() {
	p.method = MethodOther
	for method := Method(1); int(method) < len(methodNames); method++ {
		if p.methodMask&(1<<(method-1)) != 0 && int(p.methodPos) == len(methodNames[method]) {
			p.method = method
			return
		}
	}
}

func (p *Parser) matchField(b byte) {
	const (
		contentLengthBit    = uint16(1 << (fieldContentLength - 1))
		transferEncodingBit = uint16(1 << (fieldTransferEncoding - 1))
		connectionBit       = uint16(1 << (fieldConnection - 1))
		upgradeBit          = uint16(1 << (fieldUpgrade - 1))
		hostBit             = uint16(1 << (fieldHost - 1))
		trailerBit          = uint16(1 << (fieldTrailer - 1))
	)
	lower := asciiLower(b)
	if p.fieldPos == 0 {
		switch lower {
		case 'c':
			p.fieldMask = contentLengthBit | connectionBit
		case 't':
			p.fieldMask = transferEncodingBit | trailerBit
		case 'u':
			p.fieldMask = upgradeBit
		case 'h':
			p.fieldMask = hostBit
		default:
			p.fieldMask = 0
		}
	} else {
		switch p.fieldMask {
		case contentLengthBit:
			if int(p.fieldPos) >= len("content-length") || lower != "content-length"[p.fieldPos] {
				p.fieldMask = 0
			}
		case transferEncodingBit:
			if int(p.fieldPos) >= len("transfer-encoding") || lower != "transfer-encoding"[p.fieldPos] {
				p.fieldMask = 0
			}
		case connectionBit:
			if int(p.fieldPos) >= len("connection") || lower != "connection"[p.fieldPos] {
				p.fieldMask = 0
			}
		case upgradeBit:
			if int(p.fieldPos) >= len("upgrade") || lower != "upgrade"[p.fieldPos] {
				p.fieldMask = 0
			}
		case hostBit:
			if int(p.fieldPos) >= len("host") || lower != "host"[p.fieldPos] {
				p.fieldMask = 0
			}
		case trailerBit:
			if int(p.fieldPos) >= len("trailer") || lower != "trailer"[p.fieldPos] {
				p.fieldMask = 0
			}
		case contentLengthBit | connectionBit:
			if int(p.fieldPos) >= len("content-length") || lower != "content-length"[p.fieldPos] {
				p.fieldMask &^= contentLengthBit
			}
			if int(p.fieldPos) >= len("connection") || lower != "connection"[p.fieldPos] {
				p.fieldMask &^= connectionBit
			}
		case transferEncodingBit | trailerBit:
			if int(p.fieldPos) >= len("transfer-encoding") || lower != "transfer-encoding"[p.fieldPos] {
				p.fieldMask &^= transferEncodingBit
			}
			if int(p.fieldPos) >= len("trailer") || lower != "trailer"[p.fieldPos] {
				p.fieldMask &^= trailerBit
			}
		}
	}
	p.fieldPos++
}

func (p *Parser) consumeFieldValue(b byte) bool {
	switch p.field {
	case fieldContentLength:
		if b == ' ' || b == '\t' {
			if p.contentLengthDigits {
				p.contentLengthTrailing = true
			}
			return true
		}
		if b < '0' || b > '9' || p.contentLengthTrailing {
			p.fail(CodeInvalidContentLength)
			return false
		}
		p.contentLengthDigits = true
		digit := uint64(b - '0')
		if p.remaining > (math.MaxUint64-digit)/10 {
			p.fail(CodeInvalidContentLength)
			return false
		}
		p.remaining = p.remaining*10 + digit
	case fieldTransferEncoding:
		return p.consumeTransferValue(b)
	case fieldConnection:
		return p.consumeConnectionValue(b)
	case fieldHost:
		return p.consumeHostValue(b)
	case fieldUpgrade:
		return p.consumeUpgradeValue(b)
	}
	return true
}

func (p *Parser) consumeTransferValue(b byte) bool {
	switch p.transferState {
	case transferBeforeToken:
		if b == ' ' || b == '\t' {
			return true
		}
		if !isTChar(b) || p.transferSawChunked && p.kind == Request {
			p.fail(CodeInvalidTransferEncoding)
			return false
		}
		p.transferState = transferToken
		p.transferTokenPos = 0
		p.transferCurrentChunked = true
		return p.consumeTransferValue(b)
	case transferToken:
		if isTChar(b) {
			name := "chunked"
			if int(p.transferTokenPos) >= len(name) || asciiLower(b) != name[p.transferTokenPos] {
				p.transferCurrentChunked = false
			}
			if p.transferTokenPos != math.MaxUint8 {
				p.transferTokenPos++
			}
			return true
		}
		if p.transferTokenPos == 0 {
			p.fail(CodeInvalidTransferEncoding)
			return false
		}
		p.finishTransferToken()
		if p.code != CodeNone {
			return false
		}
		if b == ' ' || b == '\t' {
			p.transferState = transferAfterToken
			return true
		}
		if b == ';' {
			if p.transferChunked {
				p.fail(CodeInvalidTransferEncoding)
				return false
			}
			p.transferState = transferParamBeforeName
			return true
		}
		if b == ',' {
			p.transferState = transferBeforeToken
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	case transferAfterToken:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == ';' {
			if p.transferChunked {
				p.fail(CodeInvalidTransferEncoding)
				return false
			}
			p.transferState = transferParamBeforeName
			return true
		}
		if b == ',' {
			p.transferState = transferBeforeToken
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	case transferParamBeforeName:
		if b == ' ' || b == '\t' {
			return true
		}
		if !isTChar(b) {
			p.fail(CodeInvalidTransferEncoding)
			return false
		}
		p.transferState = transferParamName
		return true
	case transferParamName:
		if isTChar(b) {
			return true
		}
		if b == ' ' || b == '\t' {
			p.transferState = transferParamAfterName
			return true
		}
		if b == '=' {
			p.transferState = transferParamBeforeValue
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	case transferParamAfterName:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == '=' {
			p.transferState = transferParamBeforeValue
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	case transferParamBeforeValue:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == '"' {
			p.transferState = transferParamQuoted
			return true
		}
		if isTChar(b) {
			p.transferState = transferParamTokenValue
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	case transferParamTokenValue:
		if isTChar(b) {
			return true
		}
		if b == ' ' || b == '\t' {
			p.transferState = transferParamAfterValue
			return true
		}
		if b == ';' {
			p.transferState = transferParamBeforeName
			return true
		}
		if b == ',' {
			p.transferState = transferBeforeToken
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	case transferParamQuoted:
		if b == '\\' {
			p.transferState = transferParamQuotedEscape
		} else if b == '"' {
			p.transferState = transferParamAfterValue
		} else if !isQDText(b) {
			p.fail(CodeInvalidTransferEncoding)
			return false
		}
		return true
	case transferParamQuotedEscape:
		if !isQuotedPairByte(b) {
			p.fail(CodeInvalidTransferEncoding)
			return false
		}
		p.transferState = transferParamQuoted
		return true
	case transferParamAfterValue:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == ';' {
			p.transferState = transferParamBeforeName
			return true
		}
		if b == ',' {
			p.transferState = transferBeforeToken
			return true
		}
		p.fail(CodeInvalidTransferEncoding)
		return false
	default:
		p.fail(CodeInvalidTransferEncoding)
		return false
	}
}

func (p *Parser) finishTransferToken() {
	p.transferSawToken = true
	p.transferCurrentChunked = p.transferCurrentChunked && p.transferTokenPos == uint8(len("chunked"))
	p.transferChunked = p.transferCurrentChunked
	if p.transferCurrentChunked {
		if p.transferSawChunked {
			p.fail(CodeInvalidTransferEncoding)
			return
		}
		p.transferSawChunked = true
	}
}

func (p *Parser) finishTransferValue() bool {
	switch p.transferState {
	case transferToken:
		p.finishTransferToken()
		if p.code != CodeNone {
			return false
		}
	case transferAfterToken, transferParamTokenValue, transferParamAfterValue:
		// Token was already committed and any parameter is complete.
	case transferBeforeToken, transferParamBeforeName, transferParamName, transferParamAfterName,
		transferParamBeforeValue, transferParamQuoted, transferParamQuotedEscape:
		p.fail(CodeInvalidTransferEncoding)
		return false
	default:
		p.fail(CodeInvalidTransferEncoding)
		return false
	}
	if !p.transferSawToken {
		p.fail(CodeInvalidTransferEncoding)
		return false
	}
	return true
}

func (p *Parser) consumeUpgradeValue(b byte) bool {
	switch p.upgradeState {
	case upgradeBeforeProtocol:
		if b == ' ' || b == '\t' {
			return true
		}
		if isTChar(b) {
			p.upgradeState = upgradeName
			p.upgradeSawProtocol = true
			return true
		}
	case upgradeName:
		if isTChar(b) {
			return true
		}
		if b == '/' {
			p.upgradeState = upgradeVersionStart
			return true
		}
		if b == ' ' || b == '\t' {
			p.upgradeState = upgradeAfterName
			return true
		}
		if b == ',' {
			p.upgradeState = upgradeBeforeProtocol
			return true
		}
	case upgradeAfterName:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == ',' {
			p.upgradeState = upgradeBeforeProtocol
			return true
		}
	case upgradeVersionStart:
		if isTChar(b) {
			p.upgradeState = upgradeVersion
			return true
		}
	case upgradeVersion:
		if isTChar(b) {
			return true
		}
		if b == ' ' || b == '\t' {
			p.upgradeState = upgradeAfterProtocol
			return true
		}
		if b == ',' {
			p.upgradeState = upgradeBeforeProtocol
			return true
		}
	case upgradeAfterProtocol:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == ',' {
			p.upgradeState = upgradeBeforeProtocol
			return true
		}
	}
	p.fail(CodeInvalidHeaderValue)
	return false
}

func (p *Parser) finishUpgradeValue() bool {
	if !p.upgradeSawProtocol {
		p.fail(CodeInvalidHeaderValue)
		return false
	}
	switch p.upgradeState {
	case upgradeName, upgradeVersion, upgradeAfterName, upgradeAfterProtocol:
		p.hasUpgrade = true
		return true
	default:
		p.fail(CodeInvalidHeaderValue)
		return false
	}
}

func (p *Parser) consumeHostValue(b byte) bool {
	switch p.hostState {
	case hostStart:
		if b == '[' {
			p.hostNonEmpty = true
			p.hostLiteralNonEmpty = false
			p.ipLiteralLen = 0
			p.hostState = hostIPLiteral
			return true
		}
		if isRegNameChar(b) {
			p.hostNonEmpty = true
			p.hostState = hostRegName
			return true
		}
		if b == '%' {
			p.hostNonEmpty = true
			p.hostState = hostPercentFirst
			return true
		}
	case hostRegName:
		if isRegNameChar(b) {
			return true
		}
		if b == '%' {
			p.hostState = hostPercentFirst
			return true
		}
		if b == ':' {
			p.hostState = hostPortStart
			return true
		}
		if b == ' ' || b == '\t' {
			p.hostState = hostTrailing
			return true
		}
	case hostPercentFirst:
		if isHexByte(b) {
			p.hostState = hostPercentSecond
			return true
		}
	case hostPercentSecond:
		if isHexByte(b) {
			p.hostState = hostRegName
			return true
		}
	case hostIPLiteral:
		if b == ']' {
			if !p.hostLiteralNonEmpty || !validIPLiteral(p.ipLiteral[:p.ipLiteralLen]) {
				break
			}
			p.hostState = hostAfterLiteral
			return true
		}
		if p.appendIPLiteral(b) {
			p.hostLiteralNonEmpty = true
			return true
		}
	case hostAfterLiteral:
		if b == ':' {
			p.hostState = hostPortStart
			return true
		}
		if b == ' ' || b == '\t' {
			p.hostState = hostTrailing
			return true
		}
	case hostPortStart:
		if b >= '0' && b <= '9' {
			p.hostState = hostPort
			return true
		}
		if b == ' ' || b == '\t' {
			p.hostState = hostTrailing
			return true
		}
	case hostPort:
		if b >= '0' && b <= '9' {
			return true
		}
		if b == ' ' || b == '\t' {
			p.hostState = hostTrailing
			return true
		}
	case hostTrailing:
		if b == ' ' || b == '\t' {
			return true
		}
	}
	p.fail(CodeInvalidHeaderValue)
	return false
}

func (p *Parser) finishHostValue() bool {
	if !p.hostNonEmpty {
		p.fail(CodeMissingHost)
		return false
	}
	switch p.hostState {
	case hostRegName, hostAfterLiteral, hostPortStart, hostPort, hostTrailing:
		return true
	default:
		p.fail(CodeInvalidHeaderValue)
		return false
	}
}

func (p *Parser) consumeConnectionValue(b byte) bool {
	switch p.connectionState {
	case connectionBeforeToken:
		if b == ' ' || b == '\t' {
			return true
		}
		if !isTChar(b) {
			p.fail(CodeInvalidHeaderValue)
			return false
		}
		p.connectionState = connectionToken
		p.connectionTokenPos = 0
		p.connectionCloseMatch = true
		p.connectionUpgradeMatch = true
		p.connectionKeepAliveMatch = true
		return p.consumeConnectionValue(b)
	case connectionToken:
		if isTChar(b) {
			pos := int(p.connectionTokenPos)
			lower := asciiLower(b)
			if pos >= len("close") || lower != "close"[pos] {
				p.connectionCloseMatch = false
			}
			if pos >= len("upgrade") || lower != "upgrade"[pos] {
				p.connectionUpgradeMatch = false
			}
			if pos >= len("keep-alive") || lower != "keep-alive"[pos] {
				p.connectionKeepAliveMatch = false
			}
			if p.connectionTokenPos != math.MaxUint8 {
				p.connectionTokenPos++
			}
			return true
		}
		p.finishConnectionToken()
		if b == ' ' || b == '\t' {
			p.connectionState = connectionAfterToken
			return true
		}
		if b == ',' {
			p.connectionState = connectionBeforeToken
			return true
		}
		p.fail(CodeInvalidHeaderValue)
		return false
	case connectionAfterToken:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == ',' {
			p.connectionState = connectionBeforeToken
			return true
		}
		p.fail(CodeInvalidHeaderValue)
		return false
	default:
		p.fail(CodeInvalidHeaderValue)
		return false
	}
}

func (p *Parser) finishConnectionToken() {
	length := int(p.connectionTokenPos)
	p.connectionClose = p.connectionClose || (p.connectionCloseMatch && length == len("close"))
	p.connectionUpgrade = p.connectionUpgrade || (p.connectionUpgradeMatch && length == len("upgrade"))
	p.connectionKeepAlive = p.connectionKeepAlive || (p.connectionKeepAliveMatch && length == len("keep-alive"))
}

func (p *Parser) finishConnectionValue() bool {
	switch p.connectionState {
	case connectionToken:
		p.finishConnectionToken()
	case connectionAfterToken:
		// Already committed.
	default:
		p.fail(CodeInvalidHeaderValue)
		return false
	}
	return true
}

func (p *Parser) consumeChunkExtension(b byte) bool {
	switch p.chunkExtState {
	case chunkExtBeforeName:
		if b == ' ' || b == '\t' {
			return true
		}
		if !isTChar(b) {
			p.fail(CodeInvalidChunkExtension)
			return false
		}
		p.chunkExtState = chunkExtName
		return true
	case chunkExtName:
		if isTChar(b) {
			return true
		}
		if b == ' ' || b == '\t' {
			p.chunkExtState = chunkExtAfterName
			return true
		}
		if b == '=' {
			p.chunkExtState = chunkExtBeforeValue
			return true
		}
		if b == ';' {
			p.chunkExtState = chunkExtBeforeName
			return true
		}
		p.fail(CodeInvalidChunkExtension)
		return false
	case chunkExtAfterName:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == '=' {
			p.chunkExtState = chunkExtBeforeValue
			return true
		}
		if b == ';' {
			p.chunkExtState = chunkExtBeforeName
			return true
		}
		p.fail(CodeInvalidChunkExtension)
		return false
	case chunkExtBeforeValue:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == '"' {
			p.chunkExtState = chunkExtQuotedValue
			return true
		}
		if isTChar(b) {
			p.chunkExtState = chunkExtTokenValue
			return true
		}
		p.fail(CodeInvalidChunkExtension)
		return false
	case chunkExtTokenValue:
		if isTChar(b) {
			return true
		}
		if b == ' ' || b == '\t' {
			p.chunkExtState = chunkExtAfterValue
			return true
		}
		if b == ';' {
			p.chunkExtState = chunkExtBeforeName
			return true
		}
		p.fail(CodeInvalidChunkExtension)
		return false
	case chunkExtQuotedValue:
		if b == '\\' {
			p.chunkExtState = chunkExtQuotedEscape
			return true
		}
		if b == '"' {
			p.chunkExtState = chunkExtAfterValue
			return true
		}
		if !isQDText(b) {
			p.fail(CodeInvalidChunkExtension)
			return false
		}
		return true
	case chunkExtQuotedEscape:
		if !isQuotedPairByte(b) {
			p.fail(CodeInvalidChunkExtension)
			return false
		}
		p.chunkExtState = chunkExtQuotedValue
		return true
	case chunkExtAfterValue:
		if b == ' ' || b == '\t' {
			return true
		}
		if b == ';' {
			p.chunkExtState = chunkExtBeforeName
			return true
		}
		p.fail(CodeInvalidChunkExtension)
		return false
	default:
		p.fail(CodeInvalidChunkExtension)
		return false
	}
}

func (p *Parser) finishChunkExtension() bool {
	switch p.chunkExtState {
	case chunkExtName, chunkExtAfterName, chunkExtTokenValue, chunkExtAfterValue:
		return true
	default:
		p.fail(CodeInvalidChunkExtension)
		return false
	}
}

func validHTTPVersion(major, minor uint16) bool {
	return major == 1 && (minor == 0 || minor == 1)
}

func (p *Parser) consumeTargetForm(b byte) bool {
	if p.targetBytes == 0 {
		switch {
		case b == '/':
			p.targetForm = targetOrigin
		case b == '*':
			p.targetForm = targetAsterisk
		case p.method == MethodCONNECT:
			p.targetForm = targetAuthority
			switch b {
			case '[':
				p.targetAuthorityBracket = true
				p.ipLiteralLen = 0
			case ':', '/', '?', '@', ']':
				p.fail(CodeInvalidTarget)
				return false
			}
		case b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z':
			p.targetForm = targetScheme
		default:
			p.fail(CodeInvalidTarget)
			return false
		}
		return true
	}

	switch p.targetForm {
	case targetAsterisk:
		p.fail(CodeInvalidTarget)
		return false
	case targetScheme:
		if b == ':' {
			p.targetForm = targetAbsolute
			return true
		}
		if !isSchemeChar(b) {
			p.fail(CodeInvalidTarget)
			return false
		}
	case targetAuthority:
		if p.targetAuthorityBracket {
			if b == ']' {
				if !validIPLiteral(p.ipLiteral[:p.ipLiteralLen]) {
					p.fail(CodeInvalidTarget)
					return false
				}
				p.targetAuthorityBracket = false
				p.targetAuthorityClosed = true
				return true
			}
			if !p.appendIPLiteral(b) {
				p.fail(CodeInvalidTarget)
				return false
			}
			return true
		}
		if !p.targetAuthorityColon {
			if p.targetAuthorityClosed && b != ':' {
				p.fail(CodeInvalidTarget)
				return false
			}
			switch b {
			case ':':
				p.targetAuthorityColon = true
				return true
			case '/', '?', '@', '[', ']':
				p.fail(CodeInvalidTarget)
				return false
			}
			return true
		}
		if b < '0' || b > '9' {
			p.fail(CodeInvalidTarget)
			return false
		}
		p.targetAuthorityPort = true
	}
	return true
}

func (p *Parser) finishTarget() bool {
	if p.targetBytes == 0 || p.targetPercent != 0 || p.targetForm == targetScheme {
		p.fail(CodeInvalidTarget)
		return false
	}
	if p.targetForm == targetAuthority && (p.targetAuthorityBracket || !p.targetAuthorityColon || !p.targetAuthorityPort) {
		p.fail(CodeInvalidTarget)
		return false
	}
	return true
}

func isSchemeChar(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '+' || b == '-' || b == '.'
}

func validTargetByte(b byte) bool {
	if b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' {
		return true
	}
	switch b {
	case '!', '$', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/', ':', ';', '=', '?', '@', '[', ']', '_', '~':
		return true
	default:
		return false
	}
}

func isQDText(b byte) bool {
	return b == '\t' || b == ' ' || b == '!' || b >= 0x23 && b <= 0x5b || b >= 0x5d && b <= 0x7e || b >= 0x80
}

func isQuotedPairByte(b byte) bool {
	return b == '\t' || b == ' ' || b >= 0x21 && b <= 0x7e || b >= 0x80
}

func validReasonByte(b byte) bool {
	return b == '\t' || b >= 0x20 && b != 0x7f
}

func validFieldValueByte(b byte) bool {
	return b == '\t' || b >= 0x20 && b != 0x7f
}

func asciiLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func isTChar(b byte) bool {
	if b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' {
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func isRegNameChar(b byte) bool {
	if b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' {
		return true
	}
	switch b {
	case '!', '$', '&', '\'', '(', ')', '*', '+', '-', '.', ';', '=', '_', '~':
		return true
	default:
		return false
	}
}

func (p *Parser) appendIPLiteral(b byte) bool {
	if int(p.ipLiteralLen) == len(p.ipLiteral) {
		return false
	}
	p.ipLiteral[p.ipLiteralLen] = b
	p.ipLiteralLen++
	return true
}

// validIPLiteral delegates address syntax to net/netip, the address
// representation used by wago-org/net. URI percent escapes are decoded first;
// IPvFuture is intentionally rejected because the network backend supports
// concrete IPv4 and IPv6 addresses only.
func validIPLiteral(raw []byte) bool {
	if len(raw) == 0 || len(raw) > maxIPLiteralBytes {
		return false
	}
	var decoded [maxIPLiteralBytes]byte
	written := 0
	for offset := 0; offset < len(raw); offset++ {
		b := raw[offset]
		if b == '%' {
			if offset+2 >= len(raw) {
				return false
			}
			high, highOK := hexValue(raw[offset+1])
			low, lowOK := hexValue(raw[offset+2])
			if !highOK || !lowOK {
				return false
			}
			b = high<<4 | low
			offset += 2
		}
		if b == 0 || written == len(decoded) {
			return false
		}
		decoded[written] = b
		written++
	}
	address, err := netip.ParseAddr(string(decoded[:written]))
	return err == nil && address.Is6()
}

func isHexByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func hexValue(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}
