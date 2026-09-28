// Package cycle owns deterministic state transitions for content rotation.
package cycle

import (
	"errors"
	"math/rand"
)

const (
	SourceLocal  = "local"
	SourceRemote = "remote"
)

var ErrExhausted = errors.New("cursor agotado")

// Item is one catalog identity within a numbered no-repeat epoch.
type Item struct {
	ID    string `json:"id"`
	Epoch int    `json:"epoch"`
}

// State points at the next item and persists the visible content and source turn.
type State struct {
	Order      []Item   `json:"order"`
	Position   int      `json:"position"`
	CurrentID  *string  `json:"current_id"`
	Skipped    []string `json:"skipped"`
	SourceTurn string   `json:"source_turn"`
}

// InitialState counts a known visible image as already consumed in the new epoch.
func InitialState(ids []string, currentID *string, rng *rand.Rand) (State, error) {
	if len(ids) == 0 {
		return State{}, errors.New("el catálogo no tiene imágenes")
	}
	pending := make([]string, 0, len(ids))
	for _, id := range ids {
		if currentID == nil || id != *currentID {
			pending = append(pending, id)
		}
	}
	return State{Order: shuffle(pending, currentID, 0, rng), CurrentID: currentID, Skipped: []string{}, SourceTurn: SourceLocal}, nil
}

// EnsureUpcoming extends the order by whole epochs, never repeating within one epoch.
func EnsureUpcoming(state State, ids []string, count int, rng *rand.Rand) (State, error) {
	if count < 1 || len(ids) == 0 {
		return State{}, errors.New("no hay suficientes contenidos para preparar el lote")
	}
	for len(state.Order)-state.Position < count {
		epoch := 1
		var avoid *string
		if len(state.Order) > 0 {
			last := state.Order[len(state.Order)-1]
			epoch = last.Epoch + 1
			avoid = &last.ID
		} else {
			avoid = state.CurrentID
		}
		state.Order = append(state.Order, shuffle(ids, avoid, epoch, rng)...)
	}
	return state, nil
}

// MarkApplied advances only after the compositor accepts the selected image.
func MarkApplied(state State, source string) (State, error) {
	if state.Position >= len(state.Order) {
		return State{}, ErrExhausted
	}
	if source != "" && source != SourceLocal && source != SourceRemote {
		return State{}, errors.New("procedencia aplicada inválida")
	}
	item := state.Order[state.Position]
	state.Position++
	state.CurrentID = &item.ID
	if source != "" {
		if source == SourceLocal {
			state.SourceTurn = SourceRemote
		} else {
			state.SourceTurn = SourceLocal
		}
	}
	return state, nil
}

// SkipCurrent records a failed identity without changing the visible image.
func SkipCurrent(state State) (State, error) {
	if state.Position >= len(state.Order) {
		return State{}, ErrExhausted
	}
	state.Skipped = append(state.Skipped, state.Order[state.Position].ID)
	state.Position++
	return state, nil
}

func shuffle(ids []string, avoid *string, epoch int, rng *rand.Rand) []Item {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if rng != nil {
		rng.Shuffle(len(unique), func(i, j int) { unique[i], unique[j] = unique[j], unique[i] })
	}
	if avoid != nil && len(unique) > 1 && unique[0] == *avoid {
		unique = append(unique[1:], unique[0])
	}
	result := make([]Item, len(unique))
	for i, id := range unique {
		result[i] = Item{ID: id, Epoch: epoch}
	}
	return result
}
