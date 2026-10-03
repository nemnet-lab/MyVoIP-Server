// Package apns は APNs への VoIP Push・一般通知の送信を行う（03 §3: 標準 net/http・HTTP/2）。
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	// ErrTokenInvalid はトークンが無効・期限切れ（端末側の紐付けを外す）。
	ErrTokenInvalid = errors.New("apns: device token invalid")
)

const (
	productionHost = "https://api.push.apple.com"
	sandboxHost    = "https://api.sandbox.push.apple.com"
)

type Client struct {
	keyID    string
	teamID   string
	bundleID string
	key      *ecdsa.PrivateKey
	http     *http.Client
	// 試験で送信先を差し替える
	hostOverride string

	mu        sync.Mutex
	jwt       string
	jwtIssued time.Time
}

func LoadKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("apns: key file is not PEM (.p8)")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parse key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: key is not ECDSA")
	}
	return key, nil
}

func New(key *ecdsa.PrivateKey, keyID, teamID, bundleID string) *Client {
	transport := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 4, IdleConnTimeout: 5 * time.Minute}
	return &Client{
		keyID:    keyID,
		teamID:   teamID,
		bundleID: bundleID,
		key:      key,
		http:     &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}
}

// providerToken は ES256 の JWT。Apple の推奨に従い 20〜60 分で更新する。
func (c *Client) providerToken(force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.jwt != "" && time.Since(c.jwtIssued) < 40*time.Minute {
		return c.jwt, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.keyID})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iss": c.teamID, "iat": now.Unix()})
	unsigned := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	c.jwt = unsigned + "." + b64(sig)
	c.jwtIssued = now
	return c.jwt, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// VoIPPayload は 03 §5.3 の Push 内容（SIP パスワード・API トークンは含めない）。
type VoIPPayload struct {
	CallID       string  `json:"call_id"`
	DeviceID     string  `json:"device_id"`
	SentAt       float64 `json:"sent_at"`
	ExpiresAt    float64 `json:"expires_at"`
	CallerNumber string  `json:"caller_number"`
	CallerName   string  `json:"caller_name,omitempty"`
}

// SendVoIP は VoIP Push を送る。優先度10・expiration 0（期限切れ着信を滞留させない）。
func (c *Client) SendVoIP(ctx context.Context, token, environment string, p VoIPPayload) error {
	body := map[string]any{"aps": map[string]any{}}
	raw, _ := json.Marshal(p)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for k, v := range fields {
		body[k] = v
	}
	return c.send(ctx, token, environment, c.bundleID+".voip", "voip", "10", body)
}

// SendAlert は不在着信の一般通知を送る（F-17）。
func (c *Client) SendAlert(ctx context.Context, token, environment, title, message string) error {
	body := map[string]any{"aps": map[string]any{
		"alert": map[string]string{"title": title, "body": message},
		"sound": "default",
	}}
	return c.send(ctx, token, environment, c.bundleID, "alert", "10", body)
}

func (c *Client) send(ctx context.Context, token, environment, topic, pushType, priority string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	host := productionHost
	if environment == "sandbox" {
		host = sandboxHost
	}
	if c.hostOverride != "" {
		host = c.hostOverride
	}
	for attempt := 0; attempt < 2; attempt++ {
		jwt, err := c.providerToken(attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+token, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("authorization", "bearer "+jwt)
		req.Header.Set("apns-topic", topic)
		req.Header.Set("apns-push-type", pushType)
		req.Header.Set("apns-priority", priority)
		req.Header.Set("apns-expiration", "0")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("apns: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		var reason struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(data, &reason)
		switch {
		case resp.StatusCode == http.StatusGone,
			reason.Reason == "BadDeviceToken", reason.Reason == "Unregistered", reason.Reason == "DeviceTokenNotForTopic":
			return ErrTokenInvalid
		case resp.StatusCode == http.StatusForbidden && (reason.Reason == "ExpiredProviderToken" || reason.Reason == "InvalidProviderToken") && attempt == 0:
			continue // JWT を作り直して1回だけ再送
		default:
			return fmt.Errorf("apns: status %s reason %q", strconv.Itoa(resp.StatusCode), reason.Reason)
		}
	}
	return errors.New("apns: provider token rejected")
}

// verifyES256 は試験用の署名検証。
func verifyES256(pub *ecdsa.PublicKey, signed string, sig []byte) bool {
	digest := sha256.Sum256([]byte(signed))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, digest[:], r, s)
}
