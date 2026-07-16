// Package plugin provides the protocol-neutral composition contract shared by
// the root extension and its independently selectable protocol packages.
package plugin

import (
	"errors"
	"sync"

	wagonet "github.com/wago-org/net"
	wago "github.com/wago-org/wago"
)

// Key is the stable identity used to reject duplicate protocol selection.
type Key string

const (
	KeyHTTP1     Key = "http1"
	KeyHTTP2     Key = "http2"
	KeyWebSocket Key = "websocket"
	KeyHTTP3     Key = "http3"
)

// Transport is a bitset of wago-org/net transports required by a protocol.
type Transport uint8

const (
	TransportTCP Transport = 1 << iota
	TransportUDP
)

var (
	ErrInvalidModule   = errors.New("wagohttp: invalid protocol module")
	ErrDuplicateModule = errors.New("wagohttp: protocol module already registered")
	ErrFrozen          = errors.New("wagohttp: protocol registration is frozen")
)

// TransportRegistration is one opaque protocol-local net transport selector.
// Keeping the callback on the protocol descriptor prevents the root package
// from compiling every transport adapter.
type TransportRegistration struct {
	transport Transport
	register  func(*wagonet.Network) error
}

// NewTransport constructs one trusted transport contribution.
func NewTransport(transport Transport, register func(*wagonet.Network) error) TransportRegistration {
	return TransportRegistration{transport: transport, register: register}
}

func (r TransportRegistration) valid() bool {
	return (r.transport == TransportTCP || r.transport == TransportUDP) && r.register != nil
}

// Binding is one checked, fixed-shape guest import contributed by a protocol.
type Binding struct {
	Name    string
	Func    wago.HostFunc
	Params  []wago.ValType
	Results []wago.ValType
	Docs    string
}

// Module is an opaque protocol registration descriptor.
type Module struct {
	key        Key
	module     string
	capability wago.Capability
	docs       string
	transport  TransportRegistration
	bindings   []Binding
}

// NewModule constructs a trusted descriptor for one protocol package.
func NewModule(key Key, module string, capability wago.Capability, docs string, transport TransportRegistration, bindings ...Binding) Module {
	return Module{
		key:        key,
		module:     module,
		capability: capability,
		docs:       docs,
		transport:  transport,
		bindings:   append([]Binding(nil), bindings...),
	}
}

func (m Module) valid() bool {
	if m.key == "" || m.module == "" || m.capability == "" || !m.transport.valid() || len(m.bindings) == 0 {
		return false
	}
	for _, binding := range m.bindings {
		if binding.Name == "" || binding.Func == nil {
			return false
		}
	}
	return true
}

// Transports reports the network transports required by this descriptor.
func (m Module) Transports() Transport { return m.transport.transport }

// RegisterTransport installs this descriptor's network transport unless an
// earlier module already selected the same transport.
func (m Module) RegisterTransport(network *wagonet.Network, selected *Transport) error {
	if selected == nil || network == nil || !m.transport.valid() {
		return ErrInvalidModule
	}
	if *selected&m.transport.transport != 0 {
		return nil
	}
	if err := m.transport.register(network); err != nil {
		return err
	}
	*selected |= m.transport.transport
	return nil
}

// Install contributes exactly this protocol's capability and import module.
func (m Module) Install(registry *wago.Registry) {
	registry.Capability(m.capability, wago.CapabilityDocs(m.docs))
	imports := registry.ImportModule(m.module)
	for _, binding := range m.bindings {
		imports.Func(binding.Name, binding.Func).
			Params(binding.Params...).
			Results(binding.Results...).
			Capability(m.capability).
			Docs(binding.Docs)
	}
}

// Set records selected modules until registration freezes.
type Set struct {
	mu      sync.Mutex
	frozen  bool
	modules []Module
}

// Add selects one module. The expected protocol count is deliberately small,
// so linear duplicate detection avoids a retained map allocation.
func (s *Set) Add(module Module) error {
	if !module.valid() {
		return ErrInvalidModule
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return ErrFrozen
	}
	for _, existing := range s.modules {
		if existing.key == module.key {
			return ErrDuplicateModule
		}
	}
	s.modules = append(s.modules, module)
	return nil
}

// Freeze prevents further mutation and returns an immutable snapshot.
func (s *Set) Freeze() []Module {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	return append([]Module(nil), s.modules...)
}
