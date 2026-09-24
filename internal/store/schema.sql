-- Phase 2 schema. Applied idempotently on startup.

-- Accounts use a username + password (not email/SSO). Admins provision every
-- account with a random generated password; the user must set their own
-- password on first login (must_reset_password).
CREATE TABLE IF NOT EXISTS users (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    username            TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL DEFAULT '',
    password_hash       TEXT NOT NULL DEFAULT '',
    must_reset_password INTEGER NOT NULL DEFAULT 1, -- 1 = must change password before use
    role                TEXT NOT NULL DEFAULT 'engineering', -- engineering | admin
    status              TEXT NOT NULL DEFAULT 'active',      -- active | disabled
    created_at          TEXT NOT NULL,
    last_login          TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,        -- random token (also the cookie value)
    user_id     INTEGER NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS audit_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       INTEGER REFERENCES users(id),
    actor_username TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL DEFAULT '',
    resource_id   TEXT NOT NULL DEFAULT '',
    metadata      TEXT NOT NULL DEFAULT '', -- JSON blob
    created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at);

-- Phase 3: per-slot bookings/reservations.
CREATE TABLE IF NOT EXISTS reservations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    slot         TEXT NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES users(id),
    username     TEXT NOT NULL DEFAULT '',
    purpose      TEXT NOT NULL DEFAULT '',
    start_time   TEXT NOT NULL,
    end_time     TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active', -- active | released | expired | overridden
    created_at   TEXT NOT NULL,
    released_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_res_slot   ON reservations(slot);
CREATE INDEX IF NOT EXISTS idx_res_status ON reservations(status);
-- Fast lookup for overlap checks on active bookings of a slot.
CREATE INDEX IF NOT EXISTS idx_res_active ON reservations(slot, status, start_time, end_time);

-- Phase 4: deployments triggered via Jenkins.
CREATE TABLE IF NOT EXISTS deployments (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    slot          TEXT NOT NULL,
    service       TEXT NOT NULL,               -- ws | fs | wbo
    ref_type      TEXT NOT NULL,               -- branch | tag
    ref           TEXT NOT NULL,               -- branch name or tag
    user_id       INTEGER NOT NULL REFERENCES users(id),
    username      TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'queued', -- queued|running|success|failed|error
    jenkins_job   TEXT NOT NULL DEFAULT '',
    queue_url     TEXT NOT NULL DEFAULT '',     -- Jenkins queue item API URL
    build_number  INTEGER,                      -- resolved once the build starts
    build_url     TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    started_at    TEXT,
    finished_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_dep_slot    ON deployments(slot);
CREATE INDEX IF NOT EXISTS idx_dep_status  ON deployments(status);
CREATE INDEX IF NOT EXISTS idx_dep_created ON deployments(created_at);

-- Deploy console log lines, streamed to the UI via SSE.
CREATE TABLE IF NOT EXISTS deployment_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    deployment_id INTEGER NOT NULL REFERENCES deployments(id),
    seq           INTEGER NOT NULL,            -- ordering within a deployment
    line          TEXT NOT NULL,
    created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deplog ON deployment_logs(deployment_id, seq);

-- One active deployment per slot: enforces the deploy lock.
CREATE TABLE IF NOT EXISTS slot_locks (
    slot          TEXT PRIMARY KEY,
    deployment_id INTEGER NOT NULL,
    locked_at     TEXT NOT NULL
);

-- Phase 5: per-slot admin settings (visibility/bookability), managed in-app.
CREATE TABLE IF NOT EXISTS slot_settings (
    slot     TEXT PRIMARY KEY,
    hidden   INTEGER NOT NULL DEFAULT 0,  -- hide from dashboard/booking/deploy
    note     TEXT NOT NULL DEFAULT ''
);
