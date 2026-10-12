package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres は Store の PostgreSQL 実装。
type Postgres struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	// DB の起動待ち（コンテナ同時起動時）
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = pool.Ping(ctx); lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		pool.Close()
		return nil, fmt.Errorf("database not reachable: %w", lastErr)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// Migrate は埋め込みの SQL を順に適用する。
func (p *Postgres) Migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// --- extensions

func (p *Postgres) UpsertExtension(ctx context.Context, e Extension) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO extensions(number, display_name, sip_username, sip_auth_username, sip_password_enc, endpoint, ring_timeout_seconds, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (number) DO UPDATE SET display_name=$2, sip_username=$3, sip_auth_username=$4, sip_password_enc=$5,
			endpoint=$6, ring_timeout_seconds=$7, updated_at=now()`,
		e.Number, e.DisplayName, e.SIPUsername, e.SIPAuthUsername, e.SIPPasswordEnc, e.Endpoint, e.RingTimeoutSeconds)
	return err
}

const extensionColumns = `number, display_name, sip_username, sip_auth_username, sip_password_enc, endpoint, ring_timeout_seconds, updated_at, mobile_password_enc,
	COALESCE(owner_user_id, ''), pbx_extension, label, assigned_at, COALESCE(moh_file_id, '')`

func scanExtension(row pgx.Row) (Extension, error) {
	var e Extension
	err := row.Scan(&e.Number, &e.DisplayName, &e.SIPUsername, &e.SIPAuthUsername, &e.SIPPasswordEnc, &e.Endpoint, &e.RingTimeoutSeconds, &e.UpdatedAt, &e.MobilePasswordEnc,
		&e.OwnerUserID, &e.PBXExtension, &e.Label, &e.AssignedAt, &e.MOHFileID)
	return e, notFound(err)
}

func scanExtensions(rows pgx.Rows) ([]Extension, error) {
	defer rows.Close()
	var list []Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	return list, rows.Err()
}

func (p *Postgres) SetMobilePassword(ctx context.Context, number string, enc []byte) error {
	tag, err := p.pool.Exec(ctx, `UPDATE extensions SET mobile_password_enc=$2, updated_at=now() WHERE number=$1`, number, enc)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) GetExtension(ctx context.Context, number string) (Extension, error) {
	return scanExtension(p.pool.QueryRow(ctx, `SELECT `+extensionColumns+` FROM extensions WHERE number=$1`, number))
}

func (p *Postgres) ListExtensions(ctx context.Context) ([]Extension, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+extensionColumns+` FROM extensions ORDER BY length(number), number`)
	if err != nil {
		return nil, err
	}
	return scanExtensions(rows)
}

// --- enrollment

func (p *Postgres) CreateEnrollmentCode(ctx context.Context, c EnrollmentCode) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO enrollment_codes(code_hash, extension, expires_at) VALUES ($1,$2,$3)`, c.CodeHash, c.Extension, c.ExpiresAt)
	return err
}

func (p *Postgres) ConsumeEnrollmentCode(ctx context.Context, codeHash string, now time.Time) (EnrollmentCode, error) {
	var c EnrollmentCode
	err := p.pool.QueryRow(ctx, `
		UPDATE enrollment_codes SET used_at=$2
		WHERE code_hash=$1 AND used_at IS NULL AND expires_at > $2
		RETURNING code_hash, extension, expires_at, used_at, created_at`, codeHash, now).
		Scan(&c.CodeHash, &c.Extension, &c.ExpiresAt, &c.UsedAt, &c.CreatedAt)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	// 失敗理由を区別する
	var used *time.Time
	var expires time.Time
	err = p.pool.QueryRow(ctx, `SELECT used_at, expires_at FROM enrollment_codes WHERE code_hash=$1`, codeHash).Scan(&used, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if used != nil {
		return c, ErrUsed
	}
	return c, ErrExpired
}

// --- devices

const deviceColumns = `id, extension, name, platform, app_version, push_environment, voip_token, alert_token, enabled, created_at, revoked_at`

func scanDevice(row pgx.Row) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.Extension, &d.Name, &d.Platform, &d.AppVersion, &d.PushEnvironment, &d.VoIPToken, &d.AlertToken, &d.Enabled, &d.CreatedAt, &d.RevokedAt)
	return d, notFound(err)
}

