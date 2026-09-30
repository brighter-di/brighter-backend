# Strategy DSL

Standalone Brighter strategy language for native Go and browser/Node WebAssembly.
The Go core parses DSL into the Studio/backend JSON transport, validates the rules,
prints canonical DSL and compiles the backtest engine compatible score trees. It performs no
network requests and has no dependency on Studio, brighterd, token contracts, market
data or a backtesting engine.

Studio uses this core through the pinned WebAssembly package. Backend adoption
of the native module remains separate.

## Build and check

Requirements: Go 1.25 or newer, Node 22 or newer, and Make. There are no Go or npm
dependencies to install.

```sh
make check          # generated definitions, vet, Go tests, Wasm build and Wasm tests
make wasm           # dist/strategy-dsl.wasm, matching runtime, JS loader and TS types
make package        # build and produce @brighter-di/strategy-dsl as an npm tarball
```

The browser module uses Go's `js/wasm` target and ships `wasm_exec.js` from the same
compiler installation. The runtime and `.wasm` file must be distributed together.
See [Go's WebAssembly documentation](https://go.dev/wiki/WebAssembly).

## Repository layout

- `dsl.go` is the stable public Go façade.
- `internal/core/` contains parsing, validation, formatting, compilation and its private tests.
- `internal/spec/` owns the embedded language and assistant contracts.
- `internal/generate/` derives Go and TypeScript transport definitions.
- `cmd/`, `web/`, `scripts/`, `examples/` and `testdata/` contain adapters and fixtures.
- `.artifacts/` receives local npm tarballs and is ignored; `dist/` contains build output only.

## Native Go

The module is `github.com/brighter-di/strategy-dsl`; its package name is `dsl`.

```go
import dsl "github.com/brighter-di/strategy-dsl"

strategy, err := dsl.Parse(source)
if err != nil { /* err may be *dsl.SyntaxError, with one-based line and column */ }

compiled, err := dsl.Compile(*strategy)
if err != nil { /* err is *dsl.ValidationError for invalid rules */ }

code, err := dsl.Format(*strategy)
```

`Parse` checks syntax and returns a transport AST. `Validate` checks semantic
readiness; `Compile` uses those same checks and returns `{tree, benchmark, key}`.
`Format` validates first, then prints canonical text. `CompileSource` and
`FormatSource` combine parsing with those operations.

`key` is a stable identity for the rules, rebalance settings and benchmark. Names,
editor IDs and inactive fields do not change it. Score node UUIDs are freshly
generated for every compilation. Asset display names are the canonical ticker;
presentation labels belong to the consuming application.

## WebAssembly / JavaScript

After `make wasm`, use the module directly or install the tarball made by
`make package`. The package exposes TypeScript declarations and requires no
JavaScript language implementation.

```js
import { loadStrategyDSL } from '@brighter-di/strategy-dsl';

const dsl = await loadStrategyDSL();
const parsed = dsl.parse(source);
const checked = dsl.validate(source);
const formatted = dsl.format(source);
const compiled = dsl.compile(source);

if (compiled.ok) console.log(compiled.compiled.tree);
else console.log(compiled.diagnostics);
```

`format`, `validate` and `compile` accept either DSL text or a transport AST.
`loadStrategyDSL` initializes once per module; it also accepts an explicit Wasm
URL, bytes, a `WebAssembly.Module`, or a `Response`. The default loads the `.wasm`
file beside the loader. Calls are synchronous after initialization; larger
strategies can be run inside a Web Worker using the same loader.

The editor API adds `parseDraft`, `diagnose`, `formatDraft`, `formatMeasure`,
`formatPredicate`, `selectionCapacity`, `importAssistant`, `assistantSchema`,
`selectionMetadata`, and `editorLayout`. `parseDraft` accepts blank and header-only documents while
retaining the normal grammar and source limits. Its JSON response explicitly
includes zero comparison values and `sourceMetadata`: explicit header positions
and source locations keyed by AST paths, including nested predicates. `diagnose` returns all bounded field diagnostics
(up to 2,000 per request) with `path` arrays (for example `["children", 0, "predicate", "conditions", 1,
"lookback"]`), `level`, and optional `sourceLevel`. Errors can remain editable
readiness issues; `sourceLevel` tells an editor whether to reject a source edit
or show a warning. `requireName: true` also requires a nonempty display name.

Validation and strict formatting count score expansion without generating UUIDs
or constructing execution trees. Weighted-selection diagnostics and compilation
share the same bounded decision plan; `selectionCapacity(pool, count)` exposes
that plan's capacity without constructing a surrogate strategy. Compiled rule keys and execution shapes
remain compatible with the frozen backend references.

`formatDraft` accepts a transport AST, preserves full labels and prints incomplete
or invalid allocation drafts without repairing them. It does not validate
execution readiness. Its raw JSON boundary preserves missing shares, fractional
values, and nonfinite editor values encoded as strings such as `"NaN"`. Independent
structural limits (64 levels, 6,000 blocks, and 8 MiB of output) bound printing even
when a draft exceeds the stricter execution limits. `formatMeasure` and
`formatPredicate` accept a `node` expression and return a `code` fragment.
Always call `diagnose`/`validate` or `compile` before execution.

`assistantSchema()` exposes the schema generated by this core for the legacy
`type`/`root` model envelope. `importAssistant(assistant, options)` checks that
schema, converts to the execution transport with the established editor defaults,
resolves aliases and checks semantic readiness. Its result contains the validated
`strategy`; display text cleanup and editor IDs belong to the caller. Native Go
provides `Compiler.ImportAssistant(rawJSON)` and `AssistantSchema()`.

`selectionMetadata(node)` returns `{candidatePath, count}` so consumers can find
the original candidate nodes without repeating SELECT interpretation. Numeric
draft fields do not affect metadata. `editorLayout(source)` returns `{tidied,
repaired, openIfIndent}` for incomplete editor text. IF indentation recovery is
explicit and separate from strict formatting; callers must parse the proposed
text before applying it. It never invents missing rules, branches or values.

Run a complete Node example with `node examples/node.mjs`. For the browser
example, serve this repository with `python3 -m http.server 8080`, then open
`http://localhost:8080/examples/browser.html`.

## Asset policy

The language accepts symbols matching `[A-Za-z][A-Za-z0-9._]*` and canonicalizes
them to upper case. It does not decide which assets a platform can price or trade.
Pass an explicit allowlist to enforce availability and resolve aliases:

```go
compiler := dsl.New(dsl.Options{Tickers: map[string]string{
    "SPY": "SPY", "SPYon": "SPY",
    "AAPL": "AAPL", "AAPLon": "AAPL",
}})
compiled, err := compiler.CompileSource(source)
```

```js
const result = dsl.compile(source, {
  tickers: { SPY: 'SPY', SPYon: 'SPY', AAPL: 'AAPL', AAPLon: 'AAPL' },
});
```

Aliases are case insensitive. A missing allowlist accepts syntactically valid
symbols; an empty allowlist accepts no assets. The compiler copies its options
and is safe for concurrent native use. The frozen Ondo research-symbol map under
`testdata/` exists only to verify compatibility and is not embedded in the runtime.

## Language

```text
STRATEGY "Momentum with a safety net"
REBALANCE monthly
BENCHMARK SPY

IF price(SPY) > moving_average(SPY, 200 days)
  SELECT top 2 BY return (126 days)
    WEIGHT equally
      BUY AAPL
      BUY MSFT
      BUY NVDA
ELSE
  BUY BIL
END
```

Headers are optional for fragments and default to `Studio strategy`, `monthly`
and `SPY`. They appear at most once each, at the top level before the first rule.
Keywords, indicator names and modifiers are case insensitive. Canonical output
uses uppercase keywords, lowercase indicator names and two spaces of indentation.
A leading tab is treated as two spaces. Quoted names use JSON escapes. Comments,
variables and unsupported commands are rejected.

Supported blocks are `BUY`, `GROUP`, `WEIGHT`, `SELECT`, and `IF`/`ELSE`/`END`.
Both conditional branches must contain rules; `ELSE` and `END` align with `IF`.
Predicates support all five comparisons (`>`, `>=`, `=`, `<=`, `<`), numeric or
indicator operands, and nested `ANY`/`ALL`.

| DSL indicator | Parameters |
| --- | --- |
| `price` | ticker |
| `moving_average`, `ema`, `return`, `return_average`, `rsi`, `drawdown`, `volatility`, `price_volatility` | ticker, lookback in days |
| `ppo` | ticker, short window, long window |
| `ppo_signal` | ticker, short window, long window, smoothing window |

Rebalance modes are daily, weekly, monthly, quarterly, yearly and threshold.
`REBALANCE threshold (10%)` emits the backtest engine's `none` calendar with a drift corridor.
Weights support equal allocation, inverse volatility, and whole percentages
that sum to 100. Percentage suffixes apply only directly inside percentage weights.

`SELECT` ranks by a one-lookback indicator. A sole `WEIGHT` child allocates across
the selected candidates; it does not make the whole pool one ranked basket.
Inverse-volatility and percentage selections require asset candidates and retain
the backend's source-order tie breaking and percentage renormalization. Wrap a
basket in `GROUP` to rank it as one candidate. Multiple root blocks share capital
equally. Compilation never executes a strategy or fetches price data.

Limits remain aligned with the initial Studio/backend contract: 20 block levels,
2,000 source blocks, 1,000 blocks per list, 32 predicate parts, 6,000 compiled score
blocks, lookbacks of 2–2,520 whole days, selection counts of 1–50, threshold drift
of 1–50%, and fixed comparison values from −1,000,000 to 1,000,000. Source parsing
also has a 4 MiB byte limit; the JSON boundary has an 8 MiB request limit.

Formatting omits editor IDs, display names on asset nodes and inactive fields,
and normalizes display names using the backend's 80-character limit. It preserves
the strategy's rule identity. Malformed or semantically invalid code returns an
error; formatting does not infer missing branches or repair indentation.

## JSON ABI and CLI

The CLI and Wasm call the same versioned `ExecuteJSON` API:

```json
{"operation":"compile","source":"BUY SPY","options":{"tickers":{"SPY":"SPY"}}}
```

Success returns `ok`, package `version`, `languageVersion`, and an operation-specific
`strategy`, `code`, `compiled`, or `specification` field. Errors return
`diagnostics` with a code and message; syntax errors also carry one-based line
and column positions. The strict operations are `parse`, `format`, `validate`,
`compile` and `specification`; the editor operations are `parseDraft`, `diagnose`,
`formatDraft`, `formatMeasure`, `formatPredicate`, `editorLayout`,
`selectionMetadata`, `selectionCapacity`, `importAssistant` and `assistantSchema`
(the full list is `dsl.Operations`). Exactly one of `source` or `strategy` is
required for the strict operations except `specification`. A panic inside the
core answers the request with `INTERNAL_ERROR` rather than stopping the runtime. The AST uses the existing backend `kind`/`children` transport,
not the assistant's separate `type`/`root` envelope.

```sh
go run ./cmd/strategy-dsl -operation compile -input examples/momentum.dsl
go run ./cmd/strategy-dsl -operation format -input examples/momentum.dsl
go run ./cmd/strategy-dsl -operation specification
printf '%s\n' '{"operation":"compile","source":"BUY SPY"}' |
  go run ./cmd/strategy-dsl -operation json
```

The CLI prints the JSON response and exits with status 1 on errors.

## Specification and compatibility

`internal/spec/strategy-language.json` is the authority for metric mappings, transport
fields, enums and numeric limits. `make generate` regenerates Go definitions and
TypeScript declarations and the assistant JSON schema. `make check-generated` rejects stale outputs. Grammar
parsing and score lowering are implemented only in Go.

Compatibility tests include 75 frozen Studio contract cases, 92 language/parity
DSL scripts, and the backend's compiler, boolean truth-table and weighted-selection
regressions. A separate frozen reference captures execution-tree fingerprints and
rule keys from the actual backend compiler. Wasm tests replay every Studio contract
case using the built binary, including rejected inputs. Another 75 frozen
pre-cleanup assistant cases verify readiness and compiled identities. See
`testdata/ASSISTANT_REFERENCE.md` for their provenance.

See [EXTRACTION.md](EXTRACTION.md) for source revisions and future adoption boundaries.
