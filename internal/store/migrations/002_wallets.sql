-- Every wallet the app has seen, so a first visit can be told apart from a
-- return (and announced to operators once).
CREATE TABLE wallets (
    address     TEXT        PRIMARY KEY CHECK (address ~ '^0x[0-9a-f]{40}$'),
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Owners who already hold a portfolio are not new.
INSERT INTO wallets (address, first_seen)
SELECT owner, min(created_at) FROM portfolios GROUP BY owner
ON CONFLICT DO NOTHING;
