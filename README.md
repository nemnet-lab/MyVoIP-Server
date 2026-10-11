# MyVoIP-Server

[MikoPBX](https://www.mikopbx.com/) 向け iPhone アプリ「MyVoIP」の着信制御・VoIP Push 送信サービスです。Docker Compose で動きます。

- MikoPBX（Asterisk）の ARI で着信を保持し、APNs の VoIP Push で iPhone を起こしてからつなぐ
- アプリ用の HTTP(S) API（[docs/api.md](docs/api.md)）と管理コマンド（内線登録・登録コード発行・端末管理）
- Discord でログインする Web 管理画面（利用者がそれぞれ自分の PBX を設定し、iPhone を作成して登録コードを発行する・サポート用パスコード）
- リバースプロキシは Caddy か Nginx を選べる

iPhone アプリ本体は別リポジトリ（非公開）です。

## 概要

iPhone アプリ（MyVoIP）が休止中でも着信できるように、着信を受けて VoIP Push を送り、起きた iPhone へつなぐサービスです。MikoPBX とは別の Linux 環境で、Docker Compose で動かします。

**外部に直接公開せず、LAN または VPN（Tailscale など）内だけで使う構成**を前提にしており、HTTP や SIP の UDP（暗号化なし）も選べます。

### 2つの構成

| | 中継モード（推奨） | 直接モード |
|---|---|---|
| MikoPBX から見た内線 | **このサーバーが内線として常時登録**（常に online） | iPhone が直接登録（休止中は offline） |
| MikoPBX の変更 | **なし**（既存の内線と SIP パスワードを使うだけ） | ARI の有効化と、内線への着信を Stasis に渡す設定が必要 |
| iPhone の登録先 | このサーバー内の中継用 Asterisk（パスワードはサーバーが発行） | MikoPBX |
| iPhone からの発信 | 中継用 Asterisk が、その内線からの発信として MikoPBX へ送る | MikoPBX へ直接 |

```
中継モード
                    ┌──────────── このサーバー ────────────┐
iPhone ──SIP/RTP──▶ │ 中継用 Asterisk ──内線として登録──▶ │ ──SIP/RTP──▶ MikoPBX
   │                │   │ ARI                              │
   └──HTTP(S)─────▶ │ Caddy/Nginx ─▶ myvoip（Go）─▶ PostgreSQL │ ──HTTPS──▶ Apple APNs
                    └──────────────────────────────────────┘
```

- 着信：MikoPBX が内線（例 9002）を呼ぶ → 中継用 Asterisk が受けて呼制御（Stasis）に渡す → VoIP Push で iPhone を起こす → iPhone が中継用 Asterisk に登録して ready を送る → その登録先を呼んで発信側とブリッジする。
- 発信：iPhone → 中継用 Asterisk → MikoPBX（From・認証ともにその内線）。

> **外出先での着信について**：VoIP Push は Apple 経由で外出先の iPhone にも届きます。しかし、起こされた iPhone がこのサーバーに届かないと通話がつながりません。携帯回線で着信を受けるには、iPhone で VPN を常時接続（またはオンデマンド接続）にしておく必要があります。

## 1. 前提

| 項目 | 内容 |
|---|---|
| サーバー | Ubuntu Server 24.04 など Docker が動く Linux（1〜2 vCPU・メモリ 2GB 程度）。MikoPBX と同じ物理マシンの別 VM でもよい |
| 到達性 | iPhone（LAN・VPN 内）からこのサーバーの HTTP(S) ポートと MikoPBX の SIP/RTP ポートへ届くこと。このサーバーから MikoPBX の ARI と Apple の APNs（外向き HTTPS 443）へ届くこと |
| APNs 認証キー | Apple Developer の Keys で作成した .p8 と Key ID。環境は「Sandbox & Production」（TestFlight は本番 APNs を使う） |

インターネットからの受信（ポート転送）は不要です。

## 2. MikoPBX の設定

### 中継モード

MikoPBX の設定変更は不要です。

1. iPhone 用の内線（例 `9002`）を用意し、SIP パスワードを控えておく（後でこのサーバーに登録する）。
2. このサーバーから MikoPBX の SIP（例 UDP 5060）と RTP の範囲に届き、MikoPBX からこのサーバーの SIP（UDP 5060）と RTP（10000〜10999）に届くこと。
3. その内線の SIP の登録数（max_contacts）が1以上あること。iPhone を以前 MikoPBX に直接登録していた場合は、古い登録が残っていないこと。

### 直接モード

1. iPhone 用の内線を作り、SIP パスワードを控えておく。
2. SIP のトランスポート（UDP 5060 など）と、必要なら SRTP を設定する。
3. ARI を有効にし、ARI ユーザー（例 `myvoip`）を作る（[MikoPBX 2024.1 での ARI の有効化](#mikopbx-20241-での-ari-の有効化)）。
4. 内線への着信を `Stasis(myvoip,<内線>)` に渡す（[6. 着信ルート](#6-着信ルートmikopbx)。テスト着信だけなら不要）。

## 3. サーバーの起動

```bash
git clone https://github.com/nemnet-lab/MyVoIP-Server.git
cd MyVoIP-Server
cp .env.example .env
mkdir -p secrets            # 先に作る（compose に作らせると root の持ち物になる）
docker compose run --rm --no-deps myvoip gen-secret   # 出力を .env の MYVOIP_SECRET_KEY に貼る
vi .env                   # プロキシ・アドレス・ARI・SIP・APNs の値を記入
cp ~/AuthKey_XXXXXXXXXX.p8 secrets/apns.p8
chmod 644 secrets/apns.p8   # コンテナ内の非 root ユーザーが読めるようにする
docker compose up -d --build
docker compose logs -f myvoip   # "ARI events connected" が出れば MikoPBX とつながっている
```

### 中継モードの設定

`.env` で次のように設定します（[.env.example](.env.example) に既定値があります）。

| 変数 | 例 | 内容 |
|---|---|---|
| `COMPOSE_PROFILES` | `caddy,relay` | `relay` を加えると中継用 Asterisk が起動する |
| `RELAY_ENABLED` | `true` | 中継モードにする |
| `UPSTREAM_SIP_HOST` / `UPSTREAM_SIP_PORT` | `100.64.0.10` / `5060` | 既定の PBX の SIP の接続先（このサーバーから見たアドレス）。管理コマンドで登録した内線の登録先。Web 管理画面で利用者が自分の PBX を設定するなら空でもよい |
| `SIP_DOMAIN` | `100.64.0.20` | **このサーバー**のアドレス（iPhone から届くもの）。iPhone はここに登録する |
| `ARI_URL` | `http://10.211.0.1:8088/ari` | 中継用 Asterisk の ARI（`MYVOIP_DOCKER_GATEWAY` のアドレス） |
| `ARI_USER` / `ARI_PASSWORD` | `myvoip` / 任意の長い文字列 | 中継用 Asterisk の ARI ユーザー（MikoPBX の ARI とは無関係） |
| `MYVOIP_DOCKER_SUBNET` / `MYVOIP_DOCKER_GATEWAY` | `10.211.0.0/24` / `10.211.0.1` | Compose のネットワーク。ARI はこのゲートウェイだけで待ち受ける。LAN と重ならない範囲にする |
| `RELAY_SIP_PORT`、`RELAY_RTP_START`〜`RELAY_RTP_END` | `5060`、`10000`〜`10999` | 中継用 Asterisk の SIP と RTP（host ネットワークで待ち受ける） |

- 中継用 Asterisk は host ネットワークで動き、ホストの UDP 5060 と RTP の範囲を使います。ほかのプロセスが使っていないことを確認してください。
- `MYVOIP_ALLOWED_CLIENT_CIDRS` を設定すると、API に加えて、iPhone 用の SIP の受け口にも同じ制限がかかります。
- 内線を登録（`extension set`）すると、中継用 Asterisk の設定が書き出され、すぐに MikoPBX への登録が始まります。手動で反映し直すときは `docker compose exec myvoip myvoip-server relay sync`。
- 中継モードでは iPhone に MikoPBX のパスワードを渡しません。サーバーが iPhone 用のパスワードを別に発行します。

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

`online`（到達可能な登録あり）か `offline`（登録なし。アプリ休止中は正常）が表示されます。エンドポイント名が違うとエラーになります。

### Tailscale（Tailnet）だけで使う構成

iPhone・このサーバー・MikoPBX をすべて同じ tailnet に入れ、Tailnet のアドレス（100.64.0.0/10）だけでやり取りする構成です。社内でも外出先でも同じ設定で使えます。

| 経路 | 設定 |
|---|---|
| iPhone → このサーバー（API） | `MYVOIP_PUBLIC_URL=http://<このサーバーの Tailnet アドレス>` |
| iPhone → MikoPBX（SIP・RTP） | `SIP_DOMAIN=<MikoPBX の Tailnet アドレス>` |
| このサーバー → MikoPBX（ARI） | `ARI_URL=http://<MikoPBX の Tailnet アドレス>:8088/asterisk/ari` |
| Tailnet 以外からの API を拒否 | `MYVOIP_ALLOWED_CLIENT_CIDRS=100.64.0.0/10` |

- このサーバーのホストに Tailscale を入れて tailnet に参加させます（`tailscale up`）。Docker のポートは全インターフェースで待ち受けますが、`MYVOIP_ALLOWED_CLIENT_CIDRS` で Tailnet 以外からの API 要求を拒否します。
- iPhone に Tailscale を入れ、**VPN オンデマンド**を有効にします。着信 Push でアプリが起きたときに VPN がつながっていないと、通話できません。
- Tailscale の ACL で、iPhone → このサーバーの HTTP（80 または 443）、iPhone → MikoPBX の SIP（5060/UDP など）と RTP の範囲、このサーバー → MikoPBX の 8088/TCP を許可します。
- **MikoPBX 側の注意**：Tailnet のクライアントに対して、MikoPBX が SIP の Contact・Via や SDP の音声アドレス（`c=`）に **Tailnet のアドレス**を使う必要があります。LAN のアドレスを返すと、無音になったり切断できなくなったりします。Asterisk の PJSIP では、トランスポートの `local_net`（LAN のアドレス帯）と `external_signaling_address` / `external_media_address`（MikoPBX の Tailnet アドレス）で調整します。

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

### Web 管理画面（Discord でログイン）

このサーバー（Apple の VoIP Push の証明書を持つサーバー）を複数の人で共用し、**利用者はそれぞれ自分の MikoPBX・Asterisk を持つ**構成のための画面です。利用者は自分で PBX の接続先を設定し、iPhone を作成して登録コードを発行します。中継用 Asterisk は、iPhone ごとの内線として、その利用者の PBX に登録します。

```
利用者 A の iPhone ─┐                     ┌─内線として登録─▶ 利用者 A の PBX（100.x.y.a）
利用者 B の iPhone ─┼─▶ このサーバー（共用）─┼─内線として登録─▶ 利用者 B の PBX（100.x.y.b）
                    └── VoIP Push（Apple） └─ …
```

画面は API と同じアドレス（`MYVOIP_PUBLIC_URL`）で開き、`MYVOIP_ALLOWED_CLIENT_CIDRS` の制限も同じく受けます。

| 利用者ができること | 内容 |
|---|---|
| PBX の設定 | 自分の PBX の SIP の接続先（Tailnet の IP アドレスとポート）。`MYVOIP_ALLOWED_PBX_CIDRS`（既定は Tailnet の `100.64.0.0/10` と `fd7a:115c:a1e0::/48`）の IP アドレスだけを受け付ける |
| iPhone を追加 | 自分の PBX で作った内線の番号と SIP パスワード（必要なら SIP ユーザー名・認証ユーザー名）を入力する。1つの内線で使える iPhone は1台。上限は `MYVOIP_MAX_PHONES_PER_USER`（既定 3） |
| 登録コードを発行 | アプリに入力する設定 URL・登録コード（10分・1回限り）と QR を表示する。機種変更のときも再発行する |
| 内線の設定の変更・削除 | SIP の設定の変更（パスワードは変えるときだけ入力）。削除すると端末・履歴・登録コードも消え、PBX への登録もやめる |
| PBX への接続状態 | 中継用 Asterisk から PBX に届いているか（30秒ごとの OPTIONS）を iPhone ごとに表示する |
| サポート用パスコード | 数字8桁（`SUPPORT_PASSCODE_MINUTES`、既定30分・1回限り）。管理者に伝えると、管理者が `SUPPORT_SESSION_MINUTES`（既定60分）だけ利用者の画面を開いて、PBX や iPhone の設定を代わりに操作できる。管理者の操作は利用者の画面の「操作の記録」に表示される |

管理者（`MYVOIP_ADMIN_DISCORD_IDS`）は「管理」画面で、サポートの開始、利用者のログイン停止、管理コマンドで登録した内線（例 9002）の利用者への割り当て・解除ができます。

- 別々の PBX に同じ内線番号（例 201）があっても使えます。サーバーは内線ごとに `mv` で始まる識別子を振り、中継用 Asterisk のエンドポイント名と iPhone の SIP ユーザー名に使います。アプリには PBX での内線番号を表示します。
- PBX からの着信は、登録時の `line` パラメーターだけでどの内線かを見分けます（PBX 側のエンドポイントは `identify_by=ip`）。ほかの利用者が SIP のユーザー名をまねて、別の利用者の iPhone に着信させることはできません。
- 1人の利用者の設定に誤りがあっても、ほかの利用者の内線は登録されます（その内線だけを書かずに、ログに `relay: extension not registered to PBX` を出します）。

**利用者の PBX に必要な設定**

- このサーバーの Tailnet のアドレスからの SIP（UDP、既定 5060）の登録と、音声（RTP。中継用 Asterisk の `RELAY_RTP_START`〜`RELAY_RTP_END`）を受け付ける。MikoPBX はファイアウォールや「信頼するネットワーク」の設定を確認する。
- コーデックに G.711（alaw・ulaw）を含める。
- PBX が LAN にある場合、Tailnet の相手に対して SIP の Contact や SDP の音声アドレスに Tailnet のアドレスを使うようにする（[Tailscale だけで使う構成](#tailscaletailnetだけで使う構成)の「MikoPBX 側の注意」と同じ）。

**Tailnet の構成**

- このサーバー・利用者の PBX・利用者の iPhone が同じ tailnet にいれば、追加の設定は要りません（ACL で、iPhone → このサーバーの HTTP と SIP・RTP、このサーバー ↔ PBX の SIP・RTP を許可します）。
- 利用者がそれぞれ別の tailnet を使う場合は、Tailscale のマシンの共有（node sharing）で、このサーバーを利用者の tailnet に、利用者の PBX をこのサーバーの tailnet に共有します。共有されたマシンからは通信を始められないため、PBX からこのサーバーへの着信の INVITE は、このサーバーからの登録・OPTIONS（30秒ごと）で開いた経路を使って届きます。共有の構成では、着信と音声を実機で確かめてから使ってください。

**設定手順**

1. [Discord Developer Portal](https://discord.com/developers/applications) でアプリケーションを作成し、「OAuth2」で Client ID と Client Secret を控える。
2. 同じ画面の Redirects に `MYVOIP_PUBLIC_URL` + `/auth/discord/callback`（例 `http://100.x.y.z/auth/discord/callback`）を登録する。認可コードの交換はこのサーバーから Discord へ送るため、Discord からこのサーバーへ届く必要はありません（Tailnet 内だけでも動きます）。
   - http のアドレスを Discord が受け付けない場合は、Tailscale の MagicDNS 名で HTTPS にします（`tailscale cert <名前>.<tailnet>.ts.net` で証明書を作って `certs/` に置き、`MYVOIP_TLS=files`、`MYVOIP_PUBLIC_URL=https://<名前>.<tailnet>.ts.net`）。
3. `.env` に `DISCORD_CLIENT_ID`・`DISCORD_CLIENT_SECRET`・`MYVOIP_ADMIN_DISCORD_IDS`（自分の Discord ユーザー ID。Discord の設定で開発者モードを有効にすると、プロフィールからコピーできる）を設定する。
4. ログインできる人を決める。`DISCORD_GUILD_ID` を指定するとその Discord サーバーのメンバーだけ、さらに `DISCORD_ROLE_IDS` を指定するとそのロールを持つメンバーだけがログインできる。個別に許可する人は `DISCORD_ALLOWED_USER_IDS`。どれも指定しなければ管理者だけがログインできる。
5. `docker compose up -d --build --no-deps myvoip` で反映し、`MYVOIP_PUBLIC_URL` をブラウザで開く。

**管理コマンドで登録済みの内線（例 9002）を Web に移す**：管理者が自分も Discord でログインして「PBX の設定」を保存し、管理画面で 9002 を自分に割り当てます。割り当てた内線は、利用者の PBX の設定があればそちらへ、なければ既定の PBX（`UPSTREAM_SIP_HOST`）へ登録します。

> **直接モードでの注意**：直接モード（中継用 Asterisk を使わない構成）では、iPhone が MikoPBX に直接登録するため、利用者ごとの PBX には対応しません。Web 管理画面は中継モードで使ってください。

## 5. テスト着信

アプリの「設定 → 接続を確認」で確認を実行し、「テスト着信を受ける」を押します。

1. サーバーが VoIP Push を送り、iPhone に着信画面が出る（画面ロック中・アプリ終了後でも）
2. 応答すると、ガイダンス音声（`TEST_CALL_SOUND`、既定は `sound:vm-intro`）が流れる。MikoPBX にない音声を指定すると何も流れず、ログに `playback failed` が出る（15秒後に次へ進む）
3. ビープ音の後に4秒間話すと、その声がそのまま再生される（送話・受話の両方向を確認）
4. 自動で終了する

声の折り返し（手順3）には、Asterisk の ARI 録音の保存先 `<astspooldir>/recording` が必要です。ない場合はガイダンスのあと少し待って終了します（受話の確認はできますが、送話は確認できません）。MikoPBX 2024.1 では `astspooldir` が `/storage/usbdisk1/mikopbx/astspool` などになっているので、その下に `recording` ディレクトリを作ってください。テストの録音はサーバーが終了時に削除します。

着信しない・音声が出ないときは [9. トラブルシューティング](#9-トラブルシューティング)へ。

## MikoPBX 2024.1 での ARI の有効化

MikoPBX 2024.1 の画面には ARI を有効にする項目がないため、次のように設定します（2024.1.114 で確認）。

- 「システムファイルのカスタマイズ」で `modules.conf` を **script モード**にし、`res_ari` などの load 行と `ari.conf`（ユーザー `myvoip`）を生成する。設定の再生成や core reload の後も保たれる。
- この項目を画面から保存すると **core restart になり、全通話が切れる**。変更は通話のない時間に行う。
- ARI の URL は `http://<MikoPBX>:8088/asterisk/ari`（WebSocket は `/asterisk/ari/events`）。
- Asterisk の ARI ユーザーはアプリケーション名ごとの制限を持たないため、`ARI_APP=myvoip` のままでよい。

## Docker の既定のアドレス帯との衝突

Docker は既定で `172.17.0.0/16`・`172.18.0.0/16` などをコンテナ用に使います。LAN が同じアドレス帯だと、Docker が動いているホスト（このサーバー・MikoPBX のホスト）からその LAN へ届かなくなります。LAN と重なる場合は、`/etc/docker/daemon.json` の `bip` と `default-address-pools` を、使っていないアドレス帯（例 `10.200.0.0/16`）に変えてください。変更には Docker の再起動が必要です（MikoPBX を Docker で動かしている場合は通話が切れます）。

## 6. 着信ルート（MikoPBX）

ほかの内線・外線から iPhone 用の番号への着信を、SIP の登録状態にかかわらず次のダイヤルプランで渡します。

```
Stasis(myvoip,201)
```

- 第2引数はサーバーに登録した内線番号です。
- サーバーは端末へ発呼するとき、ダイヤルプランを通らず `PJSIP/<endpoint>` を直接呼びます。そのため、iPhone 用 SIP 内線への着信が再びこのルートに入るループは起きません。
- MikoPBX の「ダイヤルプランアプリケーション」などの管理画面で保持できる方法で設定してください。生成済みの設定ファイルを直接書き換える方法は、設定の再生成で消えるため使いません。

利用者がダイヤルする番号を `Stasis` に向ける方法の例（MikoPBX 2024.1）：

- **別の番号から渡す**：ダイヤルプランアプリケーションで、別の番号（例 `7201`）に `Stasis(myvoip,201)` を書く。保存しても dialplan reload だけで済む。内線番号そのものへの着信は iPhone に届かない（アプリは相関ヘッダーのない INVITE を拒否する）。
- **内線番号への着信を渡す**：MikoPBX が生成する `[internal-users]` は、Dial の前に `internal-users-custom` コンテキストに同じ番号があればそちらへ Gosub する。「システムファイルのカスタマイズ」で `/etc/asterisk/extensions.conf` に次を追記する（append モード。保存時は reload だけで通話は切れない）。不在時の転送やボイスメールは Stasis 側で扱うことになる。

  ```
  [internal-users-custom]
  exten => 201,1,Stasis(myvoip,201)
   same => n,Hangup()
  ```

### 内線の登録数の設定

アプリは SIP 接続を張り直すたびに新しい Contact（接続世代 `myvoip-reg=…` 付き）で登録します。サーバーは ARI で PJSIP エンドポイントのイベント（`ContactStatusChange`）を購読して登録先を把握し、ready のときに今回の Contact だけを直接呼ぶ（`PJSIP/<内線>/sip:…`）ため、古い Contact が残っていても誤った宛先には発呼しません。実際の着信では、発信側のチャネル上で `PJSIP_DIAL_CONTACTS` も評価します（ARI のグローバル変数の取得は関数を評価しないため、チャネル経由で読む）。それでも、MyVoIP 専用の内線は `max_contacts=1`・`remove_existing=yes` にしておくと、MikoPBX 側の Contact の一覧が分かりやすくなります。

## 7. 運用

| 作業 | コマンド |
|---|---|
| ログ | `docker compose logs -f myvoip`（呼ごとの `call_id` で追跡できる） |
| 端末一覧 | `docker compose exec myvoip myvoip-server devices` |
| 端末の失効 | `docker compose exec myvoip myvoip-server revoke-device --device d-xxxx` |
| 更新 | `docker compose up -d --build`（すべて）。myvoip だけなら `docker compose up -d --build --no-deps myvoip`（中継用 Asterisk を再起動しない。中継用 Asterisk の再起動は通話が切れるので、通話のない時間に行う） |
| 中継用 Asterisk の設定を反映し直す | `docker compose exec myvoip myvoip-server relay sync`（通話は切れない） |
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
| `asterisk/` | 中継用 Asterisk のイメージと設定のひな形 |
| `internal/relay` | 中継用 Asterisk の内線ごとの設定の生成 |
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
| `registered contact not found` | iPhone が MikoPBX に登録できていない。iPhone が LAN・VPN につながっているか、`SIP_DOMAIN`・`SIP_TRANSPORT`・ポート、内線の SIP パスワード。登録はできているのに出る場合は、ログに `ARI subscribed source=endpoint:PJSIP` と `contact status` が出ているか |
| 応答後に無音・片通話 | MikoPBX の RTP ポートへの到達性（VPN の経路・ファイアウォール）、MikoPBX の NAT 設定 |
| テスト着信でガイダンスのあと声が返らずに終わる | ログに `test recording failed`。MikoPBX の `<astspooldir>/recording` を作る |
| `SIP_SRTP=mandatory` で通話できない | MikoPBX 側で SRTP が有効か。不要なら `optional` にする |
| アプリで登録時に通信できない | iPhone から `MYVOIP_PUBLIC_URL` に届くか（LAN・VPN の接続）。初回は iPhone の「ローカルネットワーク」許可を求められるので許可する |
| API が 403 `client_not_allowed` | `MYVOIP_ALLOWED_CLIENT_CIDRS` の範囲外から接続している（ログの `client` を確認）。Tailnet 経由で接続しているか |
| ARI に接続できない（404） | 直接モードの MikoPBX 2024.1 は `ARI_URL` に接頭辞 `/asterisk` が必要 |
| 中継モードで MikoPBX に登録できない | `docker compose exec asterisk asterisk -rx "pjsip show registrations"`。`UPSTREAM_SIP_HOST`・内線の SIP パスワード・MikoPBX への到達性 |
| 中継モードで ARI に接続できない | `ARI_URL` が `MYVOIP_DOCKER_GATEWAY` のアドレスか。Compose のネットワークを変えたら `docker compose down` してから `up` する |
| アプリで「登録コードが無効」 | 10分の期限切れ・使用済み。コードを再発行する |

## ライセンス

[MIT](LICENSE)
