# Changelog

## 0.3.3

- `parseDraft` and `importAssistant` no longer decode and re-encode their whole
  response to add zero comparison values; `Node` JSON has carried them since 0.3.2.
  `parseDraft` is about 2.4 times faster and allocates about a fifth as often.
  Response keys keep their struct order instead of being sorted.

## 0.3.2

- A panic inside the core now answers the request with `INTERNAL_ERROR` instead of
  stopping the Wasm runtime. If the runtime does stop, the next `loadStrategyDSL()`
  call starts a fresh one.
- Requests over the 8 MiB limit are rejected before they are copied into Go memory.
- A fixed comparison value of `0` is kept in every JSON response and in `json.Marshal`,
  so a parsed strategy passes its own diagnostics.
- The loader compiles the Wasm while it downloads when the server sends
  `application/wasm`, adds `executeRaw(json)` for callers that already hold a JSON
  string, and no longer leaves its binding on `globalThis`.
- The package version has one source, `package.json`; the Go `Version` is generated
  from it.
- The generator fails when the language specification is missing a limit.
- Builds are pinned to one Go toolchain (`go.mod`), CI publishes the tarball with
  `SHA256SUMS`, and the package ships a license and the Go runtime's notice.
- Packaged contents follow the `internal/` layout from the reorganisation after 0.3.1;
  `./language` resolves to `internal/spec/strategy-language.json`.

## 0.3.1

- Validate assistant aliases before canonicalising transport.
