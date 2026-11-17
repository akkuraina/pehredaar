-- pehredaar:foreign-keys-off
--
-- A subscriber is now a person, reachable through one or more contacts
-- (a Telegram account, an email address). Until now a subscriber *was* a
-- Telegram account. This splits the two and moves deliveries to contacts,
-- so an alert can go out once per channel. Also adds exam interests (for
-- exams we don't watch yet), one-time tokens (email confirmation, sign-in
-- links) and a small settings table.
--
-- subscribers and deliveries are rebuilt rather than altered because
-- SQLite can't drop a UNIQUE column in place; see noForeignKeysMarker.

CREATE TABLE contacts (
    id            INTEGER PRIMARY KEY,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    channel       TEXT    NOT NULL CHECK (channel IN ('telegram', 'email')),
    -- Telegram: the user id as text. Email: lower-cased address.
    address       TEXT    NOT NULL,
    chat_id       INTEGER,          -- Telegram only
    verified_at   INTEGER,          -- NULL until an email is confirmed
    active        INTEGER NOT NULL DEFAULT 1, -- 0: blocked the bot / unsubscribed / bounced
    created_at    INTEGER NOT NULL,
    UNIQUE (channel, address)
);
CREATE INDEX contacts_subscriber ON contacts (subscriber_id);

-- Existing Telegram users become Telegram contacts (verified by definition).
INSERT INTO contacts (subscriber_id, channel, address, chat_id, verified_at, active, created_at)
SELECT id, 'telegram', CAST(telegram_id AS TEXT), chat_id, created_at, active, created_at
FROM subscribers;

CREATE TABLE subscribers_new (
    id         INTEGER PRIMARY KEY,
    language   TEXT    NOT NULL DEFAULT 'en' CHECK (language IN ('en', 'hi')),
    created_at INTEGER NOT NULL
);
INSERT INTO subscribers_new (id, language, created_at)
SELECT id, language, created_at FROM subscribers;
DROP TABLE subscribers;
ALTER TABLE subscribers_new RENAME TO subscribers;

CREATE TABLE deliveries_new (
    change_id  INTEGER NOT NULL REFERENCES changes (id) ON DELETE CASCADE,
    contact_id INTEGER NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    sent_at    INTEGER NOT NULL,
    PRIMARY KEY (change_id, contact_id)
);
INSERT INTO deliveries_new (change_id, contact_id, sent_at)
SELECT d.change_id, c.id, d.sent_at
FROM deliveries d
JOIN contacts c ON c.subscriber_id = d.subscriber_id AND c.channel = 'telegram';
DROP TABLE deliveries;
ALTER TABLE deliveries_new RENAME TO deliveries;

-- Exams someone wants watched, by exam id from internal/exams (e.g.
-- 'upsc'). Most aren't watched yet; this is how we know whom to tell
-- when they are.
CREATE TABLE interests (
    subscriber_id INTEGER NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    exam          TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (subscriber_id, exam)
);

-- One-time tokens. Only a SHA-256 hash is stored, so a leaked database
-- can't be used to sign in.
CREATE TABLE tokens (
    hash          TEXT    PRIMARY KEY,
    purpose       TEXT    NOT NULL CHECK (purpose IN ('verify-email', 'login', 'link-telegram')),
    subscriber_id INTEGER NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    contact_id    INTEGER REFERENCES contacts (id) ON DELETE CASCADE,
    expires_at    INTEGER NOT NULL,
    used_at       INTEGER,
    created_at    INTEGER NOT NULL
);
CREATE INDEX tokens_expiry ON tokens (expires_at);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
