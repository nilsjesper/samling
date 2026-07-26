// Package oauth1 implements just enough of OAuth 1.0a (RFC 5849) to talk to
// Instapaper: HMAC-SHA1 signatures, parameters in the Authorization header, and
// the xAuth extension for exchanging a username and password for a token.
package oauth1

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Credentials identify the application, and once a user has logged in, the user.
// Token and TokenSecret are empty when requesting an access token.
type Credentials struct {
	ConsumerKey    string
	ConsumerSecret string
	Token          string
	TokenSecret    string
}

// Encode percent-encodes s per RFC 5849 §3.6. The unreserved set is ALPHA,
// DIGIT, '-', '.', '_' and '~'; every other byte becomes uppercase %XX. This is
// deliberately not url.QueryEscape, which encodes a space as '+'.
func Encode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// BaseString builds the signature base string from a normalized base URL (no
// query, no fragment) and the full set of request parameters.
func BaseString(method, baseURL string, params url.Values) string {
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(params))
	for k, vs := range params {
		for _, v := range vs {
			pairs = append(pairs, pair{Encode(k), Encode(v)})
		}
	}
	// Sort by encoded key, then encoded value. Sorting the joined "k=v" strings
	// would be wrong: '-' and '.' sort below '=', so a key that is a prefix of
	// another would come out in the wrong order.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	joined := make([]string, len(pairs))
	for i, p := range pairs {
		joined[i] = p.k + "=" + p.v
	}
	return strings.ToUpper(method) + "&" + Encode(baseURL) + "&" + Encode(strings.Join(joined, "&"))
}

// Signature computes the HMAC-SHA1 signature for an already-normalized request.
func Signature(consumerSecret, tokenSecret, method, baseURL string, params url.Values) string {
	key := Encode(consumerSecret) + "&" + Encode(tokenSecret)
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(BaseString(method, baseURL, params)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// NormalizeURL splits rawURL into the base string URI (RFC 5849 §3.4.1.2) and
// its query parameters, which must be folded into the signature.
func NormalizeURL(rawURL string) (string, url.Values, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, err
	}
	q := u.Query()
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	// Drop the port when it is the scheme default.
	if i := strings.LastIndex(host, ":"); i >= 0 {
		switch port := host[i+1:]; {
		case u.Scheme == "http" && port == "80", u.Scheme == "https" && port == "443":
			host = host[:i]
		}
	}
	u.Host = host
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), q, nil
}

// Authorization signs a request and returns the value for its Authorization
// header. body holds the form parameters that will be sent in the request body;
// they participate in the signature but are not repeated in the header.
func (c Credentials) Authorization(method, rawURL string, body url.Values) (string, error) {
	n, err := Nonce()
	if err != nil {
		return "", err
	}
	return c.authorization(method, rawURL, body, n, time.Now().Unix())
}

func (c Credentials) authorization(method, rawURL string, body url.Values, nonce string, ts int64) (string, error) {
	base, query, err := NormalizeURL(rawURL)
	if err != nil {
		return "", err
	}

	oauth := url.Values{
		"oauth_consumer_key":     {c.ConsumerKey},
		"oauth_nonce":            {nonce},
		"oauth_signature_method": {"HMAC-SHA1"},
		"oauth_timestamp":        {strconv.FormatInt(ts, 10)},
		"oauth_version":          {"1.0"},
	}
	if c.Token != "" {
		oauth.Set("oauth_token", c.Token)
	}

	all := url.Values{}
	for _, src := range []url.Values{query, body, oauth} {
		for k, vs := range src {
			all[k] = append(all[k], vs...)
		}
	}
	oauth.Set("oauth_signature", Signature(c.ConsumerSecret, c.TokenSecret, method, base, all))

	keys := make([]string, 0, len(oauth))
	for k := range oauth {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = Encode(k) + `="` + Encode(oauth.Get(k)) + `"`
	}
	return "OAuth " + strings.Join(parts, ", "), nil
}

// Nonce returns a random 32-character hex string.
func Nonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
