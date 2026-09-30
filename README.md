# Brighter backend (`brighterd`)

Go service behind the Brighter strategies app. Users pick one of ten
rule-based templates, tune its parameters, see a real backtest, and
invest: the backend runs the strategy as a portfolio engine portfolio of Ondo
tokenized stocks on an EVM chain, in a smart account the user owns.

It holds every secret (backtest engine key pair, the portfolio engine tenant key, Telegram bot
token, database). The frontend talks only to this API.

```
browser ──/v1──▶ brighterd ──▶ backtest engine   backtests + current target (inline strategy trees)
                    │     ├──▶ portfolio engine strategies, enrollment, rebalances, withdrawals
                    │     ├──▶ Nasdaq     daily closes for asset pages (public API, no key)
                    │     └──▶ Telegram   operator notifications
                    └───────▶ Postgres    strategies, portfolios, wallets, evaluation log
```

## How a strategy reaches a portfolio

Historical asset charts can opt into the sibling strategy-engine's Go indexer with `HISTORICAL_PRICES_URL=http://127.0.0.1:8089`. That source reads durable indexed provider closes without dividend adjustment (Yahoo includes split adjustments; Tiingo supplies as-traded closes); the backend bypasses its untagged Nasdaq database rows in this mode. Engine backtests separately consume adjusted snapshots. This option does not replace backtest-engine-backed research and target routes. See the sibling `strategy-engine/indexer/README.md` for configuration and measured provider differences.

1. **Research.** `GET /v1/templates/{id}/backtest` builds the template's
   The backtest engine strategy tree for the chosen parameters and backtests it inline —
   nothing is saved on the backtest engine account. `GET …/target` backtests through
   the latest close and reads the model's weights on the last day the backtest engine has
   them. That is the current target: no account, no order.
2. **Gates.** A target reaches the portfolio engine only if it is complete (weights within
   2% of 100%, rounded to exactly 10,000 bps by largest remainder), names only
   assets in the template's universe, maps every line to an Ondo token on the
   chain, and is fresh (cutoff ≤ 3 trading days old). Every failure is
   reported verbatim; none is relaxed.
3. **Strategy.** One portfolio engine strategy per (template, parameter set, chain),
   created on first investment from a fresh target. Users who choose the same
   parameters share it; each gets their own portfolio.
4. **Enrollment.** `POST /v1/invest/prepare` returns the portfolio engine's 32-byte digest;
   the wallet signs it (EIP-191); `POST /v1/invest/confirm` creates the
   portfolio and starts automation. The user funds the smart account directly
   from their wallet with the chain's cash asset.
5. **Standing re-evaluation.** After each US close the scheduler recomputes
   every held (template, params). It publishes a new portfolio engine version only when
   the weights changed *and* the backtest engine's cutoff advanced (a holiday holds
   rather than republishing). The portfolio engine moves portfolios at the next scheduled
   rebalance, hourly by default (`PORTFOLIO_ENGINE_SCHEDULE_FREQUENCY`). A weekday cron inside
   US market hours, where Ondo mints and redeems, is supported through
   `PORTFOLIO_ENGINE_SCHEDULE_CRON` once the portfolio engine enables cron schedules for the tenant
   (it currently rejects them). Routine evaluations are kept for
   `EVALUATION_RETENTION_DAYS` (90); published ones are kept for good.

## MCP

`mcp.brighter.fi` serves the public catalogue and the Studio authoring loop over
the Model Context Protocol, so a model in any MCP client can browse strategies,
write rules in Brighter's language, check them and run a real backtest.

Everything it exposes is already public and read-only. Nothing touches a
portfolio, a wallet or a key, and there is nothing to authenticate — which is
deliberate: a portfolio is identified by an unguessable id rather than by
proving ownership, so exposing one would turn any id repeated into a chat
transcript into a capability.

Transport is stateless Streamable HTTP: one POST, JSON-RPC 2.0 in, JSON out, no
sessions and no server-sent events. Both protocol eras are served — the
`server/discover` of 2026-07-28, and the `initialize` handshake of 2025-11-25
and earlier, which is what most shipped clients still send.

Eleven tools. Six read the catalogue (`list_strategies`, `get_strategy`,
`backtest_strategy`, `get_allocation`, `find_assets`, `asset_history`); five
write and run rules (`dsl_reference`, `strategy_schema`, `check_strategy`,
`format_strategy`, `backtest_code`). The language specification and the strategy
schema are also resources. The authoring tools need no browser: `strategy-dsl`
is a plain Go package, so parse, check, format and compile all run in process.

