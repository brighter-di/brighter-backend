package core

// The backtest engine weights the child blocks of a weight, not the assets returned by
// a filter child. Compile weighted selection to ranked asset choices whose
// leaves carry the actual requested allocation. Bound the serialized score
// before constructing a branch that would exceed the expansion limit.
func selectedWeight(selection, allocation Node, candidates []node) (node, Node, error) {
	for _, child := range selection.Children {
		if child.Kind != "asset" {
			return nil, Node{}, invalid("Selection with inverse-volatility or percentage weights needs asset candidates. Use equal weighting to select whole groups.")
		}
	}
	count := min(selection.Count, len(candidates))
	// Retain original shares and the inverse-volatility window in identity.
	// This internal kind deliberately differs from the old basket identity.
	canon := Node{Kind: "selected-weight", Method: allocation.Method, Lookback: allocation.Lookback,
		Children: []Node{selection}}
	leaf := func(indices []int) node {
		assets := make([]node, len(indices))
		shares := make([]int, len(indices))
		total := 0
		for i, index := range indices {
			// Specified weights mutate their children. Each decision leaf needs
			// its own copy and its own denominator after selection.
			assets[i] = make(node, len(candidates[index]))
			for key, value := range candidates[index] {
				assets[i][key] = value
			}
			assets[i]["id"] = newID()
			shares[i] = selection.Children[index].Percent
			total += shares[i]
		}
		if allocation.Method == "inverse-volatility" {
			return wtInverseVol(allocation.Lookback, assets...)
		}
		out := wtSpecified(shares, assets...)
		for _, asset := range assets {
			asset["weight"].(node)["den"] = total
		}
		return out
	}
	plan, err := planWeightedSelection(len(candidates), count)
	if err != nil {
		return nil, Node{}, err
	}
	cache := map[*selectionPlan]node{}
	var emit func(*selectionPlan) node
	emit = func(p *selectionPlan) node {
		if cached := cache[p]; cached != nil {
			return cached
		}
		if p.wins == nil {
			return leaf(p.indices)
		}
		comparator := "gt"
		if selection.Direction == "bottom" {
			comparator = "lt"
		}
		fn := measures[selection.Metric].fn
		lhs := operand{fn: fn, val: selection.Children[p.next].Ticker, window: selection.Lookback}
		rhs := operand{fn: fn, val: selection.Children[p.position].Ticker, window: selection.Lookback}
		out := ifElse(lhs, comparator, rhs, []node{emit(p.wins)}, []node{emit(p.loses)})
		cache[p] = out
		return out
	}
	return emit(plan), canon, nil
}

func weightedSelectionTooLarge() error {
	return invalid("Selection with inverse-volatility or percentage weights exceeds the %d-block compilation limit. Reduce the candidate pool or use equal weighting.", customMaxExpandedBlocks)
}

// assetSelection is kept narrow: basket ranking remains explicit through GROUP.
func assetSelection(n Node) bool {
	if n.Kind != "filter" || len(n.Children) == 0 {
		return false
	}
	for _, child := range n.Children {
		if child.Kind != "asset" {
			return false
		}
	}
	return true
}
