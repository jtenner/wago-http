package http2

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestHeaderDecoderRFC7541RequestExamplesEverySplit(t *testing.T) {
	// RFC 7541 Appendix C.3: request examples without Huffman coding. Dynamic
	// table state is intentionally shared by the three blocks.
	blocks := []struct {
		wire   string
		fields []HeaderField
	}{
		{
			"828684410f7777772e6578616d706c652e636f6d",
			[]HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "www.example.com"}},
		},
		{
			"828684be58086e6f2d6361636865",
			[]HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":path", Value: "/"}, {Name: ":authority", Value: "www.example.com"}, {Name: "cache-control", Value: "no-cache"}},
		},
		{
			"828785bf400a637573746f6d2d6b65790c637573746f6d2d76616c7565",
			[]HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "https"}, {Name: ":path", Value: "/index.html"}, {Name: ":authority", Value: "www.example.com"}, {Name: "custom-key", Value: "custom-value"}},
		},
	}

	for splitPattern := 0; splitPattern < 32; splitPattern++ {
		var got []HeaderField
		decoder := NewHeaderDecoder(HeaderLimits{}, func(field HeaderField) { got = append(got, field) })
		for blockIndex, block := range blocks {
			wire, err := hex.DecodeString(block.wire)
			if err != nil {
				t.Fatal(err)
			}
			got = got[:0]
			if err := decoder.BeginBlock(); err != nil {
				t.Fatal(err)
			}
			for offset := 0; offset < len(wire); {
				size := 1 + (offset+splitPattern)%7
				if size > len(wire)-offset {
					size = len(wire) - offset
				}
				n, err := decoder.Write(wire[offset : offset+size])
				if err != nil || n != size {
					t.Fatalf("pattern %d block %d write = %d, %v", splitPattern, blockIndex, n, err)
				}
				offset += size
			}
			if err := decoder.EndBlock(); err != nil {
				t.Fatalf("pattern %d block %d close: %v", splitPattern, blockIndex, err)
			}
			if !reflect.DeepEqual(got, block.fields) {
				t.Fatalf("pattern %d block %d:\n got %#v\nwant %#v", splitPattern, blockIndex, got, block.fields)
			}
		}
	}
}

func TestHeaderDecoderLimitsAndLifecycle(t *testing.T) {
	decoder := NewHeaderDecoder(HeaderLimits{MaxHeaders: 1, MaxHeaderListBytes: 128, MaxFieldBytes: 16}, nil)
	if _, err := decoder.Write(nil); !errors.Is(err, ErrHeaderDecoderState) {
		t.Fatalf("Write inactive = %v", err)
	}
	if err := decoder.EndBlock(); !errors.Is(err, ErrHeaderDecoderState) {
		t.Fatalf("End inactive = %v", err)
	}
	if err := decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	if err := decoder.BeginBlock(); !errors.Is(err, ErrHeaderDecoderState) {
		t.Fatalf("second Begin = %v", err)
	}
	// Static :method GET followed by static :path / exceeds MaxHeaders.
	if _, err := decoder.Write([]byte{0x82, 0x84}); !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("header count error = %v", err)
	}
	if err := decoder.EndBlock(); !errors.Is(err, ErrTooManyHeaders) {
		t.Fatalf("End after count error = %v", err)
	}

	decoder = NewHeaderDecoder(HeaderLimits{MaxHeaderListBytes: 33}, nil)
	if err := decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Write([]byte{0x82}); !errors.Is(err, ErrHeaderListTooLarge) {
		t.Fatalf("list size error = %v", err)
	}
	_ = decoder.EndBlock()

	decoder = NewHeaderDecoder(HeaderLimits{}, nil)
	if err := decoder.BeginBlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Write([]byte{0x40}); err != nil {
		t.Fatal(err)
	}
	if err := decoder.EndBlock(); err == nil {
		t.Fatal("truncated literal accepted")
	}
	if err := decoder.SetAllowedDynamicTableBytes(1024); err != nil {
		t.Fatal(err)
	}
	if err := decoder.SetAllowedDynamicTableBytes(1 << 20); !errors.Is(err, ErrHeaderDecoderState) {
		t.Fatalf("oversized table = %v", err)
	}
}

