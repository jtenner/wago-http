package http_test

import (
	"errors"
	"testing"

	wagohttp "github.com/wago-org/http"
	http1 "github.com/wago-org/http/http"
	"github.com/wago-org/http/http2"
	"github.com/wago-org/http/http3"
	"github.com/wago-org/http/websocket"
	wagonet "github.com/wago-org/net"
	wago "github.com/wago-org/wago"
)

type registration func(*wagohttp.Network) error

func TestSelectiveRegistrationSurfaces(t *testing.T) {
	tests := []struct {
		name         string
		register     []registration
		capabilities []wago.Capability
		imports      map[string]int
	}{
		{name: "none", imports: map[string]int{}},
		{
			name: "http1", register: []registration{http1.Register},
			capabilities: []wago.Capability{wagonet.CapInfo, wagonet.CapTCP, wagohttp.CapInfo, http1.CapHTTP},
			imports:      map[string]int{wagonet.Module: 1, wagonet.TCPModule: 11, wagohttp.Module: 1, http1.Module: 2},
		},
		{
			name: "http2", register: []registration{http2.Register},
			capabilities: []wago.Capability{wagonet.CapInfo, wagonet.CapTCP, wagohttp.CapInfo, http2.CapHTTP2},
			imports:      map[string]int{wagonet.Module: 1, wagonet.TCPModule: 11, wagohttp.Module: 1, http2.Module: 2},
		},
		{
			name: "websocket", register: []registration{websocket.Register},
			capabilities: []wago.Capability{wagonet.CapInfo, wagonet.CapTCP, wagohttp.CapInfo, websocket.CapWebSocket},
			imports:      map[string]int{wagonet.Module: 1, wagonet.TCPModule: 11, wagohttp.Module: 1, websocket.Module: 2},
		},
		{
			name: "http3", register: []registration{http3.Register},
			capabilities: []wago.Capability{wagonet.CapInfo, wagonet.CapUDP, wagohttp.CapInfo, http3.CapHTTP3},
			imports:      map[string]int{wagonet.Module: 1, wagonet.UDPModule: 6, wagohttp.Module: 1, http3.Module: 2},
		},
		{
			name: "all", register: []registration{http1.Register, http2.Register, websocket.Register, http3.Register},
			capabilities: []wago.Capability{
				wagonet.CapInfo, wagonet.CapTCP, wagonet.CapUDP,
				wagohttp.CapInfo, http1.CapHTTP, http2.CapHTTP2, websocket.CapWebSocket, http3.CapHTTP3,
			},
			imports: map[string]int{
				wagonet.Module: 1, wagonet.TCPModule: 11, wagonet.UDPModule: 6,
				wagohttp.Module: 1, http1.Module: 2, http2.Module: 2, websocket.Module: 2, http3.Module: 2,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			network := wagohttp.New()
			for _, register := range test.register {
				if err := register(network); err != nil {
					t.Fatalf("register: %v", err)
				}
			}
			runtime := wago.NewRuntime()
			if err := runtime.Use(network); err != nil {
				t.Fatalf("Use: %v", err)
			}
			assertCapabilitySet(t, runtime.Capabilities(), test.capabilities)
			gotImports := make(map[string]int)
			for _, spec := range runtime.ProvidedImports() {
				gotImports[spec.Module]++
			}
			if !equalCounts(gotImports, test.imports) {
				t.Fatalf("import modules = %v, want %v", gotImports, test.imports)
			}
		})
	}
}

func TestTransportSelectionIsOrderIndependentAndDeduplicated(t *testing.T) {
	network := wagohttp.New()
	for _, register := range []registration{websocket.Register, http2.Register, http1.Register} {
		if err := register(network); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	runtime := wago.NewRuntime()
	if err := runtime.Use(network); err != nil {
		t.Fatalf("Use: %v", err)
	}
	counts := make(map[string]int)
	for _, spec := range runtime.ProvidedImports() {
		counts[spec.Module]++
	}
	if counts[wagonet.TCPModule] != 11 {
		t.Fatalf("TCP import count = %d, want 11", counts[wagonet.TCPModule])
	}
	if counts[wagonet.UDPModule] != 0 {
		t.Fatalf("UDP import count = %d, want 0", counts[wagonet.UDPModule])
	}
}

func TestDuplicateAndFrozenRegistrationFail(t *testing.T) {
	network := wagohttp.New()
	if err := http1.Register(network); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := http1.Register(network); !errors.Is(err, wagohttp.ErrProtocolAlreadyRegistered) {
		t.Fatalf("duplicate error = %v", err)
	}
	runtime := wago.NewRuntime()
	if err := runtime.Use(network); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if err := http2.Register(network); !errors.Is(err, wagohttp.ErrProtocolRegistrationFrozen) {
		t.Fatalf("frozen error = %v", err)
	}
}

func TestExtensionInfoIsDefensivelyCopied(t *testing.T) {
	network := wagohttp.New()
	first := network.Info()
	first.Authors[0] = "changed"
	first.Tags[0] = "changed"
	first.Compat.Engines["wago"] = "changed"
	second := network.Info()
	if second.Authors[0] == "changed" || second.Tags[0] == "changed" || second.Compat.Engines["wago"] == "changed" {
		t.Fatal("Info returned aliased metadata")
	}
}

func assertCapabilitySet(t *testing.T, got, want []wago.Capability) {
	t.Helper()
	gotSet := make(map[wago.Capability]int, len(got))
	for _, capability := range got {
		gotSet[capability]++
	}
	wantSet := make(map[wago.Capability]int, len(want))
	for _, capability := range want {
		wantSet[capability]++
	}
	if !equalCapabilityCounts(gotSet, wantSet) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func equalCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func equalCapabilityCounts(left, right map[wago.Capability]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
