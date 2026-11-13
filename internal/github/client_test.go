package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestARepeatedPollIsRevalidatedAndServedFromTheCachedBody(t *testing.T) {
	var full atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, `{"workflow_runs":[{"id":7,"status":"queued"},{"id":8,"status":"completed"}]}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, StaticToken("t"))

	for range 3 {
		runs, err := client.ActiveRuns(context.Background(), "me/web")
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 || runs[0].ID != 7 {
			t.Fatalf("runs = %+v, want only the queued run 7", runs)
		}
	}
	if got := full.Load(); got != 1 {
		t.Fatalf("full responses = %d, want 1: later polls must be 304s", got)
	}
}

func TestASecondaryRateLimitPausesTheClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, StaticToken("t"))

	if _, err := client.ActiveRuns(context.Background(), "me/web"); err == nil {
		t.Fatal("want the 403 as an error")
	}
	if _, err := client.ActiveRuns(context.Background(), "me/web"); err != ErrPaused {
		t.Fatalf("err = %v, want ErrPaused without another request", err)
	}
	if paused := client.PausedFor(); paused < 25*time.Second {
		t.Fatalf("paused for %v, want about 30s", paused)
	}
}

func TestAnAppTokenIsSignedByTheAppKeyAndReusedUntilNearExpiry(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var issued atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err := verifyJWT(jwt, &key.PublicKey, "Iv1.app"); err != nil {
			http.Error(w, `{"message":"bad jwt"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/app/installations":
			fmt.Fprint(w, `[{"id":42,"account":{"login":"me"}}]`)
		case "/app/installations/42/access_tokens":
			issued.Add(1)
			fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, issued.Load(), now.Add(time.Hour).Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	source := &AppTokenSource{AppID: "Iv1.app", Key: key, BaseURL: server.URL, Now: func() time.Time { return now }}
	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	second, _ := source.Token(context.Background())
	now = now.Add(6 * time.Minute)
	third, _ := source.Token(context.Background())

	if first != "ghs_1" || second != "ghs_1" || third != "ghs_2" {
		t.Fatalf("tokens = %s %s %s, want reuse until 5 minutes before expiry", first, second, third)
	}
}

func verifyJWT(jwt string, key *rsa.PublicKey, issuer string) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("want 3 parts")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return err
	}
	if claims.Iss != issuer || claims.Exp-claims.Iat > 600 {
		return fmt.Errorf("claims %+v", claims)
	}
	return nil
}
