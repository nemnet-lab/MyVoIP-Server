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
)

// Extension は MikoPBX の既存内線とアプリ用の接続情報（01 §7-1: 内線の正本は MikoPBX）。
type Extension struct {
	Number      string
	DisplayName string
	// SIPUsername は REGISTER の AOR ユーザー部
	SIPUsername     string
	SIPAuthUsername string
	// SIPPasswordEnc はサーバー鍵で暗号化した SIP パスワード
	SIPPasswordEnc []byte
	// Endpoint は端末レッグを発呼する PJSIP エンドポイント名（通常は内線番号）
	Endpoint           string
	RingTimeoutSeconds int
	UpdatedAt          time.Time
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
}
