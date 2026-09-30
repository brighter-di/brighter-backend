-- The first import asked the price source for windows ending in the past,
-- which it cuts to the last twelve months, and stored those partial years as
-- complete. Start over; the warmup refetches every year whole.
DELETE FROM close_years;
DELETE FROM daily_closes;
