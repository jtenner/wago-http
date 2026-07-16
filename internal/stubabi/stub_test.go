package stubabi

import "testing"

func TestIntrospectionHostFunctions(t *testing.T) {
	results := []uint64{^uint64(0)}
	ABIVersion(nil, nil, results)
	if results[0] != uint64(ABIVersion1) {
		t.Fatalf("ABI version = %#x", results[0])
	}
	results[0] = ^uint64(0)
	FeatureFlags(nil, nil, results)
	if results[0] != FeatureNone {
		t.Fatalf("feature flags = %#x", results[0])
	}
}

func TestInvalidShapesDoNotMutateOutput(t *testing.T) {
	results := []uint64{123}
	ABIVersion(nil, []uint64{1}, results)
	if results[0] != 123 {
		t.Fatalf("ABI version mutated invalid output: %d", results[0])
	}
	FeatureFlags(nil, []uint64{1}, results)
	if results[0] != 123 {
		t.Fatalf("feature flags mutated invalid output: %d", results[0])
	}
}
