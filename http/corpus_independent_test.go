package http

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type corpusExpectation uint8

const (
	corpusAccept corpusExpectation = iota
	corpusReject
	corpusIncomplete
)

type independentCorpusCase struct {
	location    string
	kind        Kind
	input       []byte
	expectation corpusExpectation
	limits      Limits
}

func TestHTTPParseCorpus(t *testing.T) {
	checkout := filepath.Join("..", "testdata", "upstream", "httparse")
	sourcePath := filepath.Join(checkout, "src", "lib.rs")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned httparse corpus not fetched; run scripts/fetch-test-corpora.sh")
		}
		t.Fatal(err)
	}
	verifyCorpusPin(t, "httparse", checkout)

	cases, err := parseHTTPParseCases(string(source))
	if err != nil {
		t.Fatal(err)
	}
	uriSource, err := os.ReadFile(filepath.Join(checkout, "tests", "uri.rs"))
	if err != nil {
		t.Fatal(err)
	}
	uriCases, err := parseHTTPParseURICases(string(uriSource))
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, uriCases...)
	tested, skipped := 0, 0
	for _, test := range cases {
		if reason := httparseRFC9112Skip(test); reason != "" {
			skipped++
			continue
		}
		tested++
		t.Run(test.location, func(t *testing.T) {
			runIndependentCorpusCase(t, test)
		})
	}
	if tested < 150 {
		t.Fatalf("only %d httparse cases tested (%d classified RFC/API deltas)", tested, skipped)
	}
	t.Logf("validated %d pinned httparse cases; skipped %d documented RFC/API deltas", tested, skipped)
}

func TestPicoHTTPParserCorpus(t *testing.T) {
	checkout := filepath.Join("..", "testdata", "upstream", "picohttpparser")
	sourcePath := filepath.Join(checkout, "test.c")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("pinned picohttpparser corpus not fetched; run scripts/fetch-test-corpora.sh")
		}
		t.Fatal(err)
	}
	verifyCorpusPin(t, "picohttpparser", checkout)

	cases, err := parsePicoHTTPParserCases(string(source))
	if err != nil {
		t.Fatal(err)
	}
	tested, skipped := 0, 0
	for _, test := range cases {
		if reason := picoRFC9112Skip(test); reason != "" {
			skipped++
			continue
		}
		tested++
		t.Run(test.location, func(t *testing.T) {
			runIndependentCorpusCase(t, test)
		})
	}
	if tested < 50 {
		t.Fatalf("only %d picohttpparser cases tested (%d classified RFC/API deltas)", tested, skipped)
	}
	t.Logf("validated %d pinned picohttpparser cases; skipped %d documented RFC/API deltas", tested, skipped)
}

func parseHTTPParseCases(source string) ([]independentCorpusCase, error) {
	clean := stripSourceComments(source)
	cases, err := parseHTTPParseMacroCases(clean, "src/lib.rs", []struct {
		name string
		kind Kind
	}{
		{name: "req", kind: Request},
		{name: "res", kind: Response},
	})
	if err != nil {
		return nil, err
	}
	chunkCases, err := parseHTTPParseChunkSizeCases(clean)
	if err != nil {
		return nil, err
	}
	return append(cases, chunkCases...), nil
}

func parseHTTPParseURICases(source string) ([]independentCorpusCase, error) {
	clean := stripSourceComments(source)
	return parseHTTPParseMacroCases(clean, "tests/uri.rs", []struct {
		name string
		kind Kind
	}{{name: "req", kind: Request}})
}

func parseHTTPParseMacroCases(source, file string, macros []struct {
	name string
	kind Kind
}) ([]independentCorpusCase, error) {
	var cases []independentCorpusCase
	for _, macro := range macros {
		calls, err := extractRustMacroCalls(source, macro.name)
		if err != nil {
			return nil, fmt.Errorf("httparse %s fixtures: %w", macro.name, err)
		}
		for _, call := range calls {
			if len(call.args) < 3 || strings.Contains(call.args[0], "$") {
				continue
			}
			name := strings.TrimSpace(call.args[0])
			input, err := resolveRustByteExpression(source, call.args[1])
			if err != nil {
				continue
			}
			expectation := corpusAccept
			if len(call.args) >= 4 {
				expected := call.args[2]
				switch {
				case strings.Contains(expected, "Err("):
					expectation = corpusReject
				case strings.Contains(expected, "Partial"):
					expectation = corpusIncomplete
				case strings.Contains(expected, "Complete"):
					expectation = corpusAccept
				default:
					continue
				}
			}
			cases = append(cases, independentCorpusCase{
				location:    fmt.Sprintf("%s/%s:%d/%s", macro.name, file, call.line, sanitizeTestName(name)),
				kind:        macro.kind,
				input:       input,
				expectation: expectation,
			})
		}
	}
	return cases, nil
}

