# 着信制御サービス API 契約（iOS アプリ v0.1）

版：0.1／作成日：2026-10-03

iPhone アプリ（MyVoIP）と着信制御サービスの間の API。サーバーはこの契約に従って実装している。

## 共通

- ベース URL：利用者が初期設定で入力する設定 URL（例 `http://192.168.1.20`、`https://voip.example.lan`、パス接頭辞可）。外部に公開しない LAN・VPN 内での利用を前提に、HTTP も許可する。WebSocket は http なら `ws`、https なら `wss`。
- JSON は snake_case。日時は UNIX 秒（小数可）。
- 認証：`Authorization: Bearer <access_token>`。401 を返すとアプリは `POST /v1/auth/refresh` で1回だけ更新して再送する。
- 端末はどの API でも自端末・自内線の情報にしかアクセスできない（02 §6）。
- ステータス：401 認証失効／403 権限なし／404 存在しない／409・410 状態競合／5xx サーバー障害。

## 端末登録・認証

### POST /v1/enroll（認証不要）

```json
{"code": "12345678", "device_name": "iPhone", "platform": "ios", "app_version": "0.1.0", "push_environment": "sandbox"}
```

- `code`：10分・1回限りの登録コード（大文字英数字、アプリが空白・ハイフンを除去済み）。試行はレート制限する。
- `push_environment`：`sandbox` / `production`。このトークンの送信先 APNs 環境（01 §7-5）。
- 同じ内線に既存端末があれば、その資格情報と Push 紐付けを失効させる（F-05）。

応答 200：

```json
{
  "device_id": "d-7f3a",
  "access_token": "...", "access_token_expires_in": 3600, "refresh_token": "...",
  "extension": {"number": "201", "display_name": "受付 iPhone"},
  "sip": {"domain": "192.168.1.10", "proxy_host": null, "proxy_port": 5060,
          "transport": "udp", "srtp": "optional", "username": "201", "auth_username": "201", "password": "..."},
  "policy": {"ring_timeout_seconds": 30}
}
```

- `transport`：`udp` / `tcp` / `tls`。`proxy_host` は省略時 `domain`、`proxy_port` は省略時 udp/tcp なら 5060、tls なら 5061。
- `srtp`：`optional`（相手が対応すれば SRTP）/ `mandatory`（SRTP 必須）/ `disabled`（暗号化しない）。省略時は `optional`。
- コード不正・期限切れは 401 `invalid_code`、使用済みは 409 `code_used`（アプリはどちらも「登録コードが無効」と表示）。失敗が IP あたり1分10回を超えると 429。

### POST /v1/auth/refresh（認証不要）

`{"refresh_token": "..."}` → `{"access_token": "...", "expires_in": 3600, "refresh_token": "..."}`（更新トークンはローテーション）。

### PUT /v1/device/push-token → 204

```json
{"voip_token": "hex", "alert_token": "hex または null", "environment": "sandbox", "bundle_id": "net.nemnet-lab.myvoip"}
```

- `bundle_id` と `environment` をサーバー側で検証する。VoIP の topic は `net.nemnet-lab.myvoip.voip`、不在通知（一般通知）の topic は `net.nemnet-lab.myvoip`。
- `alert_token` が null なら不在通知は送らない。

### PUT /v1/device/availability → 204

`{"enabled": false}`。停止中の着信はサーバーで早期に受付不可とし、Push を送らない。

### GET /v1/device/status

`{"enabled": true, "voip_token_registered": true, "extension": {...}, "server_time": 1790000000}`（接続確認で使用）。

### POST /v1/device/test-call

`{"call_id": "UUID"}`。この端末宛てに実在する短い着信呼を作り、通常の着信と同じ経路（Push → ready → INVITE）で発信する。応答後はガイダンス音声などを流し、短時間で終了する（F-02）。

### DELETE /v1/device → 204

ログアウト。端末資格情報・Push 紐付けを失効する。通信できず失敗した場合、アプリは更新トークンだけを保持し、次回の前面復帰で再試行する（F-04）。

