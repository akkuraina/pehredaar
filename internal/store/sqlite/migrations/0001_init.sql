-- Initial schema.
--
-- Timestamps are INTEGER Unix milliseconds (UTC). SQLite has no real
-- datetime type, and integers sort and compare correctly without any
-- string-format pitfalls. Durations are INTEGER seconds.

CREATE TABLE sources (
    id                   INTEGER PRIMARY KEY,
    url                  TEXT    NOT NULL UNIQUE,
    kind                 TEXT    NOT NULL CHECK (kind IN ('feed', 'pdf', 'html')),
    category             TEXT    NOT NULL,
    language             TEXT    NOT NULL CHECK (language IN ('en', 'hi')),
    label                TEXT    NOT NULL DEFAULT '',
    parent_id            INTEGER REFERENCES sources (id) ON DELETE CASCADE,
    check_interval_sec   INTEGER NOT NULL CHECK (check_interval_sec > 0),
    enabled              INTEGER NOT NULL DEFAULT 1,
    etag                 TEXT    NOT NULL DEFAULT '',
    last_modified        TEXT    NOT NULL DEFAULT '',
    last_checked_at      INTEGER,
    next_check_at        INTEGER,          -- NULL means due now
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL
);

CREATE INDEX sources_due ON sources (enabled, next_check_at);
CREATE INDEX sources_category ON sources (category);
CREATE INDEX sources_parent ON sources (parent_id);

CREATE TABLE snapshots (
    id           INTEGER PRIMARY KEY,
    source_id    INTEGER NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    content_hash TEXT    NOT NULL,
    text         TEXT    NOT NULL,
    fetched_at   INTEGER NOT NULL
);

-- "Latest snapshot" is ordered by id rather than fetched_at: ids only ever
-- grow, so this is immune to clock skew and identical timestamps.
CREATE INDEX snapshots_source_latest ON snapshots (source_id, id DESC);

CREATE TABLE changes (
    id              INTEGER PRIMARY KEY,
    source_id       INTEGER NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    old_snapshot_id INTEGER REFERENCES snapshots (id) ON DELETE CASCADE,
    new_snapshot_id INTEGER NOT NULL REFERENCES snapshots (id) ON DELETE CASCADE,
    kind            TEXT    NOT NULL CHECK (kind IN ('added', 'edited', 'removed')),
    diff            TEXT    NOT NULL,
    importance      TEXT    NOT NULL DEFAULT ''
                    CHECK (importance IN ('', 'date', 'eligibility', 'fee', 'other', 'cosmetic')),
    status          TEXT    NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'summarized', 'notified', 'suppressed', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL
);

CREATE INDEX changes_status ON changes (status, id);

CREATE TABLE change_summaries (
    change_id  INTEGER NOT NULL REFERENCES changes (id) ON DELETE CASCADE,
    language   TEXT    NOT NULL CHECK (language IN ('en', 'hi')),
    text       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (change_id, language)
);

CREATE TABLE subscribers (
    id          INTEGER PRIMARY KEY,
    telegram_id INTEGER NOT NULL UNIQUE,
    chat_id     INTEGER NOT NULL,
    language    TEXT    NOT NULL DEFAULT 'en' CHECK (language IN ('en', 'hi')),
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL
);

CREATE TABLE subscriptions (
    id            INTEGER PRIMARY KEY,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    source_id     INTEGER REFERENCES sources (id) ON DELETE CASCADE,
    category      TEXT,
    created_at    INTEGER NOT NULL,
    CHECK ((source_id IS NULL) <> (category IS NULL))
);

-- A plain UNIQUE (subscriber_id, source_id, category) would not work: SQL
-- treats NULLs as distinct, so duplicate category subscriptions (source_id
-- NULL) would slip through. Partial indexes enforce uniqueness per kind.
CREATE UNIQUE INDEX subscriptions_unique_source
    ON subscriptions (subscriber_id, source_id) WHERE source_id IS NOT NULL;
CREATE UNIQUE INDEX subscriptions_unique_category
    ON subscriptions (subscriber_id, category) WHERE category IS NOT NULL;
CREATE INDEX subscriptions_category ON subscriptions (category);
CREATE INDEX subscriptions_source ON subscriptions (source_id);

CREATE TABLE deliveries (
    change_id     INTEGER NOT NULL REFERENCES changes (id) ON DELETE CASCADE,
    subscriber_id INTEGER NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    sent_at       INTEGER NOT NULL,
    PRIMARY KEY (change_id, subscriber_id)
);
