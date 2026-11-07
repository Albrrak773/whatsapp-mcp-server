package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestGuardPage(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cases := []struct {
		name, method, host, ctype, site string
		want                            int
	}{
		{"page on loopback", "GET", "127.0.0.1:8765", "", "", 200},
		{"localhost name", "GET", "localhost:8765", "", "", 200},
		{"rebound hostname", "GET", "evil.example:8765", "", "", 403},
		{"json post from the page", "POST", "127.0.0.1:8765", "application/json", "same-origin", 200},
		{"json post without fetch metadata", "POST", "127.0.0.1:8765", "application/json", "", 200},
		{"form post", "POST", "127.0.0.1:8765", "application/x-www-form-urlencoded", "", 403},
		{"cross-site json post", "POST", "127.0.0.1:8765", "application/json", "cross-site", 403},
	}
	h := guardPage("127.0.0.1:8765", "", ok)
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/retry", strings.NewReader("{}"))
		r.Host = c.host
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestGuardPageToken(t *testing.T) {
	h := guardPage("0.0.0.0:8765", "s3cret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	do := func(target string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = "myhost:8765"
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("/", nil); w.Code != 401 {
		t.Fatalf("no token: got %d, want 401", w.Code)
	}
	if w := do("/?token=wrong", nil); w.Code != 401 {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}
	w := do("/?token=s3cret", nil)
	cookies := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatalf("token login: got %d with cookies %v", w.Code, cookies)
	}
	if w := do("/", cookies[0]); w.Code != 200 {
		t.Fatalf("with cookie: got %d, want 200", w.Code)
	}
}

func getStatus(t *testing.T, h http.Handler) pageStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	var st pageStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPageStatusOffline(t *testing.T) {
	_, wa := setup(t) // linked, never connected
	h := newPageHandler(&App{store: wa.store, wa: wa}, "127.0.0.1:8765", t.TempDir())
	wa.recordConnectFailure(syscall.ENETUNREACH, 3, time.Minute)

	st := getStatus(t, h)
	if st.State != stateReconnecting || st.Account != "+491500000000" || st.Error == "" || st.Retry == nil || st.Retry.Attempt != 3 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if st.Endpoint != "http://127.0.0.1:8765/mcp" || st.Archive.Messages == 0 || len(st.Messages) == 0 {
		t.Fatalf("missing archive data: %+v", st)
	}
	for i := 1; i < len(st.Messages); i++ {
		if st.Messages[i].Time > st.Messages[i-1].Time {
			t.Fatalf("messages not newest first: %v then %v", st.Messages[i-1].Time, st.Messages[i].Time)
		}
	}
	var sawGroup, sawMine bool
	for _, m := range st.Messages {
		sawGroup = sawGroup || (m.Group && m.Chat == "Team Lunch")
		sawMine = sawMine || m.FromMe
	}
	if !sawGroup || !sawMine {
		t.Fatalf("expected a named group message and one of mine: %+v", st.Messages)
	}
}

func TestPageStatusLoggedOutSurvivesRestart(t *testing.T) {
	_, wa := setup(t)
	h := newPageHandler(&App{store: wa.store, wa: wa}, "127.0.0.1:8765", t.TempDir())
	wa.handleEvent(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	st := getStatus(t, h)
	if st.State != stateLoggedOut || !strings.Contains(st.Error, "logged out") || st.Since == "" {
		t.Fatalf("after logout: %+v", st)
	}
	// A restart: the in-memory flag is gone and whatsmeow has dropped the device.
	wa.loggedOut.Store(nil)
	wa.Client().Store.ID = nil
	if st := getStatus(t, h); st.State != stateLoggedOut || st.Error == "" {
		t.Fatalf("after restart: %+v", st)
	}
}

func TestStartLinkWithoutNetwork(t *testing.T) {
	_, wa := setup(t)
	wa.Client().Store.ID = nil // never linked: the same client is used for pairing
	noNet := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}
	}}}
	wa.Client().SetWebsocketHTTPClient(noNet)
	wa.Client().SetPreLoginHTTPClient(noNet)
	h := guardPage("127.0.0.1:8765", "", newPageHandler(&App{store: wa.store, wa: wa}, "127.0.0.1:8765", t.TempDir()))

	r := httptest.NewRequest("POST", "/api/link", strings.NewReader(`{"full_history":false}`))
	r.Host = "127.0.0.1:8765"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "can't reach WhatsApp") {
		t.Fatalf("link without network: %d %s", w.Code, w.Body)
	}
	if st := wa.State(); st != stateNotLinked {
		t.Fatalf("state after failed link = %s, want %s", st, stateNotLinked)
	}
}

func TestPageReadOnly(t *testing.T) {
	_, wa := setup(t)
	h := newPageHandler(&App{store: wa.store}, "127.0.0.1:8765", t.TempDir())
	if st := getStatus(t, h); st.State != stateReadOnly {
		t.Fatalf("state = %s, want %s", st.State, stateReadOnly)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/link", strings.NewReader("{}")))
	if w.Code != http.StatusConflict {
		t.Fatalf("link on a read-only instance: got %d, want 409", w.Code)
	}
}
