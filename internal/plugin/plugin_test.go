package plugin

import (
	"errors"
	"testing"

	wagonet "github.com/wago-org/net"
	wago "github.com/wago-org/wago"
)

func testHostFunc(_ wago.HostModule, _, _ []uint64) {}
func testTransport(*wagonet.Network) error          { return nil }

func testModule() Module {
	return NewModule(
		KeyHTTP1,
		"wago_test",
		wago.Capability("test.http"),
		"test capability",
		NewTransport(TransportTCP, testTransport),
		Binding{Name: "test", Func: testHostFunc},
	)
}

func TestSetRejectsInvalidDuplicateAndFrozenModules(t *testing.T) {
	var set Set
	if err := set.Add(Module{}); !errors.Is(err, ErrInvalidModule) {
		t.Fatalf("invalid error = %v", err)
	}
	module := testModule()
	if err := set.Add(module); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := set.Add(module); !errors.Is(err, ErrDuplicateModule) {
		t.Fatalf("duplicate error = %v", err)
	}
	frozen := set.Freeze()
	if len(frozen) != 1 || frozen[0].Transports() != TransportTCP {
		t.Fatalf("frozen modules = %#v", frozen)
	}
	if err := set.Add(NewModule(KeyHTTP2, "wago_test2", wago.Capability("test.http2"), "test", NewTransport(TransportTCP, testTransport), Binding{Name: "test", Func: testHostFunc})); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen error = %v", err)
	}
}

func TestNewModuleCopiesBindingSlice(t *testing.T) {
	bindings := []Binding{{Name: "first", Func: testHostFunc}}
	module := NewModule(KeyHTTP1, "wago_test", wago.Capability("test.http"), "test", NewTransport(TransportTCP, testTransport), bindings...)
	bindings[0].Name = "changed"
	if module.bindings[0].Name != "first" {
		t.Fatal("module changed through caller-owned binding slice")
	}
}
