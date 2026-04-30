// Package cursor provides authentication and token management for Cursor IDE API.
// It handles the challenge/poll-based login flow, token storage and refresh.
package cursor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	LoginBaseURL = "https://cursor.com/loginDeepControl"
	PollURL      = "https://api2.cursor.sh/auth/poll"
	TokenURL     = "https://api2.cursor.sh/oauth/token"
	ClientID     = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

	defaultPollInterval = 2 * time.Second
	maxPollDuration     = 5 * time.Minute
)

// CursorAuth handles the Cursor authentication flow.
type CursorAuth struct {
	httpClient *http.Client
}

// NewCursorAuth creates a new CursorAuth service instance.
func NewCursorAuth(cfg *config.Config) *CursorAuth {
	return NewCursorAuthWithProxyURL(cfg, "")
}

// NewCursorAuthWithProxyURL creates a new CursorAuth with an optional proxy override.
func NewCursorAuthWithProxyURL(cfg *config.Config, proxyURL string) *CursorAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &CursorAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{Timeout: 30 * time.Second}),
	}
}

// LoginSession holds the parameters for a pending Cursor login.
type LoginSession struct {
	UUID      string
	Verifier  string
	Challenge string
	LoginURL  string
}

// GenerateLoginSession creates a new login session with PKCE-like challenge/verifier.
func (a *CursorAuth) GenerateLoginSession() (*LoginSession, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("failed to generate verifier: %w", err)
	}
	verifier := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(verifierBytes)

	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(challengeHash[:])

	sessionUUID := uuid.New().String()

	loginURL := fmt.Sprintf("%s?challenge=%s&uuid=%s&mode=login&redirectTarget=cli",
		LoginBaseURL, challenge, sessionUUID)

	return &LoginSession{
		UUID:      sessionUUID,
		Verifier:  verifier,
		Challenge: challenge,
		LoginURL:  loginURL,
	}, nil
}

// PollForToken polls the Cursor auth endpoint until the user completes login.
func (a *CursorAuth) PollForToken(ctx context.Context, session *LoginSession) (*CursorAuthBundle, error) {
	if session == nil {
		return nil, fmt.Errorf("login session is nil")
	}

	deadline := time.Now().Add(maxPollDuration)
	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	pollURL := fmt.Sprintf("%s?uuid=%s&verifier=%s", PollURL, session.UUID, session.Verifier)

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled: %w", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("login poll timed out after %v", maxPollDuration)
			}

			bundle, shouldContinue, err := a.doPoll(ctx, pollURL)
			if bundle != nil {
				return bundle, nil
			}
			if !shouldContinue {
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("unexpected poll termination")
			}
		}
	}
}

// doPoll makes a single poll request. Returns (bundle, shouldContinue, error).
func (a *CursorAuth) doPoll(ctx context.Context, pollURL string) (*CursorAuthBundle, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create poll request: %w", err)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		log.Debugf("cursor poll request error (will retry): %v", err)
		return nil, true, nil
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor poll: close body error: %v", errClose)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("cursor poll read error (will retry): %v", err)
		return nil, true, nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, true, nil
	}

	var pollResp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		AuthID       string `json:"authId"`
	}
	if err = json.Unmarshal(body, &pollResp); err != nil {
		log.Debugf("cursor poll parse error (will retry): %v", err)
		return nil, true, nil
	}

	if pollResp.AccessToken == "" {
		return nil, true, nil
	}

	email := extractEmailFromJWT(pollResp.AccessToken)

	return &CursorAuthBundle{
		TokenData: CursorTokenData{
			AccessToken:  pollResp.AccessToken,
			RefreshToken: pollResp.RefreshToken,
			AuthID:       pollResp.AuthID,
			Email:        email,
		},
	}, false, nil
}

// RefreshTokens refreshes an access token using a refresh token.
func (a *CursorAuth) RefreshTokens(ctx context.Context, refreshToken string) (*CursorTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClientID,
		"refresh_token": refreshToken,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal refresh payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(string(payloadBytes)))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor refresh: close body error: %v", errClose)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		ShouldLogout bool   `json:"shouldLogout"`
	}
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	if tokenResp.ShouldLogout {
		return nil, fmt.Errorf("refresh token is invalid, re-authentication required")
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in refresh response")
	}

	email := extractEmailFromJWT(tokenResp.AccessToken)

	return &CursorTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: refreshToken,
		Email:        email,
	}, nil
}

// CreateTokenStorage creates a CursorTokenStorage from an auth bundle.
func (a *CursorAuth) CreateTokenStorage(bundle *CursorAuthBundle) *CursorTokenStorage {
	return &CursorTokenStorage{
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		AuthID:       bundle.TokenData.AuthID,
		Email:        bundle.TokenData.Email,
		Type:         "cursor",
	}
}

// extractEmailFromJWT attempts to extract the email claim from a JWT access token.
func extractEmailFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}

	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	data, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}

	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if err = json.Unmarshal(data, &claims); err != nil {
		return ""
	}
	return claims.Email
}
