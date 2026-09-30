-- One portfolio engine strategy per (template, parameter set, chain). Users who pick
-- the same parameters on the same chain share a strategy; each gets their own
-- portfolio enrolled in it.
CREATE TABLE strategies (
    id                  BIGSERIAL   PRIMARY KEY,
    template_id         TEXT        NOT NULL,
    params_key          TEXT        NOT NULL,
    params              JSONB       NOT NULL,
    chain_id            BIGINT      NOT NULL,
    portfolio_engine_strategy_id  TEXT        NOT NULL UNIQUE,
    -- The allocation currently published to the portfolio engine, in basis points by
    -- model ticker, and the data cutoff it was computed from.
    allocation          JSONB       NOT NULL,
    allocation_hash     TEXT        NOT NULL,
    cutoff              DATE        NOT NULL,
    version             INT         NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (template_id, params_key, chain_id)
);

CREATE TABLE portfolios (
    portfolio_id     TEXT        PRIMARY KEY,
    owner            TEXT        NOT NULL CHECK (owner ~ '^0x[0-9a-f]{40}$'),
    strategy_id      BIGINT      NOT NULL REFERENCES strategies (id),
    chain_id         BIGINT      NOT NULL,
    deposit_address  TEXT        NOT NULL CHECK (deposit_address ~ '^0x[0-9a-f]{40}$'),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set when the portfolio engine reports the portfolio no longer exists. Kept, not
    -- deleted, so the history of who held what is never lost.
    dropped_at       TIMESTAMPTZ
);

CREATE INDEX portfolios_owner_active ON portfolios (owner) WHERE dropped_at IS NULL;
CREATE UNIQUE INDEX portfolios_owner_strategy_active ON portfolios (owner, strategy_id) WHERE dropped_at IS NULL;

-- Every evaluation of a (template, parameter set): what the backtest engine said, and
-- what we did about it. The scheduler reads the last accepted cutoff from
-- here so a holiday (cutoff not advancing) holds instead of republishing.
CREATE TABLE evaluations (
    id           BIGSERIAL   PRIMARY KEY,
    template_id  TEXT        NOT NULL,
    params_key   TEXT        NOT NULL,
    trigger      TEXT        NOT NULL CHECK (trigger IN ('create', 'schedule', 'manual')),
    status       TEXT        NOT NULL CHECK (status IN ('published', 'unchanged', 'held', 'error')),
    cutoff       DATE,
    weights      JSONB,
    change_hash  TEXT,
    reason       TEXT,
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX evaluations_combo ON evaluations (template_id, params_key, evaluated_at DESC);
