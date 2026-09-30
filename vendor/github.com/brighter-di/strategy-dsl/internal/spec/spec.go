// Package spec owns the embedded language contracts used by the core.
package spec

import _ "embed"

//go:embed strategy-language.json
var language []byte

//go:embed assistant-strategy.schema.json
var assistant []byte

func Language() []byte  { return append([]byte(nil), language...) }
func Assistant() []byte { return append([]byte(nil), assistant...) }