func parseHTTPParseChunkSizeCases(source string) ([]independentCorpusCase, error) {
	start := strings.Index(source, "fn test_chunk_size()")
	if start < 0 {
		return nil, fmt.Errorf("httparse chunk-size tests not found")
	}
	open := strings.IndexByte(source[start:], '{')
	if open < 0 {
		return nil, fmt.Errorf("httparse chunk-size test body not found")
	}
	open += start
	end, err := matchingSourceDelimiter(source, open, '{', '}')
	if err != nil {
		return nil, err
	}
	fragment := source[open+1 : end]
	baseLine := 1 + strings.Count(source[:open+1], "\n")
	var cases []independentCorpusCase
	for lineOffset, line := range strings.Split(fragment, "\n") {
		if !strings.Contains(line, "parse_chunk_size(") {
			continue
		}
		calls, err := extractSourceCalls(line, "parse_chunk_size")
		if err != nil || len(calls) != 1 || len(calls[0].args) != 1 {
			return nil, fmt.Errorf("httparse src/lib.rs:%d: malformed chunk-size fixture", baseLine+lineOffset)
		}
		encoded, err := resolveRustByteExpression(source, calls[0].args[0])
		if err != nil {
			return nil, fmt.Errorf("httparse src/lib.rs:%d: %w", baseLine+lineOffset, err)
		}
		expectation := corpusIncomplete
		if strings.Contains(line, "Err(") {
			expectation = corpusReject
		} else if strings.Contains(line, "Complete((3, 0))") {
			encoded = append(encoded, '\r', '\n')
			expectation = corpusAccept
		}
		cases = append(cases, independentCorpusCase{
			location:    fmt.Sprintf("chunk-size/src/lib.rs:%d", baseLine+lineOffset),
			kind:        Request,
			input:       wrapChunkedRequest(encoded),
			expectation: expectation,
			limits:      Limits{MaxBodyBytes: math.MaxUint64},
		})
	}
	return cases, nil
}

func httparseRFC9112Skip(test independentCorpusCase) string {
	if bytes.Contains(test.input, []byte("3735ab1 ;")) {
		return "httparse accepts whitespace between chunk-size and chunk extension"
	}
	if bytes.Contains(test.input, []byte(";foo bar*")) {
		return "httparse does not validate chunk-extension grammar"
	}
	if test.expectation != corpusReject && hasBareLF(test.input) {
		return "httparse accepts bare LF line endings"
	}
	lineEnd := bytes.Index(test.input, []byte("\r\n"))
	if lineEnd < 0 {
		lineEnd = len(test.input)
	}
	startLine := test.input[:lineEnd]
	if test.expectation == corpusAccept && test.kind == Request {
		if bytes.Contains(test.input, []byte("HTTP/1.1")) && bytes.Contains(test.input, []byte("\r\n\r\n")) {
			host, present := corpusHostValue(test.input)
			if !present {
				return "httparse is a syntax-only parser and does not enforce the HTTP/1.1 Host requirement"
			}
			if len(host) == 0 {
				return "httparse accepts an empty HTTP/1.1 Host field value"
			}
		}
		if !rfcCompatibleRequestTarget(startLine) {
			return "httparse accepts non-RFC URI characters in request-target"
		}
	}
	if test.expectation == corpusAccept && test.kind == Response {
		if len(startLine) == len("HTTP/1.1 200") && bytes.HasPrefix(startLine, []byte("HTTP/1.")) {
			return "httparse accepts a status-line without the RFC-required SP after status-code"
		}
		if bytes.HasPrefix(startLine, []byte("HTTP/1.1 101")) && (!bytes.Contains(bytes.ToLower(test.input), []byte("\r\nconnection:")) || !bytes.Contains(bytes.ToLower(test.input), []byte("\r\nupgrade:"))) {
			return "httparse parses status-line syntax without enforcing 101 upgrade fields"
		}
	}
	return ""
}

