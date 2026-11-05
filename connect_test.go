package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/store"
)

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, w := range want {
		if got := startupBackoff.delay(i); got != w*time.Second {
			t.Errorf("delay(%d) = %v, want %v", i, got, w*time.Second)
		}
	}
	if got := startupBackoff.delay(1000); got != 5*time.Minute {
		t.Errorf("delay(1000) = %v, want the 5m cap", got)
	}
}

func TestIsTransientConnectErr(t *testing.T) {
	dnsErr := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "Temporary failure in name resolution", Name: "web.whatsapp.com", IsTemporary: true}}
	cases := []struct {
		err  error
		want bool
	}{
		{dnsErr, true},
		{syscall.ECONNREFUSED, true},
		{syscall.ENETUNREACH, true},
		{socket.ErrDialFailed, true},
		{socket.ErrWithStatusCode{StatusCode: 503}, true},
		{socket.ErrWithStatusCode{StatusCode: 403}, false},
		{store.ErrDeviceDeleted, false},
		{errors.New("something else"), false},
	}
	for _, c := range cases {
		if got := isTransientConnectErr(c.err); got != c.want {
			t.Errorf("isTransientConnectErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

var fastBackoff = backoff{min: time.Millisecond, max: 4 * time.Millisecond}

func TestKeepConnectingRetriesUntilConnected(t *testing.T) {
	var calls, fails int
	connect := func(context.Context) error {
		calls++
		if calls <= 5 {
			return syscall.ENETUNREACH
		}
		return nil
	}
	err := keepConnecting(context.Background(), zerolog.Nop(), fastBackoff, connect, func(error) { fails++ })
	if err != nil || calls != 6 || fails != 5 {
		t.Fatalf("err=%v calls=%d fails=%d, want nil/6/5", err, calls, fails)
	}
}

func TestKeepConnectingStopsOnPermanentError(t *testing.T) {
	var calls int
	connect := func(context.Context) error { calls++; return store.ErrDeviceDeleted }
	err := keepConnecting(context.Background(), zerolog.Nop(), fastBackoff, connect, nil)
	if !errors.Is(err, store.ErrDeviceDeleted) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want ErrDeviceDeleted after 1 call", err, calls)
	}
}

func TestKeepConnectingStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	connect := func(context.Context) error {
		if calls.Add(1) == 3 {
			cancel()
		}
		return syscall.ECONNREFUSED
	}
	err := keepConnecting(ctx, zerolog.Nop(), backoff{min: time.Millisecond, max: time.Hour}, connect, nil)
	if !errors.Is(err, context.Canceled) || calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d, want Canceled after 3 calls", err, calls.Load())
	}
}

// A boot without network, end to end through whatsmeow: every dial fails DNS resolution the
// way it did in the journal. The real Connect error must count as transient, keep being
// retried, show up in the tools' error, and the loop must stop cleanly on shutdown.
func TestSimulatedNoNetworkStart(t *testing.T) {
	_, wa := setup(t) // linked test device
	var dials atomic.Int32
	noDNS := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "Temporary failure in name resolution", Name: "web.whatsapp.com", IsTemporary: true}}
	}}}
	wa.Client.SetWebsocketHTTPClient(noDNS)
	wa.Client.SetPreLoginHTTPClient(noDNS)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var failures atomic.Int32
	go func() {
		done <- keepConnecting(ctx, zerolog.Nop(), fastBackoff, wa.Client.ConnectContext, func(err error) {
			msg := err.Error()
			wa.connectErr.Store(&msg)
			failures.Add(1)
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for failures.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if failures.Load() < 4 || dials.Load() < 4 {
		t.Fatalf("expected repeated attempts, got failures=%d dials=%d", failures.Load(), dials.Load())
	}
	app := &App{store: wa.store, wa: wa}
	if err := app.online(); err == nil || !strings.Contains(err.Error(), "retrying in the background") || !strings.Contains(err.Error(), "name resolution") {
		t.Fatalf("online() = %v, want the retrying message with the DNS error", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("keepConnecting returned %v after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keepConnecting did not stop after cancel")
	}
}

func TestOnlineAfterLogout(t *testing.T) {
	_, wa := setup(t)
	reason := "401: logged out from another device"
	wa.loggedOut.Store(&reason)
	err := (&App{store: wa.store, wa: wa}).online()
	if err == nil || !strings.Contains(err.Error(), "whatsapp-mcp login") || !strings.Contains(err.Error(), reason) {
		t.Fatalf("online() = %v, want a re-link hint with the reason", err)
	}
}