It carries its own rate limits (`mcpIP`, `mcpGlobal` in `internal/api/server.go`)
so a model's loop can never spend the website's backtest engine budget, and only the
tools that can reach the backtest engine are charged against the shared one.

`/mcp` is the one route open to every origin (CORS `*`, no credentials), so
browser-based MCP clients work too; the rest of the API keeps its allowlist.

It is listed in the official MCP Registry as `fi.brighter/brighter`, from
`mcp/server.json`. The registry proves the `brighter.fi` domain by the public
key the website serves at `/.well-known/mcp-registry-auth`
(`frontend/public/`); the private key is kept outside every repository, in
`~/.config/brighter/mcp-registry-key.pem`. To publish a change, bump
`version`, then:

```sh
mcp-publisher login http --domain brighter.fi \
  --private-key "$(openssl pkey -in ~/.config/brighter/mcp-registry-key.pem -outform DER | tail -c 32 | xxd -p -c 64)"
mcp-publisher publish mcp/server.json
```

## Templates

| Id | Rule | Universe |
|---|---|---|
| `mag7-rank` | top N Mag7 by trailing return | Mag7, BIL fallback |
| `rsi-riskoff` | Mag7 equal weight; BIL when SPY's RSI is above a threshold | Mag7, BIL |
| `dual-momentum` | stronger of QQQ/SPY if it beats BIL | QQQ, SPY, BIL |
| `inv-vol` | Mag7 weighted by inverse volatility | Mag7 |
| `trend-filter` | each Mag7 sleeve held above its moving average, else BIL | Mag7, BIL |
| `golden-cross` | QQQ while its fast average is above its slow one, else BIL | QQQ, BIL |
| `global-rotation` | top N asset classes by trailing return, cash competing | SPY IWM EFA EEM TLT GLD BIL |
| `all-weather` | inverse-volatility weights | SPY TLT IEF GLD DBC |
| `low-vol` | the N calmest of thirteen blue chips | Mag7 + AVGO JPM LLY COST V WMT |
| `oversold-rebound` | N lowest-RSI Mag7 while SPY is above its 200-day average, else BIL | Mag7, BIL |
| `semis-switch` | SOXL while bonds' RSI leads high-beta stocks', unless chips or volatility are stretched; SOXL dips bought; else BIL | SOXL, BIL |
| `tech-trend-switch` | SQQQ when any of eleven gauges is overbought; 3× dips bought; else XLK vs KMLM picks long (TQQQ/SOXL/QQQ) or hedge (SQQQ/TLT) | TQQQ SOXL QQQ SQQQ TLT, BIL |
| `frontrunner` | BIL when overbought; TQQQ on dips; 3× long or inverse by trend above SPY's 200-day average; Nasdaq hedges below it | TQQQ SOXL QQQ SQQQ SOXS PSQ, BIL |

The last three are popular public strategies adapted to what Ondo lists:
their signals read any ticker, but they hold only registry assets.

Six templates are **retired** (`Retired: true`): they take no new investment
(`TEMPLATE_RETIRED`), the app leaves them out of its lists, and portfolios
already on them keep running.

- `dual-momentum`, `global-rotation`, `all-weather`: at default settings they
  trailed SPY over five and ten years.
- `semis-switch`, `tech-trend-switch`, `frontrunner`: the backtest engine's backtest
  trades at the close that produced the signal; Brighter trades the next day.
  Replaying the backtest engine's own weights one day late cut these fast 3× switchers
  from 166–379%/yr to 97–187%/yr before costs (a slow rule like
  `golden-cross` barely moves), so their backtests overstate what a portfolio
  could earn.

Every tree is validated against the live backtest engine API (backtest and current
target) before it ships.

## Studio strategies

`POST /v1/backtests` compiles a Brighter Studio document
(`internal/strategy/custom.go`) with the same node builders and backtests it
inline. Nothing is saved to the backtest engine. It prices the full research catalogue,
using the same 450 securities registry as BNB portfolio trading. The two
currency contracts are registered for token lookup but have no backtest engine
stock-price history.