func corpusHostValue(input []byte) ([]byte, bool) {
	lower := bytes.ToLower(input)
	marker := []byte("\r\nhost:")
	at := bytes.Index(lower, marker)
	if at < 0 {
		return nil, false
	}
	value := input[at+len(marker):]
	if end := bytes.Index(value, []byte("\r\n")); end >= 0 {
		value = value[:end]
	}
	return bytes.Trim(value, " \t"), true
}

func rfcCompatibleRequestTarget(startLine []byte) bool {
	firstSpace := bytes.IndexByte(startLine, ' ')
	lastSpace := bytes.LastIndexByte(startLine, ' ')
	if firstSpace < 0 || lastSpace <= firstSpace {
		return true
	}
	target := startLine[firstSpace+1 : lastSpace]
	if len(target) == 0 {
		return false
	}
	switch {
	case target[0] == '/':
	case target[0] == '*':
		if len(target) != 1 {
			return false
		}
	case target[0] >= 'A' && target[0] <= 'Z' || target[0] >= 'a' && target[0] <= 'z':
		colon := bytes.IndexByte(target, ':')
		if colon <= 0 {
			return false
		}
		for _, b := range target[:colon] {
			if !isSchemeChar(b) {
				return false
			}
		}
	default:
		return false
	}
	for index, b := range target {
		if !validTargetByte(b) {
			return false
		}
		if b == '%' {
			if index+2 >= len(target) || !isHexByte(target[index+1]) || !isHexByte(target[index+2]) {
				return false
			}
		}
	}
	return true
}

func parsePicoHTTPParserCases(source string) ([]independentCorpusCase, error) {
	sections := []struct {
		name  string
		end   string
		kind  Kind
		label string
	}{
		{name: "static void test_request(void)", end: "static void test_response(void)", kind: Request, label: "request"},
		{name: "static void test_response(void)", end: "static void test_headers(void)", kind: Response, label: "response"},
	}
	var cases []independentCorpusCase
	for _, section := range sections {
		start := strings.Index(source, section.name)
		if start < 0 {
			return nil, fmt.Errorf("picohttpparser %s section not found", section.label)
		}
		end := strings.Index(source[start:], section.end)
		if end < 0 {
			return nil, fmt.Errorf("picohttpparser %s section end not found", section.label)
		}
		fragment := source[start : start+end]
		baseLine := 1 + strings.Count(source[:start], "\n")
		calls, err := extractSourceCalls(fragment, "PARSE")
		if err != nil {
			return nil, fmt.Errorf("picohttpparser %s: %w", section.label, err)
		}
		for _, call := range calls {
			if len(call.args) != 4 {
				continue
			}
			input, err := decodeCStringExpression(call.args[0])
			if err != nil {
				return nil, fmt.Errorf("picohttpparser test.c:%d input: %w", baseLine+call.line-1, err)
			}
			commentBytes, err := decodeCStringExpression(call.args[3])
			if err != nil {
				return nil, fmt.Errorf("picohttpparser test.c:%d comment: %w", baseLine+call.line-1, err)
			}
			expectation, err := picoExpectation(call.args[2])
			if err != nil {
				return nil, fmt.Errorf("picohttpparser test.c:%d: %w", baseLine+call.line-1, err)
			}
			cases = append(cases, independentCorpusCase{
				location:    fmt.Sprintf("%s/test.c:%d/%s", section.label, baseLine+call.line-1, sanitizeTestName(string(commentBytes))),
				kind:        section.kind,
				input:       input,
				expectation: expectation,
			})
		}
	}

	chunkCases, err := parsePicoChunkCases(source)
	if err != nil {
		return nil, err
	}
	cases = append(cases, chunkCases...)
	return cases, nil
}

