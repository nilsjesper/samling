package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// An install from before OAuth 2 has an xAuth token with a secret; it must load
// unchanged.
func TestLoadTokenLegacyXAuth(t *testing.T) {
	t.Setenv("SAMLING_HOME", t.TempDir())
	legacy := `{"oauth_token":"tk","oauth_token_secret":"ts","user_id":7,"username":"me"}`
	if err := os.WriteFile(Path("token.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadToken()
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	want := Token{Token: "tk", TokenSecret: "ts", UserID: 7, Username: "me"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A bearer token has no secret, and that alone must not read as "not logged in".
func TestBearerTokenRoundTrips(t *testing.T) {
	t.Setenv("SAMLING_HOME", t.TempDir())
	if err := SaveToken(Token{Token: "bearer"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path("token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "oauth_token_secret") {
		t.Errorf("empty secret was written out: %s", b)
	}
	got, err := LoadToken()
	if err != nil || got.Token != "bearer" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLoadTokenMissing(t *testing.T) {
	t.Setenv("SAMLING_HOME", t.TempDir())
	if _, err := LoadToken(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if err := os.WriteFile(Path("token.json"), []byte(`{"oauth_token":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty token: err = %v, want ErrNoToken", err)
	}
}
