// Package http provides the shared Wago HTTP plugin builder and lifecycle
// composition over github.com/wago-org/net.
package http

import (
	"embed"
	"encoding/json"

	"github.com/wago-org/http/internal/plugin"
	wagonet "github.com/wago-org/net"
	wago "github.com/wago-org/wago"
)

const (
	// Module is the shared HTTP-family WebAssembly import module.
	Module = "wago_http"
	// ABIVersion1 encodes shared ABI version 1.0.
	ABIVersion1 uint32 = 0x0001_0000
	// CapInfo permits inspection of the selected HTTP-family ABI.
	CapInfo wago.Capability = "http.info"
)

var (
	ErrInvalidProtocolRegistration = plugin.ErrInvalidModule
	ErrProtocolAlreadyRegistered   = plugin.ErrDuplicateModule
	ErrProtocolRegistrationFrozen  = plugin.ErrFrozen
)

// Extension composes one shared wago-org/net extension with independently
// selected HTTP-family protocol modules.
type Extension struct {
	network *wagonet.Network
	modules plugin.Set
}

// Network is the builder passed to the http, http2, websocket, and http3
// registration packages.
type Network = Extension

// New constructs an empty HTTP-family composition. The options configure the
// single underlying wago-org/net namespace shared by all selected protocols.
func New(options ...wagonet.Option) *Network {
	return &Extension{network: wagonet.New(options...)}
}

// RegisterModule records one trusted protocol descriptor. The internal
// parameter type limits direct use to this repository's protocol packages.
func (e *Extension) RegisterModule(module plugin.Module) error {
	if e == nil || e.network == nil {
		return plugin.ErrInvalidModule
	}
	return e.modules.Add(module)
}

// Info returns aggregate plugin metadata.
func (e *Extension) Info() wago.ExtensionInfo { return cloneExtensionInfo(extensionInfo) }

// Register freezes protocol selection, installs each required net transport
// exactly once, and contributes the selected HTTP capabilities and imports.
func (e *Extension) Register(registry *wago.Registry) error {
	if e == nil || e.network == nil {
		return plugin.ErrInvalidModule
	}
	modules := e.modules.Freeze()
	if len(modules) == 0 {
		return nil
	}

	var transports plugin.Transport
	for _, module := range modules {
		if err := module.RegisterTransport(e.network, &transports); err != nil {
			return err
		}
	}
	if err := e.network.Register(registry); err != nil {
		return err
	}

	registry.Capability(CapInfo, wago.CapabilityDocs("inspect the selected Wago HTTP-family ABI"))
	registry.ImportModule(Module).
		Func("abi_version", abiVersion).
		Results(wago.ValI32).
		Capability(CapInfo).
		Docs("return the supported shared wago_http ABI version")
	for _, module := range modules {
		module.Install(registry)
	}
	return nil
}

func abiVersion(_ wago.HostModule, params, results []uint64) {
	if len(params) != 0 || len(results) != 1 {
		return
	}
	results[0] = uint64(ABIVersion1)
}

type manifest struct {
	Module      string            `json:"module"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Stability   string            `json:"stability"`
	License     string            `json:"license"`
	Homepage    string            `json:"homepage"`
	Repository  string            `json:"repository"`
	Authors     []string          `json:"authors"`
	Keywords    []string          `json:"keywords"`
	Engines     map[string]string `json:"engines"`
	Private     bool              `json:"private"`
}

//go:embed wago.json
var manifestFiles embed.FS

var extensionInfo = loadExtensionInfo()

func loadExtensionInfo() wago.ExtensionInfo {
	data, err := manifestFiles.ReadFile("wago.json")
	if err != nil {
		panic("wagohttp: reading wago.json: " + err.Error())
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		panic("wagohttp: parsing wago.json: " + err.Error())
	}
	return cloneExtensionInfo(wago.ExtensionInfo{
		ID: value.Module, Name: value.Name, Version: value.Version,
		Description: value.Description, Stability: wago.Stability(value.Stability),
		License: value.License, Homepage: value.Homepage, Repository: value.Repository,
		Authors: value.Authors, Tags: value.Keywords, Private: value.Private,
		Compat: wago.Compatibility{Engines: value.Engines},
	})
}

func cloneExtensionInfo(info wago.ExtensionInfo) wago.ExtensionInfo {
	cloned := info
	cloned.Authors = append([]string(nil), info.Authors...)
	cloned.Tags = append([]string(nil), info.Tags...)
	if info.Compat.Engines != nil {
		cloned.Compat.Engines = make(map[string]string, len(info.Compat.Engines))
		for key, value := range info.Compat.Engines {
			cloned.Compat.Engines[key] = value
		}
	}
	return cloned
}