Studio publication uses a wallet-signed login challenge: request
`POST /v1/auth/challenge` with `{address, chainId}`, sign the returned message,
and submit `{challengeId, signature}` to `POST /v1/auth/verify`. Challenges
expire after five minutes and can be consumed only once. The returned bearer
session expires after 24 hours and is bound to the permitted browser origin.
`GET /v1/auth/me` reports the authenticated author and publication count;
`POST /v1/auth/logout` revokes that session. Standard EVM account signatures
are supported; contract-wallet signature verification is not implemented.

`POST /v1/strategies` accepts `{strategy, description, parameters}` with that
bearer session. The backend assigns the wallet author, validates numeric
bindings against active rule fields, and saves an immutable rule version.
Only enabled bindings are sent by Studio. Defaults must match the submitted
rules. Percentage bindings include their final sibling's `remainderPath` so
adjustments preserve a 100% sleeve; invalid combinations are rejected before
backtesting or investing.

An author can publish at most ten strategies. PostgreSQL serializes quota
checks and enforces global rule uniqueness. Duplicate checks exclude names,
IDs, descriptions, the comparison benchmark and neutral group wrappers.
Ranking candidate order remains significant because it decides ties.
Published defaults are also checked against the built-in presets.

Published strategies appear in `GET /v1/templates` with `author`,
`publishedAt`, `rules`, and `bindings`, and support the existing backtest,
target, enrollment, portfolio and re-evaluation endpoints. Investment retains
all existing freshness, universe and approved-token/chain checks; publishing
an asset on BNB does not imply an Ethereum contract.

The Studio language specification lives in the sibling Studio repository at
`language/strategy-language.json`. From that checkout, run
`npm run language:sync-backend` after a language change and
`npm run language:check-backend` before committing both repositories. Compiler
limits, metric definitions and transport structs are generated in
`internal/strategy/language_generated.go`; do not edit them directly. Local
contract tests compile Studio's serialized fixtures and verify the generated
fields, metrics and limits against the specification snapshot. No external
services are needed for those tests.

The fixtures include every Studio template and nested allocation cases. A
separate source interpreter compares the compiled score's asset weights on
fixed indicator signals, including 256 generated nested strategies and exact
comparison boundaries. HTTP tests inspect the actual backtest engine payload and
verify that changed rules and ranges cannot reuse an old cache entry. These
checks verify rule translation, not the backtest engine's historical prices or numerical
indicator implementation. The backtest engine responses must contain one strategy curve
and the requested benchmark; missing or ambiguous series fail explicitly.

| Block | What it does |
|---|---|
| `asset` | one security, by research symbol (`NVDA`) or Ondo token (`NVDAon`) |
| `group` | a named container; the name is display only and stays out of the strategy's identity |
| `weight` | `equal`, `inverse-volatility` (a lookback), or `specified` — whole percentages per child adding to 100 |
| `filter` | keep the `top`/`bottom` N children ranked by an indicator over a lookback |
| `condition` | `IF` left indicator `gt\|gte\|eq\|lte\|lt` a number or another indicator, with then/else branches |

Conditions also accept a `predicate`: a comparison (`type: "comparison"`)
or a nested `any`/`all` expression with a `conditions` list. Up to 32 expression
parts are supported. These compile to short-circuit IF/ELSE blocks, preserving
the original then/else allocation. Expanded scores are capped at 6000 blocks.

Indicators: current price, moving average, exponential moving average,
cumulative return, moving-average return, RSI, max drawdown, standard
deviation of returns and of price — each over one lookback — plus the
percentage price oscillator and its signal line, which carry their own
`short-window`, `long-window` and (for the signal) `smooth-window`. `SELECT`
ranks only by the single-lookback indicators.

`SELECT -> WEIGHT -> candidates` ranks the weight's children and allocates
only the selected candidates. Equal weighting lowers to the backtest engine's filter;
inverse-volatility and percentage weighting lower to ranked IF branches with
native asset weights at their leaves. Percentage shares retain their ratios
and sum to 100% after selection. A sole SELECT child under inverse-volatility
weighting uses the same rules and cache identity. These two methods require
asset candidates and must fit the 6000-block expanded-score limit; oversized
combinations return 422 instead of a misleading allocation. Earlier source
candidates win ranking ties. Put a basket inside `GROUP` to rank it as one
candidate using equal weighting.

Rebalance: `daily`, `weekly`, `monthly`, `quarterly`, `yearly`, or
`threshold` with a `corridor` of 1–50%, which reaches the backtest engine as no calendar
plus `rebalance-corridor-width`.

