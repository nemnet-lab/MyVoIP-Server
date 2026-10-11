// Package store は端末・内線・呼・履歴の永続化を扱う。
package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExpired  = errors.New("expired")
	ErrUsed     = errors.New("already used")
	// ErrLimit は利用者あたりの iPhone の上限に達している
	ErrLimit = errors.New("limit reached")
	// ErrDuplicate は同じ利用者が同じ SIP ユーザーの内線をすでに持っている
	ErrDuplicate = errors.New("duplicate")
)

// Extension は MikoPBX の既存内線とアプリ用の接続情報（01 §7-1: 内線の正本は MikoPBX）。
type Extension struct {
	Number      string
	DisplayName string
	// SIPUsername は REGISTER の AOR ユーザー部
	SIPUsername     string
	SIPAuthUsername string
	// SIPPasswordEnc はサーバー鍵で暗号化した SIP パスワード（MikoPBX の内線のパスワード）
	SIPPasswordEnc []byte
	// MobilePasswordEnc は中継モードで iPhone が中継用 Asterisk に登録するためのパスワード（暗号化済み）
	MobilePasswordEnc []byte
	// Endpoint は端末レッグを発呼する PJSIP エンドポイント名（通常は内線番号）
	Endpoint           string
	RingTimeoutSeconds int
	UpdatedAt          time.Time
	// OwnerUserID はこの内線を使う利用者（Web 管理画面。空なら未割り当て）
	OwnerUserID string
	// PBXExtension は利用者の PBX での内線番号（表示用。空なら Number）
	PBXExtension string
	// Label は利用者が付けた iPhone の名前
	Label      string
	AssignedAt *time.Time
}

// User は Web 管理画面に Discord でログインする利用者。
type User struct {
	ID              string
	DiscordID       string
	DiscordUsername string
	DisplayName     string
	Disabled        bool
	CreatedAt       time.Time
	LastLoginAt     *time.Time
	// PBXHost / PBXPort は利用者の PBX の SIP の接続先（中継用 Asterisk から見たアドレス）。空なら未設定
	PBXHost string
	PBXPort int
}

// WebSession は Web 管理画面のログイン状態（Cookie にはトークン、DB にはハッシュ）。
type WebSession struct {
	Hash      string
	UserID    string
	CSRF      string
	ExpiresAt time.Time
	CreatedAt time.Time
	// SupportUserID は管理者がサポート用パスコードで開いている利用者
	SupportUserID    string
	SupportExpiresAt *time.Time
}