func TestHPACKInteroperabilityCorpus(t *testing.T) {
	root := filepath.Join("..", "testdata", "upstream", "hpack-test-case")
	if _, err := os.Stat(root); err != nil {
		t.Skip("pinned hpack-test-case corpus not fetched; run scripts/fetch-test-corpora.sh")
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	if len(paths) < 400 {
		t.Fatalf("found only %d HPACK stories, expected pinned corpus", len(paths))
	}

	strategies := []struct {
		name string
		next func(offset, length int) int
	}{
		{"contiguous", func(offset, length int) int { return length - offset }},
		{"byte-at-a-time", func(_, _ int) int { return 1 }},
		{"prime-pattern", func(offset, length int) int { return 1 + (offset*17+11)%31 }},
	}
	for _, strategy := range strategies {
		t.Run(strategy.name, func(t *testing.T) {
			cases := 0
			for _, path := range paths {
				story := readHPACKStory(t, path)
				var got []HeaderField
				decoder := NewHeaderDecoder(HeaderLimits{
					MaxDynamicTableBytes: 16 << 10,
					MaxFieldBytes:        1 << 20,
					MaxHeaderListBytes:   4 << 20,
					MaxHeaders:           4096,
				}, func(field HeaderField) { got = append(got, field) })
				for _, testCase := range story.Cases {
					// raw-data/ contains unencoded source stories used to produce
					// the implementation corpora; only cases with wire data are
					// decoder vectors.
					if testCase.Wire == "" && len(testCase.Headers) != 0 {
						continue
					}
					cases++
					wire, err := hex.DecodeString(testCase.Wire)
					if err != nil {
						t.Fatalf("%s case %d wire: %v", path, testCase.Seqno, err)
					}
					got = got[:0]
					if err := decoder.BeginBlock(); err != nil {
						t.Fatalf("%s case %d begin: %v", path, testCase.Seqno, err)
					}
					for offset := 0; offset < len(wire); {
						size := strategy.next(offset, len(wire))
						if size > len(wire)-offset {
							size = len(wire) - offset
						}
						n, err := decoder.Write(wire[offset : offset+size])
						if err != nil || n != size {
							t.Fatalf("%s case %d offset %d: wrote %d/%d: %v", path, testCase.Seqno, offset, n, size, err)
						}
						offset += size
					}
					if err := decoder.EndBlock(); err != nil {
						t.Fatalf("%s case %d end: %v", path, testCase.Seqno, err)
					}
					want := flattenHPACKHeaders(t, path, testCase)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s case %d:\n got %#v\nwant %#v", path, testCase.Seqno, got, want)
					}
				}
			}
			if cases < 47000 {
				t.Fatalf("executed %d cases, expected full pinned corpus", cases)
			}
		})
	}
}

type hpackStory struct {
	Cases []hpackCase `json:"cases"`
}

type hpackCase struct {
	Seqno   int                 `json:"seqno"`
	Wire    string              `json:"wire"`
	Headers []map[string]string `json:"headers"`
}

func readHPACKStory(t *testing.T, path string) hpackStory {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var story hpackStory
	if err := json.Unmarshal(data, &story); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return story
}

func flattenHPACKHeaders(t *testing.T, path string, testCase hpackCase) []HeaderField {
	t.Helper()
	fields := make([]HeaderField, 0, len(testCase.Headers))
	for _, object := range testCase.Headers {
		if len(object) != 1 {
			t.Fatalf("%s case %d has non-singleton header object %#v", path, testCase.Seqno, object)
		}
		for name, value := range object {
			fields = append(fields, HeaderField{Name: name, Value: value})
		}
	}
	return fields
}