func parsePicoChunkCases(source string) ([]independentCorpusCase, error) {
	start := strings.Index(source, "static void test_chunked(void)")
	end := strings.Index(source, "static void test_chunked_consume_trailer(void)")
	if start < 0 || end < start {
		return nil, fmt.Errorf("picohttpparser chunked section not found")
	}
	fragment := source[start:end]
	baseLine := 1 + strings.Count(source[:start], "\n")
	var cases []independentCorpusCase

	validCalls, err := extractSourceCalls(fragment, "chunked_test_runners[i]")
	if err != nil {
		return nil, err
	}
	for _, call := range validCalls {
		if len(call.args) != 5 {
			continue
		}
		encoded, err := decodeCStringExpression(call.args[2])
		if err != nil {
			return nil, fmt.Errorf("picohttpparser test.c:%d chunk: %w", baseLine+call.line-1, err)
		}
		// picohttpparser's consume_trailer=0 API stops at the zero-sized
		// chunk line. Complete HTTP/1 wire syntax still requires the empty
		// trailer section, so the adapter supplies its terminating CRLF.
		if !bytes.HasSuffix(encoded, []byte("\r\n\r\n")) {
			encoded = append(encoded, '\r', '\n')
		}
		cases = append(cases, independentCorpusCase{
			location:    fmt.Sprintf("chunked/test.c:%d/valid", baseLine+call.line-1),
			kind:        Request,
			input:       wrapChunkedRequest(encoded),
			expectation: corpusAccept,
			limits:      Limits{MaxBodyBytes: math.MaxUint64},
		})
	}

	failureCalls, err := extractSourceCalls(fragment, "test_chunked_failure")
	if err != nil {
		return nil, err
	}
	for _, call := range failureCalls {
		if len(call.args) != 3 {
			continue
		}
		encoded, err := decodeCStringExpression(call.args[1])
		if err != nil {
			// The function declaration is also seen by the source scanner.
			continue
		}
		expectation := corpusReject
		if strings.TrimSpace(call.args[2]) == "-2" {
			expectation = corpusIncomplete
		}
		cases = append(cases, independentCorpusCase{
			location:    fmt.Sprintf("chunked/test.c:%d/failure", baseLine+call.line-1),
			kind:        Request,
			input:       wrapChunkedRequest(encoded),
			expectation: expectation,
			limits:      Limits{MaxBodyBytes: math.MaxUint64},
		})
	}
	return cases, nil
}

func wrapChunkedRequest(encoded []byte) []byte {
	const prefix = "POST / HTTP/1.1\r\nHost: corpus.test\r\nTransfer-Encoding: chunked\r\n\r\n"
	input := make([]byte, len(prefix)+len(encoded))
	copy(input, prefix)
	copy(input[len(prefix):], encoded)
	return input
}

func picoExpectation(expression string) (corpusExpectation, error) {
	switch strings.TrimSpace(expression) {
	case "0":
		return corpusAccept, nil
	case "-1":
		return corpusReject, nil
	case "-2":
		return corpusIncomplete, nil
	default:
		return 0, fmt.Errorf("unsupported expected result %q", expression)
	}
}

func picoRFC9112Skip(test independentCorpusCase) string {
	if hasBareLF(test.input) {
		return "picohttpparser accepts bare LF line endings"
	}
	if bytes.Contains(test.input, []byte("\r\n6 ;")) {
		return "picohttpparser accepts whitespace between chunk-size and chunk extension"
	}
	if test.expectation == corpusIncomplete && bytes.Contains(test.input, []byte("ffffffffffffffff\r\n")) {
		return "picohttpparser has no cumulative decoded-body bound"
	}
	if test.expectation != corpusAccept {
		return ""
	}
	lineEnd := bytes.Index(test.input, []byte("\r\n"))
	if lineEnd < 0 {
		lineEnd = bytes.IndexByte(test.input, '\n')
	}
	if lineEnd < 0 {
		lineEnd = len(test.input)
	}
	startLine := test.input[:lineEnd]
	if bytes.Contains(startLine, []byte("  ")) {
		return "picohttpparser compatibility mode accepts multiple start-line spaces"
	}
	if bytes.Contains(test.input, []byte("\r\n ")) || bytes.Contains(test.input, []byte("\r\n\t")) {
		return "obsolete line folding"
	}
	if test.kind == Request {
		firstSpace := bytes.IndexByte(startLine, ' ')
		lastSpace := bytes.LastIndexByte(startLine, ' ')
		if firstSpace >= 0 && lastSpace > firstSpace {
			for _, b := range startLine[firstSpace+1 : lastSpace] {
				if b >= 0x80 {
					return "raw non-ASCII request-target accepted by picohttpparser"
				}
			}
		}
	}
	if test.kind == Response {
		parts := bytes.SplitN(startLine, []byte(" "), 3)
		if len(parts) == 2 && len(parts[1]) == 3 {
			return "picohttpparser accepts a status-line without the RFC-required SP after status-code"
		}
	}
	return ""
}

