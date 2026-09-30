-- Telegram chats that pressed Start on the bot. They get every event without
-- portfolio ids or wallet addresses; /stop, or blocking the bot, removes them.
CREATE TABLE telegram_subscribers (
    chat_id       BIGINT      PRIMARY KEY,
    subscribed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
