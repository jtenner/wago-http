package http

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLLHTTPCorpus validates RFC-aligned cases from the exact llhttp revision
// pinned in testdata/corpora.lock. The upstream checkout is intentionally
// optional so normal source distributions do not vendor third-party trees.
func TestLLHTTPCorpus(t *testing.T) {
	root := filepath.Join("..", "testdata", "upstream", "llhttp", "test")
	if _, err := os.Stat(root); err != nil {
		t.Skip("pinned llhttp corpus not fetched; run scripts/fetch-test-corpora.sh")
	}
	verifyLLHTTPPin(t, root)

	files := []string{
		"request/sample.md",
		"request/method.md",
		"request/uri.md",
		"request/content-length.md",
		"request/transfer-encoding.md",
		"request/invalid.md",
		"request/pipelining.md",
		"response/sample.md",
		"response/content-length.md",
		"response/transfer-encoding.md",
		"response/invalid.md",
		"response/pipelining.md",
	}

	var tested, skipped int
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		cases, err := parseLLHTTPMarkdown(string(data), name)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range cases {
			if reason := llhttpRFC9112Skip(test); reason != "" {
				skipped++
				continue
			}
			tested++
			t.Run(test.location, func(t *testing.T) {
				input := test.input
				if test.parseBytes > 0 && test.parseBytes < len(input) {
					input = input[:test.parseBytes]
				}
				for split := 0; split <= len(input); split++ {
					completed := 0
					callbacks := Callbacks{MessageComplete: func(Message) { completed++ }}
					parser := NewParser(test.kind, &callbacks, Limits{MaxBodyBytes: 1 << 30})
					first, code := parser.Parse(input[:split])
					consumed := first
					if code == CodeNone {
						second, nextCode := parser.Parse(input[split:])
						consumed += second
						code = nextCode
					}
					if test.wantError {
						if code == CodeNone {
							code = parser.Finish()
						}
						if code == CodeNone || code == CodeUpgrade {
							t.Fatalf("split %d accepted llhttp rejection after consuming %d/%d bytes", split, consumed, len(input))
						}
						continue
					}
					if code != CodeNone && code != CodeUpgrade {
						t.Fatalf("split %d rejected llhttp acceptance at %d/%d: %v", split, consumed, len(input), code)
					}
					if code == CodeNone && consumed != len(input) {
						t.Fatalf("split %d successful parse consumed %d/%d bytes", split, consumed, len(input))
					}
					if test.complete > 0 && completed != test.complete {
						t.Fatalf("split %d completed %d messages, llhttp completed %d", split, completed, test.complete)
					}
				}
			})
		}
	}
	if tested < 100 {
		t.Fatalf("only %d corpus cases tested (%d classified RFC deltas)", tested, skipped)
	}
	t.Logf("validated %d pinned llhttp cases; skipped %d documented RFC/ABI deltas", tested, skipped)
}