func hasBareLF(input []byte) bool {
	for index, b := range input {
		if b == '\n' && (index == 0 || input[index-1] != '\r') {
			return true
		}
	}
	return false
}

func runIndependentCorpusCase(t *testing.T, test independentCorpusCase) {
	t.Helper()
	for split := 0; split <= len(test.input); split++ {
		parser := NewParser(test.kind, nil, test.limits)
		first, code := parser.Parse(test.input[:split])
		consumed := first
		if code == CodeNone {
			second, nextCode := parser.Parse(test.input[split:])
			consumed += second
			code = nextCode
		}
		switch test.expectation {
		case corpusAccept:
			if code != CodeNone && code != CodeUpgrade {
				t.Fatalf("split %d rejected accepted fixture at %d/%d: %v", split, consumed, len(test.input), code)
			}
			if code == CodeNone && consumed != len(test.input) {
				t.Fatalf("split %d consumed %d/%d accepted bytes", split, consumed, len(test.input))
			}
		case corpusReject:
			if code == CodeNone || code == CodeUpgrade {
				t.Fatalf("split %d accepted rejected fixture after %d/%d bytes", split, consumed, len(test.input))
			}
		case corpusIncomplete:
			if code != CodeNone || consumed != len(test.input) {
				t.Fatalf("split %d incomplete fixture parse = %d/%d, %v", split, consumed, len(test.input), code)
			}
			if finish := parser.Finish(); finish != CodeUnexpectedEOF {
				t.Fatalf("split %d incomplete fixture Finish = %v", split, finish)
			}
		default:
			t.Fatalf("unknown corpus expectation %d", test.expectation)
		}
	}
}

