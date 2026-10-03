package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(key, "KEYID12345", "TEAMID1234", "net.nemnet-lab.myvoip")
	c.hostOverride = srv.URL
	return c, key
}

func TestVoIPPushHeadersPayloadAndJWT(t *testing.T) {
	var got *http.Request
	var body map[string]any
	c, key := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
	})
	err := c.SendVoIP(context.Background(), "abcd", "sandbox", VoIPPayload{CallID: "c1", DeviceID: "d1", SentAt: 1, ExpiresAt: 31, CallerNumber: "201"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/3/device/abcd" || got.Header.Get("apns-topic") != "net.nemnet-lab.myvoip.voip" ||
		got.Header.Get("apns-push-type") != "voip" || got.Header.Get("apns-priority") != "10" || got.Header.Get("apns-expiration") != "0" {
		t.Fatalf("headers %v", got.Header)
	}
	if body["call_id"] != "c1" || body["aps"] == nil || body["caller_number"] != "201" {
		t.Fatalf("body %v", body)
	}
	if _, ok := body["caller_name"]; ok {
		t.Fatal("empty caller_name should be omitted")
	}
	jwt := strings.TrimPrefix(got.Header.Get("authorization"), "bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatal("bad jwt")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !verifyES256(&key.PublicKey, parts[0]+"."+parts[1], sig) {
		t.Fatal("jwt signature invalid")
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if !strings.Contains(string(claims), `"iss":"TEAMID1234"`) {
		t.Fatalf("claims %s", claims)
	}
}

func TestInvalidTokenIsReported(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
	})
	err := c.SendVoIP(context.Background(), "abcd", "production", VoIPPayload{})
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestExpiredProviderTokenRetriesOnce(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"reason":"ExpiredProviderToken"}`))
		}
	})
	if err := c.SendAlert(context.Background(), "abcd", "production", "不在着信", "201"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls %d", calls)
	}
}
