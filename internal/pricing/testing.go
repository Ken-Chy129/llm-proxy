package pricing

// NewForTest builds a table from an explicit price list instead of the built-in
// rates, so tests can use round numbers and assert exact costs.
//
// Production has no equivalent on purpose: a model's rate is a published fact,
// not a deployment setting, so there is nothing for an operator to override.
func NewForTest(prices []Price) *Table {
	t := &Table{entries: make(map[string]Price, len(prices))}
	for _, p := range prices {
		name := normalize(p.Name)
		if name == "" {
			continue
		}
		p.Name = name
		t.entries[name] = p
	}
	t.reindex()
	return t
}
