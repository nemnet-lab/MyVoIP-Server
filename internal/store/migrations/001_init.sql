CREATE TABLE IF NOT EXISTS extensions (
    number               TEXT PRIMARY KEY,
    display_name         TEXT NOT NULL DEFAULT '',
    sip_username         TEXT NOT NULL,
    sip_auth_username    TEXT NOT NULL DEFAULT '',
    sip_password_enc     BYTEA NOT NULL,
    endpoint             TEXT NOT NULL,
    ring_timeout_seconds INTEGER NOT NULL DEFAULT 30,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS enrollment_codes (
    code_hash  TEXT PRIMARY KEY,
    extension  TEXT NOT NULL REFERENCES extensions(number) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS devices (
    id               TEXT PRIMARY KEY,
    extension        TEXT NOT NULL REFERENCES extensions(number) ON DELETE CASCADE,
    name             TEXT NOT NULL DEFAULT '',
    platform         TEXT NOT NULL DEFAULT 'ios',
    app_version      TEXT NOT NULL DEFAULT '',
    push_environment TEXT NOT NULL DEFAULT 'production',
    voip_token       TEXT NOT NULL DEFAULT '',
    alert_token      TEXT NOT NULL DEFAULT '',
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at       TIMESTAMPTZ
);
-- 1内線につき有効な端末は1台（01 §2）
CREATE UNIQUE INDEX IF NOT EXISTS devices_one_active_per_extension ON devices(extension) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS tokens (
    hash       TEXT PRIMARY KEY,
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS tokens_device ON tokens(device_id);

CREATE TABLE IF NOT EXISTS calls (
    id             TEXT PRIMARY KEY,
    device_id      TEXT NOT NULL,
    extension      TEXT NOT NULL,
    caller_number  TEXT NOT NULL DEFAULT '',
    caller_name    TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL,
    end_reason     TEXT NOT NULL DEFAULT '',
    is_test        BOOLEAN NOT NULL DEFAULT FALSE,
    caller_channel TEXT NOT NULL DEFAULT '',
    device_channel TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    answered_at    TIMESTAMPTZ,
    ended_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS calls_unfinished ON calls(state) WHERE state <> 'ended';

CREATE SEQUENCE IF NOT EXISTS history_seq;
CREATE TABLE IF NOT EXISTS history (
    id               TEXT PRIMARY KEY,
    extension        TEXT NOT NULL,
    call_id          TEXT NOT NULL DEFAULT '',
    direction        TEXT NOT NULL,
    result           TEXT NOT NULL,
    remote_number    TEXT NOT NULL DEFAULT '',
    remote_name      TEXT NOT NULL DEFAULT '',
    started_at       TIMESTAMPTZ NOT NULL,
    duration_seconds INTEGER,
    detail           TEXT NOT NULL DEFAULT '',
    deleted_at       TIMESTAMPTZ,
    seq              BIGINT NOT NULL DEFAULT nextval('history_seq')
);
CREATE INDEX IF NOT EXISTS history_feed ON history(extension, seq);