func (p *Postgres) CreateDevice(ctx context.Context, d Device, now time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
		UPDATE tokens SET revoked_at=$2 WHERE revoked_at IS NULL AND device_id IN
			(SELECT id FROM devices WHERE extension=$1 AND revoked_at IS NULL)`, d.Extension, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET revoked_at=$2, voip_token='', alert_token='' WHERE extension=$1 AND revoked_at IS NULL`, d.Extension, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO devices(id, extension, name, platform, app_version, push_environment, voip_token, alert_token, enabled, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,'','',TRUE,$7)`,
		d.ID, d.Extension, d.Name, d.Platform, d.AppVersion, d.PushEnvironment, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) GetDevice(ctx context.Context, id string) (Device, error) {
	return scanDevice(p.pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM devices WHERE id=$1`, id))
}

func (p *Postgres) ActiveDeviceForExtension(ctx context.Context, extension string) (Device, error) {
	return scanDevice(p.pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM devices WHERE extension=$1 AND revoked_at IS NULL`, extension))
}

func (p *Postgres) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

func (p *Postgres) UpdateDevicePush(ctx context.Context, id, voipToken, alertToken, environment string) error {
	_, err := p.pool.Exec(ctx, `UPDATE devices SET voip_token=$2, alert_token=$3, push_environment=$4 WHERE id=$1 AND revoked_at IS NULL`, id, voipToken, alertToken, environment)
	return err
}

func (p *Postgres) ClearVoIPToken(ctx context.Context, id, token string) error {
	_, err := p.pool.Exec(ctx, `UPDATE devices SET voip_token='' WHERE id=$1 AND voip_token=$2`, id, token)
	return err
}

func (p *Postgres) SetDeviceEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := p.pool.Exec(ctx, `UPDATE devices SET enabled=$2 WHERE id=$1 AND revoked_at IS NULL`, id, enabled)
	return err
}

func (p *Postgres) RevokeDevice(ctx context.Context, id string, now time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE tokens SET revoked_at=$2 WHERE device_id=$1 AND revoked_at IS NULL`, id, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET revoked_at=COALESCE(revoked_at,$2), voip_token='', alert_token='' WHERE id=$1`, id, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- tokens

func (p *Postgres) CreateToken(ctx context.Context, t Token) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO tokens(hash, device_id, kind, expires_at) VALUES ($1,$2,$3,$4)`, t.Hash, t.DeviceID, string(t.Kind), t.ExpiresAt)
	return err
}

func (p *Postgres) GetToken(ctx context.Context, hash string) (Token, error) {
	var t Token
	var kind string
	err := p.pool.QueryRow(ctx, `SELECT hash, device_id, kind, expires_at, revoked_at FROM tokens WHERE hash=$1`, hash).
		Scan(&t.Hash, &t.DeviceID, &kind, &t.ExpiresAt, &t.RevokedAt)
	t.Kind = TokenKind(kind)
	return t, notFound(err)
}

