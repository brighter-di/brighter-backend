CREATE TABLE auth_challenges (
    id TEXT PRIMARY KEY,
    author TEXT NOT NULL CHECK (author ~ '^0x[0-9a-f]{40}$'),
    origin TEXT NOT NULL,
    message TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);
CREATE INDEX auth_challenges_expiry ON auth_challenges (expires_at);
CREATE TABLE auth_sessions (
    token_hash TEXT PRIMARY KEY,
    author TEXT NOT NULL CHECK (author ~ '^0x[0-9a-f]{40}$'),
    origin TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX auth_sessions_expiry ON auth_sessions (expires_at);
CREATE TABLE published_strategies (
    id TEXT PRIMARY KEY,
    author TEXT NOT NULL CHECK (author ~ '^0x[0-9a-f]{40}$'),
    author_slot SMALLINT NOT NULL CHECK (author_slot BETWEEN 1 AND 10),
    rules_key TEXT NOT NULL UNIQUE,
    document JSONB NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (author, author_slot)
);
