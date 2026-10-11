-- 利用者ごとに自分の PBX（MikoPBX・Asterisk）を持つ構成にする。
-- 中継用 Asterisk は、内線ごとに持ち主の PBX へ登録する。管理者の内線を割り当てる方式（pool）はやめる

-- 利用者の PBX の SIP の接続先（Tailnet のアドレス）。空なら未設定
ALTER TABLE users ADD COLUMN IF NOT EXISTS pbx_host TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS pbx_port INTEGER NOT NULL DEFAULT 5060;

-- pbx_extension：利用者の PBX での内線番号（表示用）。extensions.number はサーバー内の識別子で、
-- 別々の PBX に同じ内線番号があっても重ならないよう、Web で作る内線にはサーバーが番号を振る
ALTER TABLE extensions ADD COLUMN IF NOT EXISTS pbx_extension TEXT NOT NULL DEFAULT '';
ALTER TABLE extensions DROP COLUMN IF EXISTS pool;
