package http

import "testing"

func TestParseOneStopsAtMessageBoundary(t *testing.T) {
	const first = "HTTP/1.1 103 Early Hints\r\nLink: </style.css>; rel=preload\r\n\r\n"
	const second = "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata"
	input := []byte(first + second + "tail")
	parser := NewParser(Response, nil, Limits{})

	consumed, complete, code := parser.ParseOne(input)
	if code != CodeNone || !complete || consumed != len(first) {
		t.Fatalf("first ParseOne = (%d, %v, %v), want (%d, true, ok)", consumed, complete, code, len(first))
	}
	consumed2, complete, code := parser.ParseOne(input[consumed:])
	if code != CodeNone || !complete || consumed2 != len(second) {
		t.Fatalf("second ParseOne = (%d, %v, %v), want (%d, true, ok)", consumed2, complete, code, len(second))
	}
}

func TestParseOneFragmentedMessage(t *testing.T) {
	input := []byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	parser := NewParser(Request, nil, Limits{})
	for i, b := range input {
		consumed, complete, code := parser.ParseOne([]byte{b})
		if code != CodeNone || consumed != 1 {
			t.Fatalf("byte %d ParseOne = (%d, %v, %v)", i, consumed, complete, code)
		}
		if complete != (i == len(input)-1) {
			t.Fatalf("byte %d complete = %v", i, complete)
		}
	}
}

func TestParseOneUpgradeLeavesFollowingBytes(t *testing.T) {
	const response = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	input := []byte(response + "frames")
	parser := NewParser(Response, nil, Limits{})

	consumed, complete, code := parser.ParseOne(input)
	if code != CodeUpgrade || !complete || consumed != len(response) {
		t.Fatalf("ParseOne = (%d, %v, %v), want (%d, true, upgrade)", consumed, complete, code, len(response))
	}
}

func TestParseOneZeroAllocations(t *testing.T) {
	input := []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata")
	if allocations := testing.AllocsPerRun(1000, func() {
		parser := NewParser(Response, nil, Limits{})
		consumed, complete, code := parser.ParseOne(input)
		if code != CodeNone || !complete || consumed != len(input) {
			panic("unexpected ParseOne result")
		}
	}); allocations != 0 {
		t.Fatalf("ParseOne allocations = %v, want 0", allocations)
	}
}

func TestParseStillConsumesPipeline(t *testing.T) {
	const message = "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"
	parser := NewParser(Request, nil, Limits{})
	input := []byte(message + message)
	consumed, code := parser.Parse(input)
	if code != CodeNone || consumed != len(input) || parser.MessageNumber() != 2 {
		t.Fatalf("Parse = (%d, %v), messages=%d", consumed, code, parser.MessageNumber())
	}
}