func verifyCorpusPin(t *testing.T, name, checkout string) {
	t.Helper()
	lock, err := os.ReadFile(filepath.Join("..", "testdata", "corpora.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var expected string
	for _, line := range strings.Split(string(lock), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 3 && fields[0] == name {
			expected = fields[2]
			break
		}
	}
	if expected == "" {
		t.Fatalf("%s revision missing from testdata/corpora.lock", name)
	}
	head, err := os.ReadFile(filepath.Join(checkout, ".git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	actual := strings.TrimSpace(string(head))
	if strings.HasPrefix(actual, "ref: ") {
		ref, err := os.ReadFile(filepath.Join(checkout, ".git", strings.TrimPrefix(actual, "ref: ")))
		if err != nil {
			t.Fatal(err)
		}
		actual = strings.TrimSpace(string(ref))
	}
	if actual != expected {
		t.Fatalf("%s corpus revision = %s, want pinned %s", name, actual, expected)
	}
}

type sourceCall struct {
	args []string
	line int
}

func extractRustMacroCalls(source, name string) ([]sourceCall, error) {
	needle := name + "!"
	var calls []sourceCall
	for offset := 0; offset < len(source); {
		relative := strings.Index(source[offset:], needle)
		if relative < 0 {
			break
		}
		start := offset + relative
		if start > 0 && isSourceIdent(source[start-1]) {
			offset = start + len(needle)
			continue
		}
		open := start + len(needle)
		for open < len(source) && (source[open] == ' ' || source[open] == '\t' || source[open] == '\r' || source[open] == '\n') {
			open++
		}
		if open == len(source) || source[open] != '{' && source[open] != '(' {
			offset = open
			continue
		}
		closing := byte('}')
		if source[open] == '(' {
			closing = ')'
		}
		end, err := matchingSourceDelimiter(source, open, source[open], closing)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", 1+strings.Count(source[:start], "\n"), err)
		}
		args, err := splitSourceArguments(source[open+1 : end])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", 1+strings.Count(source[:start], "\n"), err)
		}
		calls = append(calls, sourceCall{args: args, line: 1 + strings.Count(source[:start], "\n")})
		offset = end + 1
	}
	return calls, nil
}

func resolveRustByteExpression(source, expression string) ([]byte, error) {
	expression = strings.TrimSpace(expression)
	if strings.HasPrefix(expression, `b"`) {
		return decodeRustByteString(expression)
	}
	if expression == "" || !isSourceIdent(expression[0]) {
		return nil, fmt.Errorf("unsupported Rust byte expression %q", expression)
	}
	for _, b := range expression {
		if !isSourceIdent(byte(b)) {
			return nil, fmt.Errorf("unsupported Rust byte expression %q", expression)
		}
	}
	for offset := 0; offset < len(source); {
		relative := strings.Index(source[offset:], expression)
		if relative < 0 {
			break
		}
		start := offset + relative
		endName := start + len(expression)
		if (start == 0 || !isSourceIdent(source[start-1])) && (endName == len(source) || !isSourceIdent(source[endName])) {
			equals := strings.IndexByte(source[endName:], '=')
			semicolon := strings.IndexByte(source[endName:], ';')
			if equals >= 0 && semicolon >= 0 && equals < semicolon {
				value := strings.TrimSpace(source[endName+equals+1 : endName+semicolon])
				if strings.HasPrefix(value, `b"`) {
					return decodeRustByteString(value)
				}
			}
		}
		offset = endName
	}
	return nil, fmt.Errorf("Rust byte constant %q not found", expression)
}

func decodeRustByteString(expression string) ([]byte, error) {
	expression = strings.TrimSpace(expression)
	if len(expression) < 3 || expression[0] != 'b' || expression[1] != '"' {
		return nil, fmt.Errorf("unsupported Rust byte string %q", expression)
	}
	var output []byte
	for offset := 2; offset < len(expression); {
		b := expression[offset]
		offset++
		if b == '"' {
			if strings.TrimSpace(expression[offset:]) != "" {
				return nil, fmt.Errorf("trailing Rust byte expression %q", expression[offset:])
			}
			return output, nil
		}
		if b != '\\' {
			output = append(output, b)
			continue
		}
		if offset == len(expression) {
			return nil, fmt.Errorf("trailing Rust escape")
		}
		escape := expression[offset]
		offset++
		switch escape {
		case '0':
			output = append(output, 0)
		case 'n':
			output = append(output, '\n')
		case 'r':
			output = append(output, '\r')
		case 't':
			output = append(output, '\t')
		case '\\', '\'', '"':
			output = append(output, escape)
		case 'x':
			if offset+2 > len(expression) || !isHex(expression[offset]) || !isHex(expression[offset+1]) {
				return nil, fmt.Errorf("invalid Rust hexadecimal escape")
			}
			value, err := strconv.ParseUint(expression[offset:offset+2], 16, 8)
			if err != nil {
				return nil, err
			}
			output = append(output, byte(value))
			offset += 2
		default:
			return nil, fmt.Errorf("unsupported Rust escape \\%c", escape)
		}
	}
	return nil, fmt.Errorf("unterminated Rust byte string")
}

func stripSourceComments(source string) string {
	output := []byte(source)
	quote := byte(0)
	escaped := false
	for index := 0; index < len(output); index++ {
		b := output[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == quote {
				quote = 0
			}
			continue
		}
		if b == '"' || b == '\'' {
			quote = b
			continue
		}
		if b == '/' && index+1 < len(output) && output[index+1] == '/' {
			for index < len(output) && output[index] != '\n' {
				output[index] = ' '
				index++
			}
			continue
		}
		if b == '/' && index+1 < len(output) && output[index+1] == '*' {
			output[index], output[index+1] = ' ', ' '
			index += 2
			for index+1 < len(output) && !(output[index] == '*' && output[index+1] == '/') {
				if output[index] != '\n' {
					output[index] = ' '
				}
				index++
			}
			if index+1 < len(output) {
				output[index], output[index+1] = ' ', ' '
				index++
			}
		}
	}
	return string(output)
}

func extractSourceCalls(source, name string) ([]sourceCall, error) {
	needle := name + "("
	var calls []sourceCall
	for offset := 0; offset < len(source); {
		relative := strings.Index(source[offset:], needle)
		if relative < 0 {
			break
		}
		start := offset + relative
		if start > 0 && (isSourceIdent(source[start-1]) || source[start-1] == '#') {
			offset = start + len(needle)
			continue
		}
		lineStart := strings.LastIndexByte(source[:start], '\n') + 1
		if strings.Contains(source[lineStart:start], "#define") {
			offset = start + len(needle)
			continue
		}
		end, err := matchingSourceDelimiter(source, start+len(name), '(', ')')
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", 1+strings.Count(source[:start], "\n"), err)
		}
		args, err := splitSourceArguments(source[start+len(needle) : end])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", 1+strings.Count(source[:start], "\n"), err)
		}
		calls = append(calls, sourceCall{args: args, line: 1 + strings.Count(source[:start], "\n")})
		offset = end + 1
	}
	return calls, nil
}

