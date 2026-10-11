-- Web 管理画面：Discord でログインした利用者が、自分の iPhone（内線）を作成・管理する

CREATE TABLE IF NOT EXISTS users (
    id               TEXT PRIMARY KEY,
    discord_id       TEXT NOT NULL UNIQUE,
    discord_username TEXT NOT NULL DEFAULT '',
    display_name     TEXT NOT NULL DEFAULT '',
    disabled         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at    TIMESTAMPTZ
);

-- owner_user_id：この内線を使う利用者（NULL なら未割り当て）
-- pool：利用者が「iPhoneを追加」したときに割り当ててよい内線
-- label：利用者が付けた iPhone の名前
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS pool BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS label TEXT NOT NULL DEFAULT '';
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS assigned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS extensions_owner ON extensions(owner_user_id);

CREATE TABLE IF NOT EXISTS web_sessions (
    hash               TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf               TEXT NOT NULL,
    expires_at         TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 管理者がサポート用パスコードで利用者の画面を開いているとき、その利用者と期限
    support_user_id    TEXT REFERENCES users(id) ON DELETE SET NULL,
    support_expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS web_sessions_user ON web_sessions(user_id);

-- 利用者が発行するサポート用パスコード（1人につき有効なものは1つ。使うと消える）
CREATE TABLE IF NOT EXISTS support_passcodes (
    user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 利用者の画面に表示する操作記録（サポートでの操作を本人が確認できるように）
CREATE TABLE IF NOT EXISTS audit_log (
    id            BIGSERIAL PRIMARY KEY,
    at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id       TEXT NOT NULL,
    actor_user_id TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_log_user ON audit_log(user_id, at DESC);
