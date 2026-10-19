package linkcheck

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDNS answers lookups from a table. Hosts in nx don't exist; everything
// else resolves to loopback, so tests never touch real DNS.
type fakeDNS struct {
	nx      map[string]bool
	offline bool
}

func (f fakeDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	if f.offline || f.nx[host] {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []string{"127.0.0.1"}, nil
}

// site serves a few pages the way real sites answer an old link.
func site(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/live":
			w.WriteHeader(http.StatusOK)
		case "/gone":
			w.WriteHeader(http.StatusGone)
		case "/bot-blocked":
			w.WriteHeader(http.StatusForbidden)
		case "/flaky":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/no-head":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Range") == "" {
				t.Error("GET fallback asked for the whole page")
			}
			w.WriteHeader(http.StatusPartialContent)
		case "/head-404":
			// HEAD wrongly says 404; a browser's GET works.
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/retired":
			http.Redirect(w, r, "/", http.StatusMovedPermanently)
		case "/moved":
			http.Redirect(w, r, "/live", http.StatusMovedPermanently)
		case "/slow":
			time.Sleep(time.Second)
		default:
			http.NotFound(w, r)
		}
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "Mozilla") {
			t.Errorf("User-Agent = %q, want a browser's", ua)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func checker(dns fakeDNS) *Checker {
	c := New()
	c.Resolver = dns
	c.Timeout = 2 * time.Second
	return c
}

func TestCheckVerdicts(t *testing.T) {
	srv := site(t)
	c := checker(fakeDNS{nx: map[string]bool{"gone.example": true}})

	// A server that was listening and no longer is refuses connections.
	closed := httptest.NewServer(http.NotFoundHandler())
	refused := closed.URL + "/article"
	closed.Close()

	for _, tc := range []struct {
		url      string
		want     Verdict
		reason   string
		homepage bool
	}{
		{srv.URL + "/live", Live, "", false},
		{srv.URL + "/missing", Dead, "HTTP 404", false},
		{srv.URL + "/gone", Dead, "HTTP 410", false},
		{"https://gone.example/2011/article", Dead, "domain no longer resolves", false},
		{refused, Dead, "connection refused", false},
		{srv.URL + "/bot-blocked", Unknown, "HTTP 403", false},
		{srv.URL + "/flaky", Unknown, "HTTP 503", false},
		{srv.URL + "/no-head", Live, "", false},
		{srv.URL + "/head-404", Live, "", false},
		{srv.URL + "/moved", Live, "", false},
		{srv.URL + "/retired", Unknown, "redirected to the homepage", true},
		{"ftp://example.com/file", Unknown, "not a web address", false},
	} {
		got := c.Check(context.Background(), tc.url)
		if got.Verdict != tc.want || got.Reason != tc.reason || got.Homepage != tc.homepage {
			t.Errorf("Check(%s) = %v %q homepage=%v; want %v %q homepage=%v",
				tc.url, got.Verdict, got.Reason, got.Homepage, tc.want, tc.reason, tc.homepage)
		}
	}
}

// Offline, every lookup fails as "no such host". The canary catches that,
// so nothing is called dead.
func TestOfflineIsNotDead(t *testing.T) {
	c := checker(fakeDNS{offline: true})
	got := c.Check(context.Background(), "https://gone.example/article")
	if got.Verdict != Unknown {
		t.Errorf("offline check = %v %q, want unknown", got.Verdict, got.Reason)
	}
}

func TestTimeoutIsUnknown(t *testing.T) {
	srv := site(t)
	c := checker(fakeDNS{})
	c.Timeout = 100 * time.Millisecond
	got := c.Check(context.Background(), srv.URL+"/slow")
	if got.Verdict != Unknown || got.Reason != "timed out" {
		t.Errorf("slow check = %v %q, want unknown, timed out", got.Verdict, got.Reason)
	}
}

func TestCheckAllKeepsOrderAndRunsInParallel(t *testing.T) {
	srv := site(t)
	c := checker(fakeDNS{})
	urls := []string{srv.URL + "/slow", srv.URL + "/gone", srv.URL + "/slow", srv.URL + "/live"}

	start := time.Now()
	got := c.CheckAll(context.Background(), urls)
	if d := time.Since(start); d > 1900*time.Millisecond {
		t.Errorf("four checks took %v; two one-second pages should overlap", d)
	}
	want := []Verdict{Live, Dead, Live, Live}
	for i := range urls {
		if got[i].URL != urls[i] || got[i].Verdict != want[i] {
			t.Errorf("result %d = %s %v, want %s %v", i, got[i].URL, got[i].Verdict, urls[i], want[i])
		}
	}
}

func init() { recheckDelay = time.Millisecond }

// blipDNS says "no such host" for its first lookup of a name, then resolves.
type blipDNS struct{ seen map[string]bool }

func (b *blipDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	if !b.seen[host] {
		b.seen[host] = true
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []string{"127.0.0.1"}, nil
}

// A single "no such host" is not enough to call a domain gone: live, a
// working www.bbc.co.uk came back that way once, mid-batch.
func TestOneMissedLookupIsNotDead(t *testing.T) {
	c := New()
	c.Resolver = &blipDNS{seen: map[string]bool{"www.instapaper.com": true}}
	c.Canary = "www.instapaper.com"
	if r := c.Check(context.Background(), "http://bbc.invalid/page"); r.Verdict == Dead {
		t.Errorf("one missed lookup gave %+v, want not dead", r)
	}
}