func (p *Postgres) RevokeToken(ctx context.Context, hash string, now time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE tokens SET revoked_at=$2 WHERE hash=$1 AND revoked_at IS NULL`, hash, now)
	return err
}

// --- calls

const callColumns = `id, device_id, extension, caller_number, caller_name, state, end_reason, is_test, caller_channel, device_channel, created_at, expires_at, answered_at, ended_at`

func scanCall(row pgx.Row) (Call, error) {
	var c Call
	err := row.Scan(&c.ID, &c.DeviceID, &c.Extension, &c.CallerNumber, &c.CallerName, &c.State, &c.EndReason, &c.IsTest,
		&c.CallerChannel, &c.DeviceChannel, &c.CreatedAt, &c.ExpiresAt, &c.AnsweredAt, &c.EndedAt)
	return c, notFound(err)
}

func (p *Postgres) SaveCall(ctx context.Context, c Call) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO calls(`+callColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO UPDATE SET state=$6, end_reason=$7, caller_channel=$9, device_channel=$10, answered_at=$13, ended_at=$14`,
		c.ID, c.DeviceID, c.Extension, c.CallerNumber, c.CallerName, c.State, c.EndReason, c.IsTest,
		c.CallerChannel, c.DeviceChannel, c.CreatedAt, c.ExpiresAt, c.AnsweredAt, c.EndedAt)
	return err
}

func (p *Postgres) GetCall(ctx context.Context, id string) (Call, error) {
	return scanCall(p.pool.QueryRow(ctx, `SELECT `+callColumns+` FROM calls WHERE id=$1`, id))
}

func (p *Postgres) ListUnfinishedCalls(ctx context.Context) ([]Call, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+callColumns+` FROM calls WHERE state <> 'ended'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Call
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

// --- history

func (p *Postgres) AddHistory(ctx context.Context, h HistoryItem) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO history(id, extension, call_id, direction, result, remote_number, remote_name, started_at, duration_seconds, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (id) DO NOTHING`,
		h.ID, h.Extension, h.CallID, h.Direction, h.Result, h.RemoteNumber, h.RemoteName, h.StartedAt, h.DurationSeconds, h.Detail)
	return err
}

func (p *Postgres) ListHistory(ctx context.Context, extension string, cursor int64, limit int) ([]HistoryItem, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, extension, call_id, direction, result, remote_number, remote_name, started_at, duration_seconds, detail, deleted_at, seq
		FROM history WHERE extension=$1 AND seq > $2 ORDER BY seq LIMIT $3`, extension, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []HistoryItem
	for rows.Next() {
		var h HistoryItem
		if err := rows.Scan(&h.ID, &h.Extension, &h.CallID, &h.Direction, &h.Result, &h.RemoteNumber, &h.RemoteName,
			&h.StartedAt, &h.DurationSeconds, &h.Detail, &h.DeletedAt, &h.Seq); err != nil {
			return nil, err
		}
		list = append(list, h)
	}
	return list, rows.Err()
}

func (p *Postgres) DeleteHistory(ctx context.Context, extension, id string, now time.Time) error {
	tag, err := p.pool.Exec(ctx, `UPDATE history SET deleted_at=$3, seq=nextval('history_seq') WHERE extension=$1 AND id=$2 AND deleted_at IS NULL`, extension, id, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteAllHistory(ctx context.Context, extension string, now time.Time) error {
	_, err := p.pool.Exec(ctx, `UPDATE history SET deleted_at=$2, seq=nextval('history_seq') WHERE extension=$1 AND deleted_at IS NULL`, extension, now)
	return err
}

func (p *Postgres) PurgeHistoryBefore(ctx context.Context, before time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM history WHERE started_at < $1`, before)
	if err != nil {
		return 0, err
	}
	// 期限切れの登録コード・トークンも整理する
	_, _ = p.pool.Exec(ctx, `DELETE FROM enrollment_codes WHERE expires_at < $1`, before)
	_, _ = p.pool.Exec(ctx, `DELETE FROM tokens WHERE expires_at < $1`, before)
	_, _ = p.pool.Exec(ctx, `DELETE FROM calls WHERE created_at < $1`, before)
	_, _ = p.pool.Exec(ctx, `DELETE FROM web_sessions WHERE expires_at < now()`)
	_, _ = p.pool.Exec(ctx, `DELETE FROM support_passcodes WHERE expires_at < now()`)
	_, _ = p.pool.Exec(ctx, `DELETE FROM audit_log WHERE at < $1`, before)
	return tag.RowsAffected(), nil
}

// --- Web 管理画面：利用者

const userColumns = `id, discord_id, discord_username, display_name, disabled, created_at, last_login_at, pbx_host, pbx_port`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.DiscordID, &u.DiscordUsername, &u.DisplayName, &u.Disabled, &u.CreatedAt, &u.LastLoginAt, &u.PBXHost, &u.PBXPort)
	return u, notFound(err)
}

func (p *Postgres) UpsertDiscordUser(ctx context.Context, u User, now time.Time) (User, error) {
	return scanUser(p.pool.QueryRow(ctx, `
		INSERT INTO users(id, discord_id, discord_username, display_name, created_at, last_login_at)
		VALUES ($1,$2,$3,$4,$5,$5)
		ON CONFLICT (discord_id) DO UPDATE SET discord_username=$3, display_name=$4, last_login_at=$5
		RETURNING `+userColumns, u.ID, u.DiscordID, u.DiscordUsername, u.DisplayName, now))
}

func (p *Postgres) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(p.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id=$1`, id))
}

func (p *Postgres) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

func (p *Postgres) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	tag, err := p.pool.Exec(ctx, `UPDATE users SET disabled=$2 WHERE id=$1`, id, disabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetUserPBX(ctx context.Context, id, host string, port int) error {
	tag, err := p.pool.Exec(ctx, `UPDATE users SET pbx_host=$2, pbx_port=$3 WHERE id=$1`, id, host, port)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Web 管理画面：ログイン状態

func (p *Postgres) CreateWebSession(ctx context.Context, s WebSession) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO web_sessions(hash, user_id, csrf, expires_at) VALUES ($1,$2,$3,$4)`, s.Hash, s.UserID, s.CSRF, s.ExpiresAt)
	return err
}

func (p *Postgres) GetWebSession(ctx context.Context, hash string) (WebSession, error) {
	var s WebSession
	err := p.pool.QueryRow(ctx, `
		SELECT hash, user_id, csrf, expires_at, created_at, COALESCE(support_user_id, ''), support_expires_at
		FROM web_sessions WHERE hash=$1`, hash).
		Scan(&s.Hash, &s.UserID, &s.CSRF, &s.ExpiresAt, &s.CreatedAt, &s.SupportUserID, &s.SupportExpiresAt)
	return s, notFound(err)
}

func (p *Postgres) SetWebSessionSupport(ctx context.Context, hash, supportUserID string, until *time.Time) error {
	var target any
	if supportUserID != "" {
		target = supportUserID
	}
	_, err := p.pool.Exec(ctx, `UPDATE web_sessions SET support_user_id=$2, support_expires_at=$3 WHERE hash=$1`, hash, target, until)
	return err
}

func (p *Postgres) DeleteWebSession(ctx context.Context, hash string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM web_sessions WHERE hash=$1`, hash)
	return err
}

func (p *Postgres) DeleteUserWebSessions(ctx context.Context, userID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM web_sessions WHERE user_id=$1`, userID)
	return err
}

// --- Web 管理画面：内線の割り当て

func (p *Postgres) CreateOwnedExtension(ctx context.Context, e Extension, max int, now time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// 同じ利用者の同時追加で上限を超えないよう、利用者の行を押さえる
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, e.OwnerUserID).Scan(&id); err != nil {
		return notFound(err)
	}
	var owned, same int
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE sip_username=$2) FROM extensions WHERE owner_user_id=$1`,
		e.OwnerUserID, e.SIPUsername).Scan(&owned, &same); err != nil {
		return err
	}
	if same > 0 {
		return ErrDuplicate
	}
	if owned >= max {
		return ErrLimit
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO extensions(number, display_name, sip_username, sip_auth_username, sip_password_enc, endpoint, ring_timeout_seconds,
			mobile_password_enc, owner_user_id, pbx_extension, label, assigned_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now())`,
		e.Number, e.DisplayName, e.SIPUsername, e.SIPAuthUsername, e.SIPPasswordEnc, e.Endpoint, e.RingTimeoutSeconds,
		e.MobilePasswordEnc, e.OwnerUserID, e.PBXExtension, e.Label, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) UpdateExtensionSIP(ctx context.Context, number, pbxExtension, sipUser, authUser string, passwordEnc []byte) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE extensions SET pbx_extension=$2, sip_username=$3, sip_auth_username=$4, endpoint=$3,
			sip_password_enc=COALESCE($5, sip_password_enc), updated_at=now()
		WHERE number=$1`, number, pbxExtension, sipUser, authUser, passwordEnc)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteExtension(ctx context.Context, number string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM history WHERE extension=$1`, number); err != nil {
		return err
	}
	// 端末（とそのトークン）・登録コードは外部キーでまとめて消える
	tag, err := tx.Exec(ctx, `DELETE FROM extensions WHERE number=$1`, number)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (p *Postgres) AssignExtension(ctx context.Context, number, userID, label string, now time.Time) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE extensions SET owner_user_id=$2, label=$3, assigned_at=$4, moh_file_id=NULL, updated_at=now()
		WHERE number=$1 AND owner_user_id IS NULL`, number, userID, label, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ReleaseExtension(ctx context.Context, number string, now time.Time) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE extensions SET owner_user_id=NULL, label='', assigned_at=NULL, moh_file_id=NULL, updated_at=now() WHERE number=$1`, number)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tokens SET revoked_at=$2 WHERE revoked_at IS NULL AND device_id IN
			(SELECT id FROM devices WHERE extension=$1 AND revoked_at IS NULL)`, number, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET revoked_at=$2, voip_token='', alert_token='' WHERE extension=$1 AND revoked_at IS NULL`, number, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM enrollment_codes WHERE extension=$1`, number); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM history WHERE extension=$1`, number); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) SetExtensionLabel(ctx context.Context, number, label string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE extensions SET label=$2, updated_at=now() WHERE number=$1`, number, label)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ListExtensionsByOwner(ctx context.Context, userID string) ([]Extension, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+extensionColumns+` FROM extensions WHERE owner_user_id=$1 ORDER BY assigned_at, number`, userID)
	if err != nil {
		return nil, err
	}
	return scanExtensions(rows)
}

