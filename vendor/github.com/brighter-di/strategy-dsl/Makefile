.PHONY: generate check-generated test test-go test-wasm wasm typecheck check package

generate:
	go generate ./...

check-generated:
	go run ./internal/generate -check

test-go:
	go vet ./...
	go test ./...

wasm:
	node scripts/build-wasm.mjs

# The Wasm tests run against a fresh build, so `make test` never skips them.
test-wasm: wasm
	node --test web/*.test.mjs

test: test-go test-wasm

typecheck:
	npm run typecheck

check: check-generated test typecheck

package: wasm
	mkdir -p .artifacts
	npm pack --pack-destination .artifacts --cache .artifacts/npm-cache
	node scripts/checksums.mjs
