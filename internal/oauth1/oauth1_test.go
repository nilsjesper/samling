package oauth1

import (
	"net/url"
	"strings"
	"testing"
)

func merge(vs ...url.Values) url.Values {
	out := url.Values{}
	for _, src := range vs {
		for k, v := range src {
			out[k] = append(out[k], v...)
		}
	}
	return out
}

// The worked example from RFC 5849 §3.4.1.1. If this passes, the hard parts
// (percent-encoding, parameter normalization, URL normalization) are right.
func TestBaseStringRFC5849(t *testing.T) {
	// Upper-case host and an explicit default port, both of which must normalize away.
	base, query, err := NormalizeURL("http://EXAMPLE.COM:80/request?b5=%3D%253D&a3=a&c%40=&a2=r%20b")
	if err != nil {
		t.Fatalf("NormalizeURL: %v", err)
	}
	if want := "http://example.com/request"; base != want {
		t.Errorf("base URL = %q, want %q", base, want)
	}

	body, err := url.ParseQuery("c2&a3=2+q")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	oauth := url.Values{
		"oauth_consumer_key":     {"9djdj82h48djs9d2"},
		"oauth_token":            {"kkk9d7dh3k39sjv7"},
		"oauth_signature_method": {"HMAC-SHA1"},
		"oauth_timestamp":        {"137131201"},
		"oauth_nonce":            {"7d8f3e4a"},
	}

	got := BaseString("POST", base, merge(query, body, oauth))
	want := "POST&http%3A%2F%2Fexample.com%2Frequest&a2%3Dr%2520b%26a3%3D2%2520q%26a3%3D" +
		"a%26b5%3D%253D%25253D%26c%2540%3D%26c2%3D%26oauth_consumer_key%3D9djdj82h48" +
		"djs9d2%26oauth_nonce%3D7d8f3e4a%26oauth_signature_method%3DHMAC-SHA1%26oaut" +
		"h_timestamp%3D137131201%26oauth_token%3Dkkk9d7dh3k39sjv7"
	if got != want {
		t.Errorf("base string mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestEncode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"r b", "r%20b"},     // space is %20, never '+'
		{"~-._", "~-._"},     // the unreserved punctuation set
		{"=%3D", "%3D%253D"}, // '%' itself gets encoded
		{"c@", "c%40"},       //
		{"åx", "%C3%A5x"},    // UTF-8, byte by byte, uppercase hex
		{"", ""},             //
	} {
		if got := Encode(tc.in); got != tc.want {
			t.Errorf("Encode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Parameters sort by encoded key first, then encoded value. Naively sorting the
// joined "k=v" strings gets this wrong, because '-' (0x2D) sorts below '=' (0x3D).
func TestBaseStringSortsByKeyThenValue(t *testing.T) {
	params := url.Values{"a-b": {"1"}, "a": {"2", "1"}}
	got := BaseString("POST", "https://example.com/x", params)
	want := "POST&https%3A%2F%2Fexample.com%2Fx&a%3D1%26a%3D2%26a-b%3D1"
	if got != want {
		t.Errorf("base string mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestAuthorizationHeader(t *testing.T) {
	c := Credentials{
		ConsumerKey:    "ck",
		ConsumerSecret: "cs",
		Token:          "tk",
		TokenSecret:    "ts",
	}
	got, err := c.authorization("POST", "https://www.instapaper.com/api/1/bookmarks/archive",
		url.Values{"bookmark_id": {"1234"}}, "abc123", 1700000000)
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if !strings.HasPrefix(got, "OAuth ") {
		t.Fatalf("header does not start with the OAuth scheme: %q", got)
	}
	for _, want := range []string{
		`oauth_consumer_key="ck"`,
		`oauth_nonce="abc123"`,
		`oauth_signature_method="HMAC-SHA1"`,
		`oauth_timestamp="1700000000"`,
		`oauth_token="tk"`,
		`oauth_version="1.0"`,
		`oauth_signature="`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %s\ngot: %s", want, got)
		}
	}
	// The body parameter is signed but must not be echoed into the header.
	if strings.Contains(got, "bookmark_id") {
		t.Errorf("header leaked a body parameter: %s", got)
	}
	// Signing is deterministic given a fixed nonce and timestamp.
	again, _ := c.authorization("POST", "https://www.instapaper.com/api/1/bookmarks/archive",
		url.Values{"bookmark_id": {"1234"}}, "abc123", 1700000000)
	if got != again {
		t.Errorf("signing is not deterministic:\n%s\n%s", got, again)
	}
}

// Without a token (the xAuth access_token call) the signing key still needs its
// trailing separator, and oauth_token must be absent.
func TestAuthorizationWithoutToken(t *testing.T) {
	c := Credentials{ConsumerKey: "ck", ConsumerSecret: "cs"}
	got, err := c.authorization("POST", "https://www.instapaper.com/api/1/oauth/access_token",
		url.Values{"x_auth_mode": {"client_auth"}}, "n", 1)
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if strings.Contains(got, "oauth_token=") {
		t.Errorf("oauth_token should be omitted when there is no token: %s", got)
	}
}

func TestNonceIsRandom(t *testing.T) {
	a, err := Nonce()
	if err != nil {
		t.Fatalf("Nonce: %v", err)
	}
	b, _ := Nonce()
	if len(a) != 32 {
		t.Errorf("nonce length = %d, want 32", len(a))
	}
	if a == b {
		t.Errorf("two nonces came back identical: %q", a)
	}
}