Limits: 2000 blocks, 1000 per list, 20 levels deep, lookbacks of 2–2520 days,
`SELECT` keeps at most 50.

Two of the backtest engine's own blocks are deliberately absent: **market-cap
weighting**, which it now refuses ("no longer supported"), and **MACD**, which
answers 500 for every parameter set. Both were checked against the live API,
as was every block above.

## Chains and cash

| Chain | Id | Cash asset | Status |
|---|---|---|---|
| BNB Chain | 56 | USDT (18 dp) | enabled (`CHAINS=56`) |
| Ethereum | 1 | USDC (6 dp) | in registry, disabled |

The cash asset is Ondo's own cash leg on each chain — the only asset Ondo
redemption settles in — so deposits need no swap before minting and
liquidations settle straight into it. HyperEVM is excluded: it has no BILon
(every template's fallback) and the portfolio engine's enroll rejects chain 999.

BNB token addresses and metadata live in
`internal/tokens/ondo_bnb_registry.json`: 450 stocks/ETFs plus USDon and USDY,
**452 BNB contracts**. The 451 stock-registry addresses match the CSV linked
from [Ondo's official addresses](https://docs.ondo.finance/addresses); USDY
comes from the separate BNB table on that page. This snapshot was checked
against symbol() and decimals() on September 13, 2026.

Studio's `src/domain/ondoBnbRegistry.json` is a generated view of this source.
After changing it, run `npm run tokens:sync` from Studio.
`npm run tokens:check` checks for drift. Existing Ethereum addresses and editorial descriptions live in
`internal/tokens/registry.go`; Ethereum has 28 mapped securities, so BNB-only
contracts are never inferred there. Check mappings against the chain:

```bash
go run ./cmd/brighterd verify-tokens   # reads symbol() and decimals() for 482 mapped contracts, including cash
```

## Running locally

```bash
cp .env.example .env        # DATABASE_URL, BACKTEST_ENGINE_*, optionally PORTFOLIO_ENGINE_API_KEY
go run ./cmd/brighterd      # migrates, then serves :3200
```

Without `PORTFOLIO_ENGINE_API_KEY` research works and investing answers 503. Without
backtest engine credentials research answers 503. `/health/ready` reports both.
Without `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` notifications are off;
leave them unset locally so development does not post to the operators' chat.

Commands: `brighterd serve` (default), `brighterd migrate`,
`brighterd verify-tokens`.

## Tests

```bash
go test ./...                                              # unit + HTTP tests
TEST_DATABASE_URL=postgres://…/brighter go test ./...      # plus the Postgres tests
```

The backtest engine, the portfolio engine, Nasdaq and Telegram are faked with `httptest` servers
(`internal/testkit`); no test calls a real upstream. Postgres tests run in a
throwaway schema of `TEST_DATABASE_URL`, dropped afterwards, and are skipped
when it is unset.

## API

| Method | Path | |
|---|---|---|
| GET | `/health/live`, `/health/ready` | probes |
| GET | `/v1/config` | enabled chains, cash assets, schedule, feature flags |
| GET | `/v1/templates` | built-in and published strategies, authors, parameter grids, holdable assets |
| GET | `/v1/templates/summaries` | every listed strategy's card in one request |
| GET | `/v1/templates/{id}/rules` | a published strategy's rules, for opening in Studio |
| GET | `/v1/strategies` | the list page: filtered, sorted and paged |
| GET | `/v1/strategies/facets` | the tickers and cadences the filter row offers |
| POST | `/v1/backtests` | a Studio strategy, backtested without saving it |
| POST | `/mcp` | Model Context Protocol, served at `mcp.brighter.fi` |
| GET | `/v1/templates/{id}/backtest?range=1y\|3y\|5y\|10y&points=N&<param>=…` | cached 6h |
| GET | `/v1/templates/{id}/target?<param>=…` | current allocation + per-chain readiness, cached 20m |
| GET | `/v1/assets`, `/v1/assets/{ticker}` | asset registry; one asset with token contracts and the strategies that hold it |
| GET | `/v1/assets/{ticker}/closes?year=YYYY` | one calendar year of daily closes from Nasdaq; past years cached 24h, the current year 15m, last good copy served for 7 days if Nasdaq fails |
| POST | `/v1/auth/challenge`, `/v1/auth/verify` | origin-bound, one-use wallet login |
| GET | `/v1/auth/me` | bearer author, publication count and limit |
| POST | `/v1/auth/logout` | revoke bearer session |
| POST | `/v1/strategies` | authenticated publication; 10 per author, globally unique rules |
| POST | `/v1/invest/prepare` | `{templateId, params, chainId, owner}` → signable or existing portfolio |
| POST | `/v1/invest/confirm` | prepare fields + enrollment round-trip + `signature` |
| GET | `/v1/portfolios?owner=0x…` | a wallet's portfolios |
| GET | `/v1/portfolios/{id}` | portfolio + rebalance schedule |
| GET | `/v1/portfolios/{id}/positions` | the portfolio engine positions (passthrough) |
| GET | `/v1/portfolios/{id}/performance` | daily value, net deposits, profit and money-weighted return; cached 1m |
| GET | `/v1/portfolios/{id}/activity` | allocation changes published for the portfolio's strategy since it was created, each against the one before (from `evaluations`) |
| POST | `/v1/portfolios/{id}/deploy` | start automation + immediate rebalance |
| POST | `/v1/portfolios/{id}/withdraw/signature` | `{mode: liquidate\|withdraw, assets?}` → EIP-712 typed data |
| POST | `/v1/portfolios/{id}/withdraw` | `{mode, message, signature}` |
| GET | `/v1/portfolios/{id}/operations/{opId}` | poll an async operation |
| POST | `/v1/admin/reevaluate` | run the re-evaluation now (Bearer `ADMIN_TOKEN`) |
| GET | `/v1/admin/evaluations` | evaluation log |

The asset page loads its range a year at a time and draws each year as it
arrives, so the chart shows the newest year while older ones are fetched.

Errors are `{"error": "...", "code": "..."}`. Trading errors pass through
verbatim; research and price failures are logged and answered generically.
Research routes are rate-limited per IP on cache misses only — 15 upstream
computations a minute, bursts of 20 — so cached answers are never refused.
Writes are limited to 20/min. Withdrawal proceeds always go to the portfolio
owner's own address.

The portfolio engine reads (GET) are retried twice on 502/503/504; writes never are, since
a write that timed out may still have happened. Nasdaq requests are retried
twice on 429 and 5xx.

## Notifications

With `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` set, the service posts to
that chat: a wallet's first visit, new strategies and portfolios, published
allocation changes, re-evaluation errors, requested rebalances and
withdrawals. Sending is queued and best-effort; Telegram being slow or down
never affects a request, and the token never appears in a log.

Anyone can subscribe too: the bot long-polls its updates, `/start` adds the
chat to `telegram_subscribers`, and `/stop` or blocking the bot removes it.
Subscribers get every event worded without portfolio ids or wallet addresses
(a portfolio id opens its holdings in the app) and without error details; the
operator chat keeps the full text. Broadcasts are spaced under Telegram's rate
limit. Both variables are the switch for all of it, so a local copy holding
only the token neither posts nor competes with production for the updates.

## Layout

```
cmd/brighterd/          entrypoint: serve | migrate | verify-tokens
internal/config/        env config + .env loader
internal/backtestengine/      backtest engine client and response parsing
internal/metrics/       return, drawdown, volatility, Sharpe, Sortino from an equity curve
internal/prices/        Nasdaq daily closes for asset pages
internal/notify/        Telegram notifier
internal/strategy/      the thirteen templates (six retired), parameter grid, bps rounding, gates
internal/tokens/        chains, cash assets, Ondo token registry, on-chain verifier
internal/portfolioengine/        portfolio engine client
internal/store/         Postgres (pgx) + embedded migrations
internal/service/       research, investing, portfolios, re-evaluation, cache
internal/scheduler/     daily re-evaluation loop + retention (advisory-locked)
internal/api/           HTTP routes, errors, CORS, rate limits
internal/testkit/       fake upstreams and a throwaway Postgres schema, for tests
deploy/, scripts/       systemd unit and deploy script
```

## Deploying

`./scripts/deploy.sh` tests, cross-compiles a static linux binary, uploads it
to `onepage:/opt/brighter-backend`, runs migrations and restarts
`brighter-backend.service`, then waits for `/health/ready`. It never uploads
secrets; `/etc/brighter-backend/env` on the server is the only copy. One-time
setup steps are listed at the top of the script.
