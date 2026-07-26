// Package config locates samling's state directory and reads and writes the
// two small files that live in it: the app's consumer credentials and the
// user's OAuth token.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds the Instapaper application credentials. Get a pair by applying
// at https://www.instapaper.com/developers/applications/create.
type Config struct {
	ConsumerKey    string `json:"consumer_key"`
	ConsumerSecret string `json:"consumer_secret"`
}

// Token is the per-user OAuth access token returned by xAuth. The password used
// to obtain it is never stored.
type Token struct {
	Token       string `json:"oauth_token"`
	TokenSecret string `json:"oauth_token_secret"`
	UserID      int64  `json:"user_id,omitempty"`
	Username    string `json:"username,omitempty"`
}

// ErrNoToken means the user has not run `samling login` yet.
var ErrNoToken = errors.New("not logged in: run `samling login`")

// Home is the state directory, ~/.samling unless SAMLING_HOME overrides it.
func Home() string {
	if v := os.Getenv("SAMLING_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".samling"
	}
	return filepath.Join(home, ".samling")
}

// Path returns the path to a file inside the state directory.
func Path(name string) string { return filepath.Join(Home(), name) }

// EnsureHome creates the state directory if it does not exist.
func EnsureHome() error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", Home(), err)
	}
	return nil
}

// LoadConfig reads the consumer credentials. Environment variables win over
// config.json so a key can be supplied for a single invocation.
func LoadConfig() (Config, error) {
	var c Config
	if err := readJSON(Path("config.json"), &c); err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if v := os.Getenv("INSTAPAPER_CONSUMER_KEY"); v != "" {
		c.ConsumerKey = v
	}
	if v := os.Getenv("INSTAPAPER_CONSUMER_SECRET"); v != "" {
		c.ConsumerSecret = v
	}
	if c.ConsumerKey == "" || c.ConsumerSecret == "" {
		return c, fmt.Errorf("no Instapaper consumer credentials found.\n"+
			"Set INSTAPAPER_CONSUMER_KEY and INSTAPAPER_CONSUMER_SECRET, or write %s:\n"+
			`  {"consumer_key": "...", "consumer_secret": "..."}`+"\n"+
			"Request a key at https://www.instapaper.com/developers/applications/create",
			Path("config.json"))
	}
	return c, nil
}

// SaveConfig writes config.json.
func SaveConfig(c Config) error {
	if err := EnsureHome(); err != nil {
		return err
	}
	return WriteJSON(Path("config.json"), c)
}

// LoadToken reads the stored access token.
func LoadToken() (Token, error) {
	var t Token
	err := readJSON(Path("token.json"), &t)
	if os.IsNotExist(err) {
		return t, ErrNoToken
	}
	if err != nil {
		return t, err
	}
	if t.Token == "" || t.TokenSecret == "" {
		return t, ErrNoToken
	}
	return t, nil
}

// SaveToken writes the access token with owner-only permissions.
func SaveToken(t Token) error {
	if err := EnsureHome(); err != nil {
		return err
	}
	return WriteJSON(Path("token.json"), t)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

// WriteJSON writes v as indented JSON to path, atomically and mode 0600, so an
// interrupted write can never leave a half-written file behind.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
