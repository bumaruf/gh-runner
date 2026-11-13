package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenSource hands out the bearer token of every API request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a personal access token.
type StaticToken string

func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// AppTokenSource authenticates as a GitHub App installation. Installation tokens last one hour
// and only reach the repositories the app was installed on.
type AppTokenSource struct {
	// AppID is the app's client ID or numeric ID; GitHub accepts either as the JWT issuer.
	AppID          string
	Key            *rsa.PrivateKey
	InstallationID int64
	BaseURL        string
	HTTP           *http.Client
	Now            func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func ParsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block in the private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return key, nil
}

func (s *AppTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.token != "" && s.expires.Sub(now) > 5*time.Minute {
		return s.token, nil
	}

	jwt, err := s.jwt(now)
	if err != nil {
		return "", err
	}
	if s.InstallationID == 0 {
		if s.InstallationID, err = s.onlyInstallation(ctx, jwt); err != nil {
			return "", err
		}
	}

	var created struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", s.InstallationID)
	if err := s.appRequest(ctx, http.MethodPost, path, jwt, &created); err != nil {
		return "", err
	}
	s.token, s.expires = created.Token, created.ExpiresAt
	return s.token, nil
}

func (s *AppTokenSource) jwt(now time.Time) (string, error) {
	header := encodeSegment([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		// Backdated a minute against clock drift, as GitHub recommends; ten minutes is the most
		// GitHub accepts for the lifetime.
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": s.AppID,
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + encodeSegment(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(nil, s.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signingInput + "." + encodeSegment(signature), nil
}

func (s *AppTokenSource) onlyInstallation(ctx context.Context, jwt string) (int64, error) {
	var installations []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := s.appRequest(ctx, http.MethodGet, "/app/installations", jwt, &installations); err != nil {
		return 0, err
	}
	if len(installations) != 1 {
		return 0, fmt.Errorf("the app has %d installations; set GITHUB_APP_INSTALLATION_ID", len(installations))
	}
	return installations[0].ID, nil
}

func (s *AppTokenSource) appRequest(ctx context.Context, method, path, jwt string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	setCommonHeaders(req, jwt)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return newError(method, path, resp, body)
	}
	return json.Unmarshal(body, out)
}

func (s *AppTokenSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *AppTokenSource) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func encodeSegment(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