## 着信

### VoIP Push

- ヘッダー：`apns-push-type: voip`、`apns-topic: net.nemnet-lab.myvoip.voip`、`apns-priority: 10`、`apns-expiration: 0`。
- 実際の着信開始だけに送る。取消・生存確認・履歴同期には使わない（F-44）。

```json
{"aps": {}, "call_id": "UUID", "device_id": "d-7f3a", "sent_at": 1790000000.0, "expires_at": 1790000030.0,
 "caller_number": "0312345678", "caller_name": "代表"}
```

- `sent_at` と `expires_at` の差を呼出の残り時間として使う（端末時計のずれを補正）。
- SIP パスワード・API トークンは含めない（02 §6）。

### POST /v1/calls/{call_id}/ready

アプリが SIP REGISTER に成功した後に送る。

```json
{"registration_id": "9c1e...（32桁16進）"}
```

- アプリは REGISTER の Contact に `;myvoip-reg=<registration_id>` を付ける。サーバーは ARI で内線の現在の Contact を取得し、この値を含む Contact が存在することを確認してから端末レッグを発呼する。古い Contact だけでは到達可能と判定しない（03 §4.1）。
- 応答は `CallStatus`。既に終了した呼には 200 で `state: "ended"` と `end_reason` を返す（冪等。重複 ready は同じ結果）。
- アプリはネットワーク失敗時、初回を含め最大3試行・呼期限と10秒の早い方まで再送する（F-43）。

### 端末レッグの INVITE

- サーバーは端末向け INVITE に `X-MyVoIP-Call-ID: <call_id>` を付ける。外部から来た同名ヘッダーは PBX 境界で除去する（03 §5.6）。
- アプリは、このヘッダーが受付中の call UUID と一致しない INVITE を 603 で拒否する。

### GET /v1/calls/{call_id}

`CallStatus` を返す。Push 受信直後に1回呼び、既に終了していれば着信をすぐ閉じる。

```json
{"id": "UUID", "state": "waiting_ready", "end_reason": null, "expires_at": 1790000030.0,
 "caller_number": "0312345678", "caller_name": "代表"}
```

- `state`：`created` / `pushing` / `waiting_ready` / `inviting` / `ringing` / `connected` / `ended`
- `end_reason`：`caller_cancelled` / `timeout` / `declined` / `busy` / `completed` / `paused` / `server_error` / `device_unreachable`（未知の値は「サーバーが終了」と表示）

### POST /v1/calls/{call_id}/decline

`{"reason": "declined"}` または `{"reason": "busy"}`（通話中の着信: F-16）。終端状態なら追加作用なし。応答は `CallStatus`。

### WebSocket /v1/calls/{call_id}/events（wss）

- 着信処理中・通話中だけアプリが接続する補助経路。待機中は接続しない。
- サーバーは状態が変わるたびに `CallStatus` の JSON をテキストフレームで送る。アプリは `state: "ended"` を受けたら呼を終える（F-14）。
- 接続できなくても SIP の CANCEL/BYE と呼期限で処理が進むため、アプリは再接続を繰り返さない。

## 履歴

### GET /v1/history?cursor=...

```json
{"items": [{"id": "h-1", "call_id": "UUID", "direction": "incoming", "result": "missed",
            "remote_number": "0312345678", "remote_name": null, "started_at": 1790000000,
            "duration_seconds": null, "detail": "応答なし"}],
 "deleted_ids": ["h-0"], "next_cursor": "opaque"}
```

- `result`：`answered` / `outgoing` / `missed` / `declined` / `busy` / `failed` / `cancelled`
- 差分取得。`deleted_ids` で削除済みを伝え、端末で再出現させない。180日で削除する。
- 端末で記録した呼（同じ `call_id`）は端末の内容を優先する。端末に届かなかった不在着信だけがサーバー由来で追加される。

### DELETE /v1/history/{id} → 204、DELETE /v1/history → 204

アプリ用履歴の削除。PBX の CDR は対象外。
