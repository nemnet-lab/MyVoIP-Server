-- 利用者ごとの保留音。利用者が Web 管理画面で登録し、自分の内線に割り当てる。
-- 音声は 8kHz・モノラル・16bit の WAV に変換して保存する（中継用 Asterisk がそのまま再生できる形式）
CREATE TABLE IF NOT EXISTS moh_files (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    duration_ms   INTEGER NOT NULL,
    size_bytes    INTEGER NOT NULL,
    data          BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS moh_files_owner ON moh_files(owner_user_id);

-- 内線に割り当てた保留音（NULL なら中継用 Asterisk の標準の保留音）
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS moh_file_id TEXT REFERENCES moh_files(id) ON DELETE SET NULL;
