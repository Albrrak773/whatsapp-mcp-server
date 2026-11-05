package main

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/store"
)

// backoff is a capped exponential delay: min, 2*min, 4*min, ... up to max.
type backoff struct{ min, max time.Duration }

// startupBackoff paces connect attempts while the network isn't up yet (e.g. right after boot).
var startupBackoff = backoff{min: 2 * time.Second, max: 5 * time.Minute}

// delay is the wait before retry number attempt (0-based).
func (b backoff) delay(attempt int) time.Duration {
	d := b.min
	for i := 0; i < attempt && d < b.max; i++ {
		d *= 2
	}
	return min(d, b.max)
}

// isTransientConnectErr reports whether a failed Connect is worth retrying: network trouble
// (DNS, refused, unreachable, timeouts) or a 5xx/408 from the websocket endpoint. It mirrors
// whatsmeow's own (unexported) isRetryableConnectError. A logged-out session is never transient.
func isTransientConnectErr(err error) bool {
	if errors.Is(err, store.ErrDeviceDeleted) {
		return false
	}
	if exhttp.IsNetworkError(err) || errors.Is(err, socket.ErrDialFailed) {
		return true
	}
	var statusErr socket.ErrWithStatusCode
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case 408, 500, 501, 502, 503, 504:
			return true
		}
	}
	return false
}

// keepConnecting calls connect until it succeeds, fails permanently, or ctx ends, waiting
// b.delay between attempts; a receive on wake ends the current wait early (nil never does).
// onFail runs after every failed attempt with the wait before the next one, 0 when it gives up
// (nil is fine). It returns nil once connected, the permanent error, or ctx.Err().
//
// Only the first connection needs this: once connected, whatsmeow reconnects by itself
// after drops. A logged-out session (401) is not a Connect error; whatsmeow reports it with
// a LoggedOut event after the socket is up and deletes the device, so later attempts fail
// with store.ErrDeviceDeleted, which is permanent.
func keepConnecting(ctx context.Context, log zerolog.Logger, b backoff, connect func(context.Context) error, wake <-chan struct{}, onFail func(err error, attempt int, wait time.Duration)) error {
	for attempt := 0; ; attempt++ {
		err := connect(ctx)
		if err == nil {
			if attempt > 0 {
				log.Info().Int("attempts", attempt+1).Msg("connected to WhatsApp after retrying")
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := b.delay(attempt)
		if !isTransientConnectErr(err) {
			wait = 0
		}
		if onFail != nil {
			onFail(err, attempt+1, wait)
		}
		if wait == 0 {
			return err
		}
		log.Warn().Err(err).Dur("retry_in", wait).Int("attempt", attempt+1).
			Msg("connect to WhatsApp failed; serving the archive meanwhile and retrying")
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		case <-wake:
			t.Stop()
		}
	}
}