func matchingSourceDelimiter(source string, open int, opening, closing byte) (int, error) {
	depth := 0
	quote := byte(0)
	escaped := false
	for index := open; index < len(source); index++ {
		b := source[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == quote {
				quote = 0
			}
			continue
		}
		if b == '"' || b == '\'' {
			quote = b
			continue
		}
		switch b {
		case opening:
			depth++
		case closing:
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated %c delimiter", opening)
}

func splitSourceArguments(source string) ([]string, error) {
	var args []string
	start := 0
	paren, brace, bracket := 0, 0, 0
	quote := byte(0)
	escaped := false
	for index := 0; index < len(source); index++ {
		b := source[index]
		if quote != 0 {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == quote {
				quote = 0
			}
			continue
		}
		if b == '"' || b == '\'' {
			quote = b
			continue
		}
		switch b {
		case '(':
			paren++
		case ')':
			paren--
		case '{':
			brace++
		case '}':
			brace--
		case '[':
			bracket++
		case ']':
			bracket--
		case ',':
			if paren == 0 && brace == 0 && bracket == 0 {
				args = append(args, strings.TrimSpace(source[start:index]))
				start = index + 1
			}
		}
		if paren < 0 || brace < 0 || bracket < 0 {
			return nil, fmt.Errorf("unbalanced source expression")
		}
	}
	if quote != 0 || paren != 0 || brace != 0 || bracket != 0 {
		return nil, fmt.Errorf("unterminated source expression")
	}
	args = append(args, strings.TrimSpace(source[start:]))
	return args, nil
}

func decodeCStringExpression(expression string) ([]byte, error) {
	expression = strings.TrimSpace(expression)
	var output []byte
	for offset := 0; offset < len(expression); {
		for offset < len(expression) && (expression[offset] == ' ' || expression[offset] == '\t' || expression[offset] == '\r' || expression[offset] == '\n') {
			offset++
		}
		if offset == len(expression) {
			break
		}
		if expression[offset] != '"' {
			return nil, fmt.Errorf("unsupported C string expression %q", expression)
		}
		offset++
		closed := false
		for offset < len(expression) {
			b := expression[offset]
			offset++
			if b == '"' {
				closed = true
				break
			}
			if b != '\\' {
				output = append(output, b)
				continue
			}
			if offset == len(expression) {
				return nil, fmt.Errorf("trailing C escape")
			}
			escape := expression[offset]
			offset++
			switch escape {
			case 'a':
				output = append(output, '\a')
			case 'b':
				output = append(output, '\b')
			case 'f':
				output = append(output, '\f')
			case 'n':
				output = append(output, '\n')
			case 'r':
				output = append(output, '\r')
			case 't':
				output = append(output, '\t')
			case 'v':
				output = append(output, '\v')
			case '\\', '\'', '"', '?':
				output = append(output, escape)
			case 'x':
				start := offset
				for offset < len(expression) && isHex(expression[offset]) {
					offset++
				}
				if start == offset {
					return nil, fmt.Errorf("empty C hexadecimal escape")
				}
				value, err := strconv.ParseUint(expression[start:offset], 16, 8)
				if err != nil {
					return nil, fmt.Errorf("C hexadecimal escape: %w", err)
				}
				output = append(output, byte(value))
			default:
				if escape < '0' || escape > '7' {
					return nil, fmt.Errorf("unsupported C escape \\%c", escape)
				}
				start := offset - 1
				for offset < len(expression) && offset < start+3 && expression[offset] >= '0' && expression[offset] <= '7' {
					offset++
				}
				value, err := strconv.ParseUint(expression[start:offset], 8, 8)
				if err != nil {
					return nil, fmt.Errorf("C octal escape: %w", err)
				}
				output = append(output, byte(value))
			}
		}
		if !closed {
			return nil, fmt.Errorf("unterminated C string")
		}
	}
	if len(output) == 0 && expression != `""` {
		return nil, fmt.Errorf("no C string literals in %q", expression)
	}
	return output, nil
}

func isSourceIdent(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}
