// Package linkcheck decides whether an article's address is still alive.
//
// It is built for an old reading list, where a link that looks broken is
// usually just a site turning away a script. So it only calls a link dead on
// a clear signal: 404 or 410, a domain that no longer resolves, or a refused
// connection. Everything else, including 401, 403, 429, 5xx, timeouts and TLS
// errors, is unknown, and callers treat unknown as alive.
package linkcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// Verdict is what a check concluded.
type Verdict int

const (
	Live    Verdict = iota // the page answered
	Unknown                // no clear answer; treat as alive
	Dead                   // a clear signal the page is gone
)

func (v Verdict) String() string {
	switch v {
	case Live:
		return "live"
	case Dead:
		return "dead"
	default:
		return "unknown"
	}
}

// Result is the outcome of checking one address.
type Result struct {
	URL     string
	Verdict Verdict
	Reason  string // short and human readable: "HTTP 404", "domain no longer resolves"
	// Homepage is set when the address redirected to its site's front page,
	// which is how many sites retire an article. The verdict stays Unknown
	// until there is data on how often that is a false alarm.
	Homepage bool
}

// Resolver looks up a host name. *net.Resolver satisfies it; tests supply
// canned answers.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Checker checks addresses over HTTP. The zero value is not usable; call New.
type Checker struct {
	Client   *http.Client
	Resolver Resolver
	// Timeout bounds each address's check, redirects and fallback included.
	Timeout   time.Duration
	UserAgent string
	// Canary is a host that should always resolve. When an address's domain
	// fails to resolve, the canary is looked up too: if it fails as well,
	// the network is down rather than the domain gone, and nothing is
	// called dead for it. Empty skips that guard.
	Canary string
	// Parallel caps how many checks run at once in CheckAll.
	Parallel int
}

// userAgent is a current desktop browser's. Plenty of sites answer an
// unfamiliar agent with 403, which would only add noise to the unknowns.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

// New returns a Checker with the defaults pick uses.
func New() *Checker {
	return &Checker{
		Client:    &http.Client{},
		Resolver:  net.DefaultResolver,
		Timeout:   5 * time.Second,
		UserAgent: userAgent,
		Canary:    "www.instapaper.com",
		Parallel:  32,
	}
}

// CheckAll checks every address in parallel and returns the results in the
// same order.
func (c *Checker) CheckAll(ctx context.Context, urls []string) []Result {
	out := make([]Result, len(urls))
	sem := make(chan struct{}, max(c.Parallel, 1))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = c.Check(ctx, u)
		}()
	}
	wg.Wait()
	return out
}

// Check checks one address.
func (c *Checker) Check(ctx context.Context, raw string) Result {
	r := Result{URL: raw, Verdict: Unknown}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		r.Reason = "not a web address"
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	// Resolve first, through the injectable resolver, so a vanished domain
	// is told apart from every other way a request can fail.
	if net.ParseIP(u.Hostname()) == nil {
		if _, err := c.Resolver.LookupHost(ctx, u.Hostname()); err != nil {
			if notFound(err) && c.stillGone(ctx, u.Hostname()) && c.online(ctx) {
				r.Verdict, r.Reason = Dead, "domain no longer resolves"
			} else {
				r.Reason = "DNS lookup failed"
			}
			return r
		}
	}

	resp, err := c.request(ctx, http.MethodHead, raw)
	if err == nil && (resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented) {
		resp, err = c.request(ctx, http.MethodGet, raw)
	}
	if err == nil && gone(resp.StatusCode) {
		// Some servers answer HEAD with 404 for pages a browser gets fine.
		// Calling a page dead archives it, so ask again the way a browser
		// would before believing it.
		resp, err = c.request(ctx, http.MethodGet, raw)
	}
	if err != nil {
		return c.classifyError(ctx, r, err)
	}

	switch code := resp.StatusCode; {
	case gone(code):
		r.Verdict, r.Reason = Dead, fmt.Sprintf("HTTP %d", code)
	case code < 400:
		r.Verdict = Live
		if homepage(u, resp.Request.URL) {
			r.Verdict, r.Homepage, r.Reason = Unknown, true, "redirected to the homepage"
		}
	default:
		r.Reason = fmt.Sprintf("HTTP %d", code)
	}
	return r
}

// request sends one request and closes the body; only the status and the
// final address matter. GETs ask for a small range so a fallback doesn't
// download a whole page.
func (c *Checker) request(ctx context.Context, method, raw string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-1023")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

// classifyError sorts a failed request. A redirect can lead to another host,
// so a vanished domain can still show up here rather than in the lookup.
func (c *Checker) classifyError(ctx context.Context, r Result, err error) Result {
	switch {
	case notFound(err) && c.stillGone(ctx, dnsName(err)) && c.online(ctx):
		r.Verdict, r.Reason = Dead, "domain no longer resolves"
	case errors.Is(err, syscall.ECONNREFUSED):
		r.Verdict, r.Reason = Dead, "connection refused"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		r.Reason = "timed out"
	case errors.Is(err, context.Canceled):
		r.Reason = "cancelled"
	default:
		r.Reason = shortError(err)
	}
	return r
}

// online reports whether the canary resolves. Offline, a resolver can
// answer "no such host" for everything, and believing it would bury a whole
// pick under dead tags.
func (c *Checker) online(ctx context.Context) bool {
	if c.Canary == "" {
		return true
	}
	_, err := c.Resolver.LookupHost(ctx, c.Canary)
	return err == nil
}

// recheckDelay is how long stillGone waits before asking again.
var recheckDelay = 500 * time.Millisecond

// stillGone asks about host a second time, a moment later. Seen live: a pick's
// batch had www.bbc.co.uk come back "no such host" alongside two genuinely
// dead domains, in the same second, and resolve fine on every run after. A
// dead verdict archives the article, so one answer is not enough to bury it.
func (c *Checker) stillGone(ctx context.Context, host string) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(recheckDelay):
	}
	_, err := c.Resolver.LookupHost(ctx, host)
	return notFound(err)
}

// notFound reports an NXDOMAIN-style answer: the name does not exist, as
// opposed to a lookup that failed or timed out.
func notFound(err error) bool {
	var dns *net.DNSError
	return errors.As(err, &dns) && dns.IsNotFound
}

// dnsName is the host a failed lookup was for.
func dnsName(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.Name
	}
	return ""
}

func gone(code int) bool { return code == http.StatusNotFound || code == http.StatusGone }

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// homepage reports whether a request for an article ended on a site's front
// page. The host may differ: a retired site often forwards to its new owner.
func homepage(from, to *url.URL) bool {
	return to != nil && root(to.Path) && !root(from.Path)
}

func root(path string) bool { return path == "" || path == "/" }

// shortError drops the "Head \"https://...\": " prefix url.Error adds, since
// the address is printed beside the reason anyway.
func shortError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}
