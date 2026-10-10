package mockaccounting

import "testing"

func TestRandomOutcomeHasRequiredDistribution(t *testing.T) {
	counts := map[string]int{}
	for draw := 0; draw < 100; draw++ {
		counts[outcome(draw)]++
	}
	if counts["fail"] != 20 || counts["hang"] != 10 || counts["success"] != 70 {
		t.Fatalf("unexpected probability buckets: %v", counts)
	}
}
