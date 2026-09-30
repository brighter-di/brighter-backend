-- Daily closes behind asset pages, one row per ticker and trading day. A past
-- year never changes, so once fetched it is served from here for good and the
-- price source is only asked about the current year.
CREATE TABLE daily_closes (
    ticker  TEXT             NOT NULL,
    day     DATE             NOT NULL,
    close   DOUBLE PRECISION NOT NULL CHECK (close > 0),
    PRIMARY KEY (ticker, day)
);

-- The (ticker, year) pairs stored in full: a year counts once it was fetched
-- after it ended.
CREATE TABLE close_years (
    ticker      TEXT        NOT NULL,
    year        INT         NOT NULL,
    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, year)
);