// --- Web 管理画面：サポート用パスコード

func (p *Postgres) SetSupportPasscode(ctx context.Context, c SupportPasscode) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO support_passcodes(user_id, code_hash, expires_at, created_at) VALUES ($1,$2,$3,now())
		ON CONFLICT (user_id) DO UPDATE SET code_hash=$2, expires_at=$3, created_at=now()`, c.UserID, c.CodeHash, c.ExpiresAt)
	return err
}

func (p *Postgres) GetSupportPasscode(ctx context.Context, userID string) (SupportPasscode, error) {
	var c SupportPasscode
	err := p.pool.QueryRow(ctx, `SELECT user_id, code_hash, expires_at, created_at FROM support_passcodes WHERE user_id=$1`, userID).
		Scan(&c.UserID, &c.CodeHash, &c.ExpiresAt, &c.CreatedAt)
	return c, notFound(err)
}

func (p *Postgres) DeleteSupportPasscode(ctx context.Context, userID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM support_passcodes WHERE user_id=$1`, userID)
	return err
}

func (p *Postgres) ConsumeSupportPasscode(ctx context.Context, codeHash string, now time.Time) (string, error) {
	var userID string
	err := p.pool.QueryRow(ctx, `DELETE FROM support_passcodes WHERE code_hash=$1 AND expires_at > $2 RETURNING user_id`, codeHash, now).Scan(&userID)
	return userID, notFound(err)
}

