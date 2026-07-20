-- SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
-- SPDX-License-Identifier: AGPL-3.0-only

-- Schema for the standalone hub's sqlite state file. Applied idempotently at
-- every startup. All timestamps are unix milliseconds.

CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY, -- base62 id part of the encoded key
    secret_hash TEXT NOT NULL, -- hex sha256 of the secret bytes
    name TEXT,
    created_at_millis INTEGER NOT NULL,
    revoked_at_millis INTEGER
);

CREATE TABLE IF NOT EXISTS connectivity_groups (
    id TEXT PRIMARY KEY, -- UUID
    name TEXT,
    association_key TEXT,
    next_node_number INTEGER NOT NULL DEFAULT 1,
    roster BLOB, -- serialized roster proto, NULL = empty
    roster_version INTEGER NOT NULL DEFAULT 0,
    created_at_millis INTEGER NOT NULL,
    deleted_at_millis INTEGER
);

-- Association keys are unique among live groups only, so a key can be reused
-- after its group is deleted (matches the hosted per-domain semantics).
CREATE UNIQUE INDEX IF NOT EXISTS idx_cg_association_key
ON connectivity_groups(association_key)
WHERE association_key IS NOT NULL AND deleted_at_millis IS NULL;

CREATE TABLE IF NOT EXISTS nodes (
    connectivity_group_id TEXT NOT NULL REFERENCES connectivity_groups(id),
    node_number INTEGER NOT NULL,
    name TEXT,
    auth_token_hash TEXT NOT NULL,
    metadata TEXT,
    last_seen_at_millis INTEGER,
    created_at_millis INTEGER NOT NULL,
    deleted_at_millis INTEGER,
    PRIMARY KEY (connectivity_group_id, node_number)
);

CREATE TABLE IF NOT EXISTS node_registration_tokens (
    id TEXT PRIMARY KEY, -- UUID
    connectivity_group_id TEXT NOT NULL REFERENCES connectivity_groups(id),
    token_hash TEXT NOT NULL, -- hex sha256 of the token (or codehalf|token)
    node_name TEXT,
    node_metadata TEXT,
    created_at_millis INTEGER NOT NULL,
    expires_at_millis INTEGER NOT NULL,
    used_at_millis INTEGER
);

CREATE INDEX IF NOT EXISTS idx_nrt_token_hash
ON node_registration_tokens(token_hash);

CREATE TABLE IF NOT EXISTS node_activity_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    connectivity_group_id TEXT NOT NULL,
    node_number INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    success INTEGER NOT NULL,
    details BLOB, -- serialized bepb.EventDetails
    created_at_millis INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_nal_cg_created
ON node_activity_log(connectivity_group_id, created_at_millis);
