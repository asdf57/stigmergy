package resource

import "testing"

func TestEqualJSONNormalizesStoredNumbers(t *testing.T) {
	left := map[string]any{"observedGeneration": int64(1), "nested": map[string]any{"count": 2}}
	right := map[string]any{"nested": map[string]any{"count": float64(2)}, "observedGeneration": float64(1)}
	if !EqualJSON(left, right) {
		t.Fatalf("EqualJSON(%#v, %#v) = false", left, right)
	}
	right["observedGeneration"] = float64(2)
	if EqualJSON(left, right) {
		t.Fatalf("EqualJSON(%#v, %#v) = true", left, right)
	}
}
