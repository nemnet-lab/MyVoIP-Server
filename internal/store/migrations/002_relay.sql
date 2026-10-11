-- 中継モード：iPhone が中継用 Asterisk に登録するための SIP パスワード（MikoPBX のパスワードとは別）
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS mobile_password_enc BYTEA;
