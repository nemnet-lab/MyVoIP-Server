package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
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

const extensionColumns = `number, display_name, sip_username, sip_auth_username, sip_password_enc, endpoint, ring_timeout_seconds, updated_at`

func scanExtension(row pgx.Row) (Extension, error) {
	var e Extension
	err := row.Scan(&e.Number, &e.DisplayName, &e.SIPUsername, &e.SIPAuthUsername, &e.SIPPasswordEnc, &e.Endpoint, &e.RingTimeoutSeconds, &e.UpdatedAt)
	return e, notFound(err)
}

func (p *Postgres) GetExtension(ctx context.Context, number string) (Extension, error) {
	return scanExtension(p.pool.QueryRow(ctx, `SELECT `+extensionColumns+` FROM extensions WHERE number=$1`, number))
}

func (p *Postgres) ListExtensions(ctx context.Context) ([]Extension, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+extensionColumns+` FROM extensions ORDER BY number`)
	if err != nil {
		return nil, err
	}
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
	return tag.RowsAffected(), nil
}
