package core

import "fmt"

// The bounded decision plan is shared by diagnostics and score emission.
// Its size counts serialization, including repeated references to cached plans.
type selectionPlan struct {
	size, next, position int
	indices              []int
	wins, loses          *selectionPlan
}

func planWeightedSelection(pool, requested int) (*selectionPlan, error) {
	count := min(pool, requested)
	if pool < 1 || pool > customMaxList || requested < 1 || requested > customMaxCount {
		return nil, invalid("Invalid weighted selection pool or count.")
	}
	if count == pool {
		indices := make([]int, count)
		for i := range indices {
			indices[i] = i
		}
		return &selectionPlan{size: 1 + count, indices: indices}, nil
	}
	cache := map[string]*selectionPlan{}
	var build func(int, []int) (*selectionPlan, error)
	var insert func(int, int, []int) (*selectionPlan, error)
	build = func(next int, selected []int) (*selectionPlan, error) {
		if next == pool {
			return &selectionPlan{size: 1 + count, indices: append([]int(nil), selected...)}, nil
		}
		key := fmt.Sprint(next, ":", selected)
		if plan := cache[key]; plan != nil {
			return plan, nil
		}
		out, err := insert(next, 0, selected)
		if err == nil {
			cache[key] = out
		}
		return out, err
	}
	insert = func(next, position int, selected []int) (*selectionPlan, error) {
		if position == len(selected) {
			if len(selected) < count {
				selected = append(append([]int(nil), selected...), next)
			}
			return build(next+1, selected)
		}
		updated := append([]int(nil), selected[:position]...)
		updated = append(updated, next)
		updated = append(updated, selected[position:]...)
		updated = updated[:min(count, len(updated))]
		wins, err := build(next+1, updated)
		if err != nil {
			return nil, err
		}
		loses, err := insert(next, position+1, selected)
		if err != nil {
			return nil, err
		}
		size := 3 + wins.size + loses.size
		if size > customMaxExpandedBlocks {
			return nil, weightedSelectionTooLarge()
		}
		return &selectionPlan{size: size, next: next, position: selected[position], wins: wins, loses: loses}, nil
	}
	return build(0, nil)
}
