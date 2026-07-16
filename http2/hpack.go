package http2

import (
	"errors"

	"golang.org/x/net/http2/hpack"
)

var (
	ErrHeaderDecoderState = errors.New("wagohttp/http2: invalid header decoder state")
	ErrHeaderListTooLarge = errors.New("wagohttp/http2: decoded header list too large")
	ErrTooManyHeaders     = errors.New("wagohttp/http2: too many decoded headers")
)

// HeaderField is one decoded HPACK field. Name and Value are immutable for the
// duration of the callback. Sensitive reports the never-indexed representation.
type HeaderField struct {
	Name      string
	Value     string
	Sensitive bool
}

// HeaderLimits bounds HPACK decoder-controlled output. Header-list accounting
// uses the RFC 7541 size formula: name bytes + value bytes + 32 per field.
type HeaderLimits struct {
	MaxDynamicTableBytes uint32
	MaxFieldBytes        uint32
	MaxHeaderListBytes   uint64
	MaxHeaders           uint32
}

const (
	defaultMaxDynamicTableBytes = 4 << 10
	defaultMaxFieldBytes        = 16 << 10
	defaultMaxHeaderListBytes   = 64 << 10
	defaultMaxHeaders           = 256
)

func (limits HeaderLimits) normalized() HeaderLimits {
	if limits.MaxDynamicTableBytes == 0 {
		limits.MaxDynamicTableBytes = defaultMaxDynamicTableBytes
	}
	if limits.MaxFieldBytes == 0 {
		limits.MaxFieldBytes = defaultMaxFieldBytes
	}
	if limits.MaxHeaderListBytes == 0 {
		limits.MaxHeaderListBytes = defaultMaxHeaderListBytes
	}
	if limits.MaxHeaders == 0 {
		limits.MaxHeaders = defaultMaxHeaders
	}
	return limits
}

// HeaderDecoder incrementally decodes HPACK blocks while preserving dynamic
// table state across blocks. BeginBlock and EndBlock delimit each block; Write
// accepts arbitrary fragmentation, including one byte at a time.
type HeaderDecoder struct {
	decoder  *hpack.Decoder
	limits   HeaderLimits
	emit     func(HeaderField)
	active   bool
	fields   uint32
	listSize uint64
	err      error
}

// NewHeaderDecoder constructs a bounded HPACK decoder. Construction allocates
// the compression table; decoding does not retain encoded input spans.
func NewHeaderDecoder(limits HeaderLimits, emit func(HeaderField)) *HeaderDecoder {
	limits = limits.normalized()
	result := &HeaderDecoder{limits: limits, emit: emit}
	result.decoder = hpack.NewDecoder(limits.MaxDynamicTableBytes, result.emitField)
	result.decoder.SetAllowedMaxDynamicTableSize(limits.MaxDynamicTableBytes)
	maxStringBytes := uint64(limits.MaxFieldBytes)
	maxInt := uint64(^uint(0) >> 1)
	if maxStringBytes > maxInt {
		maxStringBytes = maxInt
	}
	result.decoder.SetMaxStringLength(int(maxStringBytes))
	return result
}

// BeginBlock starts one HPACK header block.
func (decoder *HeaderDecoder) BeginBlock() error {
	if decoder == nil || decoder.active {
		return ErrHeaderDecoderState
	}
	decoder.active = true
	decoder.fields = 0
	decoder.listSize = 0
	decoder.err = nil
	return nil
}

// Write decodes one fragment of the active block.
func (decoder *HeaderDecoder) Write(src []byte) (int, error) {
	if decoder == nil || !decoder.active {
		return 0, ErrHeaderDecoderState
	}
	if decoder.err != nil {
		return 0, decoder.err
	}
	n, err := decoder.decoder.Write(src)
	if decoder.err != nil {
		return n, decoder.err
	}
	return n, err
}

// EndBlock validates that the current HPACK representation is complete.
func (decoder *HeaderDecoder) EndBlock() error {
	if decoder == nil || !decoder.active {
		return ErrHeaderDecoderState
	}
	decoder.active = false
	err := decoder.decoder.Close()
	if decoder.err != nil {
		return decoder.err
	}
	return err
}

// SetAllowedDynamicTableBytes applies a peer SETTINGS_HEADER_TABLE_SIZE bound.
// It is rejected in the middle of a block.
func (decoder *HeaderDecoder) SetAllowedDynamicTableBytes(size uint32) error {
	if decoder == nil || decoder.active || size > decoder.limits.MaxDynamicTableBytes {
		return ErrHeaderDecoderState
	}
	decoder.decoder.SetAllowedMaxDynamicTableSize(size)
	return nil
}

func (decoder *HeaderDecoder) emitField(field hpack.HeaderField) {
	if decoder.err != nil {
		return
	}
	decoder.fields++
	if decoder.fields > decoder.limits.MaxHeaders {
		decoder.err = ErrTooManyHeaders
		return
	}
	size := uint64(len(field.Name)) + uint64(len(field.Value)) + 32
	if size > decoder.limits.MaxHeaderListBytes-decoder.listSize {
		decoder.err = ErrHeaderListTooLarge
		return
	}
	decoder.listSize += size
	if decoder.emit != nil {
		decoder.emit(HeaderField{Name: field.Name, Value: field.Value, Sensitive: field.Sensitive})
	}
}