// SupportPasscode は利用者が管理者に伝えるサポート用パスコード（DB にはハッシュ）。
type SupportPasscode struct {
	UserID    string
	CodeHash  string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// AuditEntry は利用者の画面に表示する操作記録。
type AuditEntry struct {
	ID          int64
	At          time.Time
	UserID      string
	ActorUserID string
	Action      string
	Detail      string
}

type EnrollmentCode struct {
	CodeHash  string
	Extension string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

type Device struct {
	ID              string
	Extension       string
	Name            string
	Platform        string
	AppVersion      string
	PushEnvironment string
	VoIPToken       string
	AlertToken      string
	Enabled         bool
	CreatedAt       time.Time
	RevokedAt       *time.Time
}

type TokenKind string

const (
	TokenAccess  TokenKind = "access"
	TokenRefresh TokenKind = "refresh"
)

type Token struct {
	Hash      string
	DeviceID  string
	Kind      TokenKind
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Call はサーバーが保持する呼（03 §6）。
type Call struct {
	ID            string
	DeviceID      string
	Extension     string
	CallerNumber  string
	CallerName    string
	State         string
	EndReason     string
	IsTest        bool
	CallerChannel string
	DeviceChannel string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	AnsweredAt    *time.Time
	EndedAt       *time.Time
}

// HistoryItem はアプリ用履歴（PBX の CDR とは別保存: 01 §7-6）。
type HistoryItem struct {
	ID              string
	Extension       string
	CallID          string
	Direction       string
	Result          string
	RemoteNumber    string
	RemoteName      string
	StartedAt       time.Time
	DurationSeconds *int
	Detail          string
	DeletedAt       *time.Time
	Seq             int64
}

// Store は永続化の境界。本番は PostgreSQL、試験はメモリ実装を使う。
type Store interface {
	UpsertExtension(ctx context.Context, e Extension) error
	SetMobilePassword(ctx context.Context, number string, enc []byte) error
	GetExtension(ctx context.Context, number string) (Extension, error)
	ListExtensions(ctx context.Context) ([]Extension, error)

	CreateEnrollmentCode(ctx context.Context, c EnrollmentCode) error
	// ConsumeEnrollmentCode は有効なコードを原子的に使用済みにする。
	ConsumeEnrollmentCode(ctx context.Context, codeHash string, now time.Time) (EnrollmentCode, error)

	// CreateDevice は同じ内線の既存端末を失効させてから新端末を登録する（F-05）。
	CreateDevice(ctx context.Context, d Device, now time.Time) error
	GetDevice(ctx context.Context, id string) (Device, error)
	ActiveDeviceForExtension(ctx context.Context, extension string) (Device, error)
	ListDevices(ctx context.Context) ([]Device, error)
	UpdateDevicePush(ctx context.Context, id, voipToken, alertToken, environment string) error
	ClearVoIPToken(ctx context.Context, id, token string) error
	SetDeviceEnabled(ctx context.Context, id string, enabled bool) error
	RevokeDevice(ctx context.Context, id string, now time.Time) error

	CreateToken(ctx context.Context, t Token) error
	GetToken(ctx context.Context, hash string) (Token, error)
	RevokeToken(ctx context.Context, hash string, now time.Time) error

	SaveCall(ctx context.Context, c Call) error
	GetCall(ctx context.Context, id string) (Call, error)
	ListUnfinishedCalls(ctx context.Context) ([]Call, error)

	AddHistory(ctx context.Context, h HistoryItem) error
	// ListHistory は seq が cursor より大きい変更（削除を含む）を古い順に返す。
	ListHistory(ctx context.Context, extension string, cursor int64, limit int) ([]HistoryItem, error)
	DeleteHistory(ctx context.Context, extension, id string, now time.Time) error
	DeleteAllHistory(ctx context.Context, extension string, now time.Time) error
	PurgeHistoryBefore(ctx context.Context, before time.Time) (int64, error)

	// --- Web 管理画面

	// UpsertDiscordUser は Discord の利用者を登録・更新し（ログイン日時も更新する）、保存した内容を返す。
	UpsertDiscordUser(ctx context.Context, u User, now time.Time) (User, error)
	GetUser(ctx context.Context, id string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	SetUserDisabled(ctx context.Context, id string, disabled bool) error
	SetUserPBX(ctx context.Context, id, host string, port int) error

	CreateWebSession(ctx context.Context, s WebSession) error
	GetWebSession(ctx context.Context, hash string) (WebSession, error)
	SetWebSessionSupport(ctx context.Context, hash, supportUserID string, until *time.Time) error
	DeleteWebSession(ctx context.Context, hash string) error
	DeleteUserWebSessions(ctx context.Context, userID string) error

	// CreateOwnedExtension は利用者の内線を登録する。利用者の内線が max 以上なら ErrLimit、
	// 同じ利用者が同じ SIP ユーザーの内線を持っていれば ErrDuplicate。
	CreateOwnedExtension(ctx context.Context, e Extension, max int, now time.Time) error
	// UpdateExtensionSIP は内線の SIP の設定を変える（passwordEnc が nil ならパスワードは変えない）。
	UpdateExtensionSIP(ctx context.Context, number, pbxExtension, sipUser, authUser string, passwordEnc []byte) error
	// DeleteExtension は内線と、その端末・登録コード・履歴を消す。
	DeleteExtension(ctx context.Context, number string) error
	// AssignExtension は持ち主のいない内線を利用者に割り当てる（管理者の操作）。
	AssignExtension(ctx context.Context, number, userID, label string, now time.Time) error
	// ReleaseExtension は内線の割り当てを外し、端末・登録コード・履歴を消す（次の利用者に引き継がない）。
	ReleaseExtension(ctx context.Context, number string, now time.Time) error
	SetExtensionLabel(ctx context.Context, number, label string) error
	ListExtensionsByOwner(ctx context.Context, userID string) ([]Extension, error)

	// SetSupportPasscode は利用者のサポート用パスコードを置き換える。
	SetSupportPasscode(ctx context.Context, p SupportPasscode) error
	GetSupportPasscode(ctx context.Context, userID string) (SupportPasscode, error)
	DeleteSupportPasscode(ctx context.Context, userID string) error
	// ConsumeSupportPasscode は有効なパスコードを消して、その利用者を返す。
	ConsumeSupportPasscode(ctx context.Context, codeHash string, now time.Time) (string, error)

	AddAudit(ctx context.Context, a AuditEntry) error
	ListAudit(ctx context.Context, userID string, limit int) ([]AuditEntry, error)
}
