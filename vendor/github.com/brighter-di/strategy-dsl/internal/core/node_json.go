package core

import (
	"bytes"
	"encoding/json"
)

// identityJSON encodes v the way the generated omitempty tags always did, for
// hashes that must not change: compile keys identify published strategies.
// MarshalJSON writes the explicit zero value as the last member of a node
// object, and inside a JSON string a quote after a comma is always escaped, so
// these bytes can only come from that field.
func identityJSON(v any) ([]byte, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	encoded = bytes.ReplaceAll(encoded, []byte(`,"value":0}`), []byte(`}`))
	return bytes.ReplaceAll(encoded, []byte(`,"value":-0}`), []byte(`}`)), nil
}

// MarshalJSON keeps a fixed comparison value of 0. The generated transport
// type tags every field omitempty, which would drop that value and make a
// parsed strategy fail its own diagnostics.
func (n Node) MarshalJSON() ([]byte, error) {
	type plain Node
	if n.CompareMode != "value" || n.Value != 0 {
		return json.Marshal(plain(n))
	}
	return json.Marshal(struct {
		plain
		Value float64 `json:"value"`
	}{plain(n), n.Value})
}