// --- Web 管理画面：保留音

const mohColumns = `id, owner_user_id, name, duration_ms, size_bytes, created_at`

func scanMOH(row pgx.Row) (MOHFile, error) {
	var m MOHFile
	err := row.Scan(&m.ID, &m.OwnerUserID, &m.Name, &m.DurationMS, &m.SizeBytes, &m.CreatedAt)
	return m, notFound(err)
}

func (p *Postgres) listMOH(ctx context.Context, query string, args ...any) ([]MOHFile, error) {
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []MOHFile
	for rows.Next() {
		m, err := scanMOH(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func (p *Postgres) CreateMOH(ctx context.Context, m MOHFile, max int) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, m.OwnerUserID).Scan(&id); err != nil {
		return notFound(err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM moh_files WHERE owner_user_id=$1`, m.OwnerUserID).Scan(&count); err != nil {
		return err
	}
	if count >= max {
		return ErrLimit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO moh_files(id, owner_user_id, name, duration_ms, size_bytes, data) VALUES ($1,$2,$3,$4,$5,$6)`,
		m.ID, m.OwnerUserID, m.Name, m.DurationMS, len(m.Data), m.Data); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) GetMOH(ctx context.Context, id string) (MOHFile, error) {
	return scanMOH(p.pool.QueryRow(ctx, `SELECT `+mohColumns+` FROM moh_files WHERE id=$1`, id))
}

func (p *Postgres) GetMOHData(ctx context.Context, id string) ([]byte, error) {
	var data []byte
	err := p.pool.QueryRow(ctx, `SELECT data FROM moh_files WHERE id=$1`, id).Scan(&data)
	return data, notFound(err)
}

func (p *Postgres) ListMOHByOwner(ctx context.Context, userID string) ([]MOHFile, error) {
	return p.listMOH(ctx, `SELECT `+mohColumns+` FROM moh_files WHERE owner_user_id=$1 ORDER BY created_at, id`, userID)
}

func (p *Postgres) ListAllMOH(ctx context.Context) ([]MOHFile, error) {
	return p.listMOH(ctx, `SELECT `+mohColumns+` FROM moh_files ORDER BY id`)
}

func (p *Postgres) DeleteMOH(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM moh_files WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) SetExtensionMOH(ctx context.Context, number, mohID string) error {
	var tag pgconn.CommandTag
	var err error
	if mohID == "" {
		tag, err = p.pool.Exec(ctx, `UPDATE extensions SET moh_file_id=NULL, updated_at=now() WHERE number=$1`, number)
	} else {
		// 内線の持ち主と保留音の持ち主が同じときだけ割り当てる
		tag, err = p.pool.Exec(ctx, `
			UPDATE extensions e SET moh_file_id=m.id, updated_at=now()
			FROM moh_files m
			WHERE e.number=$1 AND m.id=$2 AND e.owner_user_id IS NOT NULL AND m.owner_user_id=e.owner_user_id`, number, mohID)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Web 管理画面：操作記録

func (p *Postgres) AddAudit(ctx context.Context, a AuditEntry) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO audit_log(at, user_id, actor_user_id, action, detail) VALUES ($1,$2,$3,$4,$5)`,
		a.At, a.UserID, a.ActorUserID, a.Action, a.Detail)
	return err
}

func (p *Postgres) ListAudit(ctx context.Context, userID string, limit int) ([]AuditEntry, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, at, user_id, actor_user_id, action, detail FROM audit_log WHERE user_id=$1 ORDER BY at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.At, &a.UserID, &a.ActorUserID, &a.Action, &a.Detail); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
