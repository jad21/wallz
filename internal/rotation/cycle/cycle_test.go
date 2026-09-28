package cycle

import (
	"math/rand"
	"testing"
)

func TestCycleDoesNotRepeatBeforeEveryIDIsUsed(t *testing.T) {
	// Objetivo: mantener una permutación completa antes de comenzar el siguiente ciclo.
	ids := []string{"a", "b", "c"}
	state, err := InitialState(ids, nil, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for range ids {
		state, err = EnsureUpcoming(state, ids, 2, rand.New(rand.NewSource(2)))
		if err != nil {
			t.Fatal(err)
		}
		id := state.Order[state.Position].ID
		if seen[id] {
			t.Fatalf("id repetido antes de terminar ciclo: %s", id)
		}
		seen[id] = true
		state, err = MarkApplied(state, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("usados %d ids; esperados %d", len(seen), len(ids))
	}
}

func TestSkipPreservesCurrentAndRecordsSkippedID(t *testing.T) {
	// Criterio R-OFFLINE: una fuente fallida avanza el cursor sin cambiar el fondo visible.
	state, err := InitialState([]string{"a", "b"}, stringPtr("prior"), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	failed := state.Order[state.Position].ID
	state, err = SkipCurrent(state)
	if err != nil {
		t.Fatal(err)
	}
	if state.Position != 1 || state.CurrentID == nil || *state.CurrentID != "prior" || len(state.Skipped) != 1 || state.Skipped[0] != failed {
		t.Fatalf("estado inesperado después de omitir: %+v", state)
	}
}

func TestAppliedSourceAlternatesNextPreferredSource(t *testing.T) {
	// Criterio R-MIX: alternar la preferencia según el origen del último fondo aplicado.
	state, err := InitialState([]string{"a", "b"}, nil, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	state, err = MarkApplied(state, SourceLocal)
	if err != nil || state.SourceTurn != SourceRemote {
		t.Fatalf("turno remoto esperado, estado=%+v error=%v", state, err)
	}
	state, err = MarkApplied(state, SourceRemote)
	if err != nil || state.SourceTurn != SourceLocal {
		t.Fatalf("turno local esperado, estado=%+v error=%v", state, err)
	}
}

func stringPtr(value string) *string { return &value }
