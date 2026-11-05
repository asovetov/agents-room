-- agents-room schema, version 1.
--
-- Every participant is a short-lived process that opens this database
-- directly. There is no daemon. All timestamps are Unix seconds.

CREATE TABLE IF NOT EXISTS rooms (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    room_id    TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    harness    TEXT NOT NULL,
    cwd        TEXT NOT NULL,
    pid        INTEGER NOT NULL DEFAULT 0,
    label      TEXT NOT NULL DEFAULT '',
    tags       TEXT NOT NULL DEFAULT '[]',
    autonomy   INTEGER NOT NULL DEFAULT 0,
    joined_at  INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_room ON sessions(room_id);

CREATE TABLE IF NOT EXISTS messages (
    id         TEXT PRIMARY KEY,
    room_id    TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    sender     TEXT NOT NULL,
    selector   TEXT NOT NULL,
    subject    TEXT NOT NULL,
    body       TEXT NOT NULL,
    reply_to   TEXT,
    created_at INTEGER NOT NULL,
    expires_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_messages_room ON messages(room_id, created_at);

-- One row per resolved recipient. State moves forward only:
-- stored -> offered -> accepted -> read -> acked, or -> expired.
CREATE TABLE IF NOT EXISTS deliveries (
    message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    state      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (message_id, session_id)
);

CREATE INDEX IF NOT EXISTS idx_deliveries_session ON deliveries(session_id, state);

-- Leases are keyed by (room, path, owner) so that a shared lease can have
-- several holders. The exclusive check is done in a transaction.
CREATE TABLE IF NOT EXISTS leases (
    room_id     TEXT NOT NULL,
    path        TEXT NOT NULL,
    owner       TEXT NOT NULL,
    mode        TEXT NOT NULL,
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    PRIMARY KEY (room_id, path, owner)
);

CREATE INDEX IF NOT EXISTS idx_leases_room ON leases(room_id);

-- What each session last read, so drift can be detected later.
CREATE TABLE IF NOT EXISTS file_reads (
    room_id    TEXT NOT NULL,
    path       TEXT NOT NULL,
    session_id TEXT NOT NULL,
    hash       TEXT NOT NULL,
    read_at    INTEGER NOT NULL,
    PRIMARY KEY (room_id, path, session_id)
);

-- Append-only audit trail. Nothing updates or deletes rows here.
CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    room_id TEXT NOT NULL,
    ts      INTEGER NOT NULL,
    kind    TEXT NOT NULL,
    payload TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_room ON events(room_id, id);
