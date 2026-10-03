# MyVoIP-Server

[MikoPBX](https://www.mikopbx.com/) 向け iPhone アプリ「MyVoIP」の着信制御・VoIP Push 送信サービスです。Docker Compose で動きます。

- MikoPBX（Asterisk）の ARI で着信を保持し、APNs の VoIP Push で iPhone を起こしてからつなぐ
- アプリ用の HTTP(S) API（[docs/api.md](docs/api.md)）と管理コマンド（内線登録・登録コード発行・端末管理）
- リバースプロキシは Caddy か Nginx を選べる

iPhone アプリ本体は別リポジトリ（非公開）です。

## 概要

iPhone アプリ（MyVoIP）が休止中でも着信できるように、MikoPBX の着信を受けて VoIP Push を送り、起きた iPhone へつなぐサービスです。MikoPBX とは別の Linux 環境で、Docker Compose で動かします。

**外部に直接公開せず、LAN または VPN 内だけで使う構成**を前提にしており、HTTP や SIP の UDP/TCP（暗号化なし）も選べます。

```
iPhone（LAN / VPN）──HTTP(S)──▶ Caddy または Nginx ─▶ myvoip（Go）─▶ PostgreSQL
   │                                                    │
   │                                                    ├─ARI（LAN 内）──▶ MikoPBX
   │                                                    └─HTTPS（外向き）─▶ Apple APNs
   └──SIP（UDP/TCP/TLS）・RTP/SRTP─────────────────────────────────────▶ MikoPBX
```

> **外出先での着信について**：VoIP Push は Apple 経由で外出先の iPhone にも届きます。しかし、起こされた iPhone がこのサーバーと MikoPBX に届かないと通話がつながりません。携帯回線で着信を受けるには、iPhone で VPN を常時接続（またはオンデマンド接続）にしておく必要があります。

## 1. 前提

| 項目 | 内容 |
|---|---|
| サーバー | Ubuntu Server 24.04 など Docker が動く Linux（1〜2 vCPU・メモリ 2GB 程度）。MikoPBX と同じ物理マシンの別 VM でもよい |
| 到達性 | iPhone（LAN・VPN 内）からこのサーバーの HTTP(S) ポートと MikoPBX の SIP/RTP ポートへ届くこと。このサーバーから MikoPBX の ARI と Apple の APNs（外向き HTTPS 443）へ届くこと |
| APNs 認証キー | Apple Developer の Keys で作成した .p8 と Key ID。環境は「Sandbox & Production」（TestFlight は本番 APNs を使う） |

インターネットからの受信（ポート転送）は不要です。

## 2. MikoPBX の設定

画面名は MikoPBX の版によって異なることがあります。

1. **iPhone 用の内線**を作る（例 `201`）。SIP パスワードを控えておく（後でサーバーに登録する）。
2. **SIP のトランスポート**：既定は UDP 5060（暗号化なし）。TCP・TLS も選べる。TLS を使う場合は、`SIP_DOMAIN` のホスト名と一致する、iPhone が信頼できる証明書が必要（自己署名は接続できない）。
3. **音声の暗号化（SRTP）**：必須ではない。既定の `SIP_SRTP=optional` では、MikoPBX が SRTP に対応していれば暗号化し、対応していなければ暗号化なしで通話する。
4. **ARI ユーザー**を作る（例：ユーザー名 `myvoip`、アプリケーション `myvoip`）。
5. **着信ルート**（ほかの内線・外線からの着信を、このサービスに渡す設定）は、テスト着信には不要です。[6. 着信ルート](#6-着信ルートmikopbx)を参照。

## 3. サーバーの起動

```bash
git clone https://github.com/nemnet-lab/MyVoIP-Server.git
cd MyVoIP-Server
cp .env.example .env
docker compose run --rm --no-deps myvoip gen-secret   # 出力を .env の MYVOIP_SECRET_KEY に貼る
vi .env                   # プロキシ・アドレス・ARI・SIP・APNs の値を記入
mkdir -p secrets
cp ~/AuthKey_XXXXXXXXXX.p8 secrets/apns.p8
chmod 644 secrets/apns.p8   # コンテナ内の非 root ユーザーが読めるようにする
docker compose up -d --build
docker compose logs -f myvoip   # "ARI events connected" が出れば MikoPBX とつながっている
```

### リバースプロキシと HTTPS の選択

`.env` で選びます。

| 変数 | 値 | 内容 |
|---|---|---|
| `COMPOSE_PROFILES` | `caddy` / `nginx` | 起動するリバースプロキシ |
| `MYVOIP_TLS` | `off` | HTTP のみ（`HTTP_PORT`、既定 80）。`MYVOIP_PUBLIC_URL` は `http://…` |
| | `files` | HTTPS（`HTTPS_PORT`、既定 443）。`certs/fullchain.pem` と `certs/privkey.pem` を置く。`MYVOIP_PUBLIC_URL` は `https://…` |

`files` の証明書は、iPhone が信頼できるもの（公的な認証局の証明書、または iPhone に構成プロファイルで信頼させた社内認証局の証明書）にしてください。

### SIP の設定

| 変数 | 既定 | 内容 |
|---|---|---|
| `SIP_DOMAIN` | — | iPhone から届く MikoPBX のアドレス（IP アドレスまたはホスト名） |
| `SIP_TRANSPORT` | `udp` | `udp` / `tcp` / `tls` |
| `SIP_PROXY_PORT` | 5060（tls は 5061） | MikoPBX の SIP ポート |
| `SIP_SRTP` | `optional` | `optional` / `mandatory` / `disabled` |

ARI と内線の確認：

```bash
docker compose exec myvoip myvoip-server check-ari --extension 201
```

## 4. 内線と iPhone の登録

```bash
# MikoPBX の既存内線を登録する（SIP パスワードを聞かれるので入力。コマンド履歴には残らない）
docker compose exec -it myvoip myvoip-server extension set --number 201 --display-name "受付 iPhone"

# 登録コード（10分・1回限り）と QR を発行する
docker compose exec myvoip myvoip-server enroll-code --extension 201
```

表示された **設定URL と登録コード**をアプリの「はじめに」に入力するか、QR を読み取ります。

`extension set` の主なオプション：

| オプション | 既定 | 内容 |
|---|---|---|
| `--sip-user` | 内線番号 | アプリが REGISTER する SIP ユーザー名 |
| `--auth-user` | SIP ユーザー名 | 認証ユーザー名 |
| `--endpoint` | SIP ユーザー名 | 端末へ発呼する PJSIP エンドポイント名 |
| `--ring-timeout` | 30 | 呼出秒数（15〜60） |

## 5. テスト着信

アプリの「設定 → 接続を確認」で確認を実行し、「テスト着信を受ける」を押します。

1. サーバーが VoIP Push を送り、iPhone に着信画面が出る（画面ロック中・アプリ終了後でも）
2. 応答すると、ガイダンス音声（`TEST_CALL_SOUND`）が流れる
3. ビープ音の後に4秒間話すと、その声がそのまま再生される（送話・受話の両方向を確認）
4. 自動で終了する

着信しない・音声が出ないときは [9. トラブルシューティング](#9-トラブルシューティング)へ。

## 6. 着信ルート（MikoPBX）

ほかの内線・外線から iPhone 用の番号への着信を、SIP の登録状態にかかわらず次のダイヤルプランで渡します。

```
Stasis(myvoip,201)
```

- 第2引数はサーバーに登録した内線番号です。
- サーバーは端末へ発呼するとき、ダイヤルプランを通らず `PJSIP/<endpoint>` を直接呼びます。そのため、iPhone 用 SIP 内線への着信が再びこのルートに入るループは起きません。
- MikoPBX の「ダイヤルプランアプリケーション」などの管理画面で保持できる方法で設定してください。生成済みの設定ファイルを直接書き換える方法は、設定の再生成で消えるため使いません。

利用者がダイヤルする番号を `Stasis` に向ける方法は、MikoPBX の版によって選択肢が変わります。版を教えていただければ、具体的な手順に落とし込みます。

## 7. 運用

| 作業 | コマンド |
|---|---|
| ログ | `docker compose logs -f myvoip`（呼ごとの `call_id` で追跡できる） |
| 端末一覧 | `docker compose exec myvoip myvoip-server devices` |
| 端末の失効 | `docker compose exec myvoip myvoip-server revoke-device --device d-xxxx` |
| 更新 | `docker compose up -d --build` |
| バックアップ | `docker compose exec -T db pg_dump -U myvoip myvoip \| gzip > backup-$(date +%F).sql.gz`（`.env` と `secrets/` も別途保管） |

- 履歴は180日を過ぎると自動で削除します。
- `MYVOIP_SECRET_KEY` を失うと、登録済みの SIP パスワードを復号できなくなります。その場合は `extension set` をやり直してください。

## 8. 設定値（.env）

[.env.example](.env.example) を参照してください。

## 開発

```bash
go test -race ./...                 # Go 1.24 以降
docker compose build myvoip         # イメージのビルド
```

| 場所 | 内容 |
|---|---|
| `cmd/myvoip-server` | サーバー本体と管理コマンド |
| `internal/calls` | 呼制御（着信の保持・Push・ready の照合・端末への発呼・ブリッジ・終了処理） |
| `internal/api` | アプリ向け HTTP API と呼状態の WebSocket |
| `internal/ari` | ARI クライアント |
| `internal/apns` | APNs（VoIP Push・一般通知）クライアント |
| `internal/store` | PostgreSQL（試験用のメモリ実装あり） |
| `proxy/` | Caddy・Nginx の設定 |

## 9. トラブルシューティング

| 症状 | 確認すること |
|---|---|
| ログに `ARI events disconnected` | `ARI_URL`・ユーザー・パスワード、MikoPBX の ARI 設定、LAN の到達性 |
| `voip push failed ... InvalidProviderToken` | `APNS_KEY_ID`・`APNS_TEAM_ID`・`secrets/apns.p8` の組み合わせ |
| `voip push failed ... BadDeviceToken` | アプリの APNs 環境とキーの環境の不一致（TestFlight は本番） |
| `registered contact not found` | iPhone が MikoPBX に登録できていない。iPhone が LAN・VPN につながっているか、`SIP_DOMAIN`・`SIP_TRANSPORT`・ポート、内線の SIP パスワード |
| 応答後に無音・片通話 | MikoPBX の RTP ポートへの到達性（VPN の経路・ファイアウォール）、MikoPBX の NAT 設定 |
| `SIP_SRTP=mandatory` で通話できない | MikoPBX 側で SRTP が有効か。不要なら `optional` にする |
| アプリで登録時に通信できない | iPhone から `MYVOIP_PUBLIC_URL` に届くか（LAN・VPN の接続）。初回は iPhone の「ローカルネットワーク」許可を求められるので許可する |
| アプリで「登録コードが無効」 | 10分の期限切れ・使用済み。コードを再発行する |

## ライセンス

[MIT](LICENSE)
