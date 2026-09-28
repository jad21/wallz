package cli

import "testing"

func TestParseLimitEnforcesSupportedBatchBounds(t *testing.T) {
	// Criterio CLI-LIMIT: aceptar solo tamaños explícitos entre uno y cien.
	for _, value := range []string{"0", "101", "bad"} {
		if _, err := ParseLimit(value); err == nil {
			t.Errorf("se aceptó --limit %q", value)
		}
	}
	if got, err := ParseLimit("16"); err != nil || got != 16 {
		t.Fatalf("limit=%d err=%v", got, err)
	}
}