func verifyLLHTTPPin(t *testing.T, testRoot string) {
	t.Helper()
	lock, err := os.ReadFile(filepath.Join("..", "testdata", "corpora.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var expected string
	for _, line := range strings.Split(string(lock), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) >= 3 && fields[0] == "llhttp" {
			expected = fields[2]
			break
		}
	}
	if expected == "" {
		t.Fatal("llhttp revision missing from testdata/corpora.lock")
	}
	gitDir := filepath.Join(filepath.Dir(testRoot), ".git")
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	actual := strings.TrimSpace(string(head))
	if strings.HasPrefix(actual, "ref: ") {
		ref, err := os.ReadFile(filepath.Join(gitDir, strings.TrimPrefix(actual, "ref: ")))
		if err != nil {
			t.Fatal(err)
		}
		actual = strings.TrimSpace(string(ref))
	}
	if actual != expected {
		t.Fatalf("llhttp corpus revision = %s, want pinned %s", actual, expected)
	}
}

type llhttpCase struct {
	location   string
	kind       Kind
	input      []byte
	log        string
	wantError  bool
	complete   int
	parseBytes int
}

func parseLLHTTPMarkdown(source, file string) ([]llhttpCase, error) {
	lines := strings.Split(source, "\n")
	var cases []llhttpCase
	heading := "unnamed"
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if strings.HasPrefix(line, "#") {
			heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		if !strings.HasPrefix(line, "<!-- meta=") {
			continue
		}
		meta := line
		if strings.Contains(meta, `"skipBody": true`) {
			continue
		}
		kind := Kind(0)
		switch {
		case strings.Contains(meta, `"type": "request"`):
			kind = Request
		case strings.Contains(meta, `"type": "response"`):
			kind = Response
		default:
			continue
		}
		input, next, ok := fencedBlock(lines, index+1, "```http")
		if !ok {
			return nil, fmt.Errorf("%s:%d: missing http block", file, index+1)
		}
		log, next, ok := fencedBlock(lines, next, "```log")
		if !ok {
			return nil, fmt.Errorf("%s:%d: missing log block", file, index+1)
		}
		normalized, ok := normalizeLLHTTPInput(input)
		if !ok {
			index = next
			continue
		}
		parseBytes := 0
		if strings.Contains(log, "Pause on CONNECT/Upgrade") {
			parseBytes = firstMessageCompleteOffset(log) + loggedContentLength(log)
		}
		cases = append(cases, llhttpCase{
			location: fmt.Sprintf("%s:%d/%s", strings.TrimSuffix(file, ".md"), index+1, sanitizeTestName(heading)),
			kind:     kind,
			input:    normalized,
			log:      log,
			wantError: strings.Contains(log, " error code=") &&
				!strings.Contains(log, "Pause on CONNECT/Upgrade"),
			complete: strings.Count(log, " message complete"), parseBytes: parseBytes,
		})
		index = next
	}
	return cases, nil
}

func fencedBlock(lines []string, start int, fence string) (string, int, bool) {
	for start < len(lines) && lines[start] != fence {
		start++
	}
	if start == len(lines) {
		return "", start, false
	}
	start++
	end := start
	for end < len(lines) && lines[end] != "```" {
		end++
	}
	if end == len(lines) {
		return "", end, false
	}
	return strings.Join(lines[start:end], "\n") + "\n", end + 1, true
}

func normalizeLLHTTPInput(input string) ([]byte, bool) {
	input = strings.TrimSuffix(input, "\n")
	input = strings.ReplaceAll(input, "\\\n", "")
	input = strings.ReplaceAll(input, "\n", "\r\n")
	input = strings.ReplaceAll(input, `\r`, "\r")
	input = strings.ReplaceAll(input, `\n`, "\n")
	input = strings.ReplaceAll(input, `\t`, "\t")
	input = strings.ReplaceAll(input, `\f`, "\f")
	if strings.Contains(input, "${") {
		return nil, false
	}
	var output []byte
	for index := 0; index < len(input); {
		if input[index] != '\\' || index+1 == len(input) {
			output = append(output, input[index])
			index++
			continue
		}
		if input[index+1] == 'x' {
			end := index + 2
			for end < len(input) && isHex(input[end]) {
				end++
			}
			if end == index+2 {
				output = append(output, input[index])
				index++
				continue
			}
			value, err := strconv.ParseUint(input[index+2:end], 16, 8)
			if err != nil {
				return nil, false
			}
			output = append(output, byte(value))
			index = end
			continue
		}
		if input[index+1] >= '0' && input[index+1] <= '7' {
			end := index + 1
			for end < len(input) && end < index+4 && input[end] >= '0' && input[end] <= '7' {
				end++
			}
			value, err := strconv.ParseUint(input[index+1:end], 8, 8)
			if err != nil {
				return nil, false
			}
			output = append(output, byte(value))
			index = end
			continue
		}
		output = append(output, input[index])
		index++
	}
	return output, true
}

func firstMessageCompleteOffset(log string) int {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, " message complete") || !strings.HasPrefix(line, "off=") {
			continue
		}
		end := strings.IndexByte(line, ' ')
		if end < 4 {
			return 0
		}
		offset, err := strconv.Atoi(line[4:end])
		if err == nil {
			return offset
		}
	}
	return 0
}

func loggedContentLength(log string) int {
	const marker = " content_length="
	for _, line := range strings.Split(log, "\n") {
		at := strings.Index(line, marker)
		if at < 0 {
			continue
		}
		value := line[at+len(marker):]
		if end := strings.IndexByte(value, ' '); end >= 0 {
			value = value[:end]
		}
		length, err := strconv.Atoi(value)
		if err == nil {
			return length
		}
	}
	return 0
}

func llhttpRFC9112Skip(test llhttpCase) string {
	if test.wantError && (strings.Contains(test.log, "Invalid method for HTTP") || strings.Contains(test.log, "Expected space after method")) {
		return "llhttp method whitelist rejects valid extension methods"
	}
	if test.wantError {
		return ""
	}
	lower := bytes.ToLower(test.input)
	if test.kind == Response && (!bytes.HasPrefix(test.input, []byte("HTTP/")) || bytes.Contains(test.input, []byte("ICY "))) {
		return "legacy non-HTTP response protocol"
	}
	if test.kind == Response && test.complete > 1 {
		return "llhttp tolerates separator CRLF before pipelined responses"
	}
	firstLineEnd := bytes.Index(test.input, []byte("\r\n"))
	if firstLineEnd < 0 {
		return "incomplete start line fixture"
	}
	firstLine := test.input[:firstLineEnd]
	if !bytes.Contains(firstLine, []byte("HTTP/1.0")) && !bytes.Contains(firstLine, []byte("HTTP/1.1")) {
		return "legacy or non-HTTP version"
	}
	if test.kind == Request && bytes.Contains(firstLine, []byte("HTTP/1.1")) && !bytes.Contains(lower, []byte("\r\nhost:")) {
		return "llhttp does not enforce RFC 9112 Host requirement"
	}
	if bytes.Contains(test.input, []byte("\r\n ")) || bytes.Contains(test.input, []byte("\r\n\t")) {
		return "obsolete line folding"
	}
	if bytes.Contains(lower, []byte("transfer-encoding: identity")) {
		return "obsolete identity transfer coding"
	}
	return ""
}

func sanitizeTestName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "/", "-")
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}
