package cursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
)

// CursorTokenData holds the raw token response from Cursor.
type CursorTokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AuthID       string `json:"auth_id,omitempty"`
	Email        string `json:"email,omitempty"`
}

// CursorAuthBundle aggregates authentication data after a successful login.
type CursorAuthBundle struct {
	TokenData CursorTokenData
}

// CursorTokenStorage stores OAuth2 token information for Cursor API authentication.
type CursorTokenStorage struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AuthID       string `json:"auth_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Type         string `json:"type"`

	Metadata map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *CursorTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serializes the Cursor token storage to a JSON file.
func (ts *CursorTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "cursor"

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// CredentialFileName returns the filename for persisting Cursor credentials.
func CredentialFileName(email string) string {
	if email == "" {
		return fmt.Sprintf("cursor-%d.json", os.Getpid())
	}
	return fmt.Sprintf("cursor-%s.json", email)
}
