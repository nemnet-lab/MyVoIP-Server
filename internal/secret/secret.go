// Package secret はトークン・登録コードの生成と、SIP パスワードの暗号化を扱う。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
)

// NewToken は推測できない不透明トークンを作る（DB にはハッシュだけ保存する）。
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash はトークン・登録コードの保存用ハッシュ。
func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// 読み間違えやすい文字（0/O, 1/I/L）を除いた英大文字と数字。
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewEnrollmentCode は8文字の登録コードを作る（表示は XXXX-XXXX）。
func NewEnrollmentCode() string {
	var sb strings.Builder
	max := big.NewInt(int64(len(codeAlphabet)))
	for i := 0; i < 8; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		sb.WriteByte(codeAlphabet[n.Int64()])
	}
	return sb.String()
}

// FormatCode は 4 文字ごとにハイフンを入れる。
func FormatCode(code string) string {
	if len(code) != 8 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// NormalizeCode はアプリと同じく空白・ハイフンを除いて大文字にする。
func NormalizeCode(raw string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(raw) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// NewKey は Box 用の32バイト鍵を base64 で返す（MYVOIP_SECRET_KEY）。
func NewKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Box は AES-256-GCM による暗号化（SIP パスワードを DB に平文で置かない）。
type Box struct {
	aead cipher.AEAD
}

// NewBox は base64 で表した32バイト鍵から作る。
func NewBox(base64Key string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(base64Key))
	if err != nil {
		return nil, errors.New("MYVOIP_SECRET_KEY must be base64")
	}
	if len(key) != 32 {
		return nil, errors.New("MYVOIP_SECRET_KEY must be 32 bytes (myvoip-server gen-secret)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain string) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return b.aead.Seal(nonce, nonce, []byte(plain), nil)
}

func (b *Box) Open(sealed []byte) (string, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt (wrong MYVOIP_SECRET_KEY?)")
	}
	return string(plain), nil
}

// NewID は UUID v4 形式の ID を作る。
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ShortID はログ用の短い ID（トークン全文をログに出さない: 02 §6）。
func ShortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}
