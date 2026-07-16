// Package stubabi contains the allocation-free, introspection-only ABI shared
// by protocol scaffolds while their data paths are still unimplemented.
package stubabi

import wago "github.com/wago-org/wago"

const (
	// ABIVersion1 encodes placeholder ABI version 1.0.
	ABIVersion1 uint32 = 0x0001_0000
	// FeatureNone truthfully reports that no request/stream data path is ready.
	FeatureNone uint64 = 0
)

// ABIVersion returns the protocol scaffold ABI version.
func ABIVersion(_ wago.HostModule, params, results []uint64) {
	if len(params) != 0 || len(results) != 1 {
		return
	}
	results[0] = uint64(ABIVersion1)
}

// FeatureFlags returns zero until a protocol data path is implemented and
// independently tested.
func FeatureFlags(_ wago.HostModule, params, results []uint64) {
	if len(params) != 0 || len(results) != 1 {
		return
	}
	results[0] = FeatureNone
}
