package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startCallback runs waitForCode on a free loopback port and returns its base
// URL and a channel carrying the outcome.
func startCallback(t *testing.T, ctx context.Context, state string) (string, <-chan callbackResult) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan callbackResult, 1)
	go func() {
		code, err := waitForCode(ctx, ln, state)
		out <- callbackResult{code, err}
	}()
	return "http://" + ln.Addr().String(), out
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func result(t *testing.T, ch <-chan callbackResult) callbackResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("waitForCode did not return")
		return callbackResult{}
	}
}

func TestCallbackDeliversCode(t *testing.T) {
	base, out := startCallback(t, context.Background(), "s3cret")

	// Browsers ask for a favicon too; that must not end the wait.
	if status, _ := get(t, base+"/favicon.ico"); status != http.StatusNotFound {
		t.Errorf("favicon status = %d, want 404", status)
	}
	status, body := get(t, base+"/callback?code=1a2b3c4d&state=s3cret")
	if status != http.StatusOK || !strings.Contains(body, "close this tab") {
		t.Errorf("callback page = %d %q", status, body)
	}

	r := result(t, out)
	if r.err != nil || r.code != "1a2b3c4d" {
		t.Fatalf("got code %q err %v", r.code, r.err)
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	base, out := startCallback(t, context.Background(), "s3cret")

	status, _ := get(t, base+"/callback?code=attacker&state=other")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	r := result(t, out)
	if r.err == nil || r.code != "" || !strings.Contains(r.err.Error(), "wrong state") {
		t.Fatalf("got code %q err %v, want a state error and no code", r.code, r.err)
	}
}

func TestCallbackMissingStateIsAMismatch(t *testing.T) {
	base, out := startCallback(t, context.Background(), "s3cret")
	get(t, base+"/callback?code=abc")
	if r := result(t, out); r.err == nil || r.code != "" {
		t.Fatalf("got code %q err %v, want rejection", r.code, r.err)
	}
}

func TestCallbackReportsDenial(t *testing.T) {
	base, out := startCallback(t, context.Background(), "s")
	get(t, base+"/callback?error=access_denied&error_reason=user_denied"+
		"&error_description=The+user+denied+your+request.&state=s")
	r := result(t, out)
	if r.err == nil || !strings.Contains(r.err.Error(), "The user denied your request.") {
		t.Fatalf("err = %v", r.err)
	}
}

func TestCallbackWithoutCode(t *testing.T) {
	base, out := startCallback(t, context.Background(), "s")
	get(t, base+"/callback?state=s")
	if r := result(t, out); r.err == nil {
		t.Fatal("expected an error for a callback with no code")
	}
}

func TestCallbackTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, out := startCallback(t, ctx, "s")
	r := result(t, out)
	if r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", r.err)
	}
}

func TestCallbackClosesListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitForCode(ctx, ln, "s")

	// The port must be free again so a retry with the same --port works.
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port still held after waitForCode returned: %v", err)
	}
	ln2.Close()
}

func TestLoginRefusesTokenOnCommandLine(t *testing.T) {
	err := cmdLogin([]string{"--token", "abc123"})
	if err == nil || !strings.Contains(err.Error(), "shell history") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "abc123") {
		t.Error("the error message echoes the token")
	}
}

func TestLoginTokenAndPasswordExclusive(t *testing.T) {
	if err := cmdLogin([]string{"--token", "--password"}); err == nil {
		t.Fatal("expected --token and --password to be rejected together")
	}
}

func TestRandomStateIsUnique(t *testing.T) {
	a, err := randomState()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := randomState()
	if a == b || len(a) != 32 {
		t.Errorf("states %q and %q", a, b)
	}
}
