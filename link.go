package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// Linking from the status page: the running server shows the QR codes itself, so nothing has
// to be stopped to link again. One session at a time; it ends on success, timeout, or cancel.

// historyQuiet is how long the page keeps showing "receiving history" after the last batch.
const historyQuiet = 90 * time.Second

type linkState struct {
	active    bool
	qr        string    // current QR payload; "" until the socket is up
	qrExpires time.Time // when the phone stops accepting this code
	err       string    // why the last session ended badly, shown until the next attempt
	cancel    context.CancelFunc

	linkedAt    time.Time // last successful link in this process
	historyBase int64     // historyMessages at linkedAt, to count only this link's history
}

// Connection states shown on the page and in whatsapp_status.mode.
const (
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
	stateLoggedOut    = "logged_out" // WhatsApp ended the session
	stateNotLinked    = "not_linked" // never linked, or unlinked by hand
	stateLinking      = "linking"
	stateSyncing      = "syncing" // just linked; the phone is still sending history
)

// State is the connection state, most specific first.
func (w *WA) State() string {
	w.linkMu.Lock()
	linking, linkedAt, base := w.link.active, w.link.linkedAt, w.link.historyBase
	w.linkMu.Unlock()
	cli := w.Client()
	switch {
	case linking:
		return stateLinking
	case w.loggedOut.Load() != nil:
		return stateLoggedOut
	case cli.Store.ID == nil:
		if w.store.Meta(context.Background(), "logged_out_reason") != "" {
			return stateLoggedOut
		}
		return stateNotLinked
	case !cli.IsConnected():
		return stateReconnecting
	case !linkedAt.IsZero() && time.Since(w.lastActivity(linkedAt, base)) < historyQuiet:
		return stateSyncing
	}
	return stateConnected
}

// lastActivity is the later of the link time and the last history batch after it.
func (w *WA) lastActivity(linkedAt time.Time, base int64) time.Time {
	if w.historyMessages.Load() > base {
		if last := time.Unix(0, w.lastHistoryEvt.Load()); last.After(linkedAt) {
			return last
		}
	}
	return linkedAt
}

// recordConnectFailure is keepConnecting's onFail hook: it feeds the page's retry countdown.
func (w *WA) recordConnectFailure(err error, attempt int, wait time.Duration) {
	msg := err.Error()
	w.connectErr.Store(&msg)
	w.offlineSince.CompareAndSwap(0, time.Now().UnixNano())
	w.retryAttempt.Store(int32(attempt))
	if wait > 0 {
		w.nextRetryAt.Store(time.Now().Add(wait).UnixNano())
	} else {
		w.nextRetryAt.Store(0)
	}
}

// RetryNow cuts the startup retry wait short, or reconnects directly once that loop is done
// (for example after whatsmeow's own reconnects gave up).
func (w *WA) RetryNow() {
	select {
	case w.wake <- struct{}{}:
	default:
		go func() {
			if err := w.Client().Connect(); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
				w.recordConnectFailure(err, int(w.retryAttempt.Load())+1, 0)
			}
		}()
	}
}

// StartLink begins a linking session; the QR codes then show up in LinkStatus.
// fullHistory asks the phone for its whole history instead of the recent default.
func (w *WA) StartLink(fullHistory bool) error {
	w.linkMu.Lock()
	defer w.linkMu.Unlock()
	if w.link.active {
		return nil
	}
	cli := w.Client()
	if cli.Store.ID != nil && w.loggedOut.Load() == nil {
		return errors.New("already linked")
	}
	if cli.Store.Deleted {
		// WhatsApp deleted the old device on logout; pair a fresh one.
		cli.Disconnect()
		cli.RemoveEventHandlers()
		cli = w.newClient(w.container.NewDevice())
		w.client.Store(cli)
	}
	// Device props are global and only read while pairing.
	store.DeviceProps.RequireFullSync = proto.Bool(fullHistory)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	qrs, err := cli.GetQRChannel(ctx)
	if err != nil {
		cancel()
		return err
	}
	if err := cli.Connect(); err != nil {
		cancel()
		return fmt.Errorf("can't reach WhatsApp: %w", err)
	}
	w.link = linkState{active: true, cancel: cancel, linkedAt: w.link.linkedAt, historyBase: w.link.historyBase}
	go w.watchLink(cli, qrs, cancel)
	return nil
}

func (w *WA) watchLink(cli *whatsmeow.Client, qrs <-chan whatsmeow.QRChannelItem, cancel context.CancelFunc) {
	defer cancel()
	end := func(errMsg string) {
		w.linkMu.Lock()
		w.link.active, w.link.qr, w.link.err = false, "", errMsg
		w.linkMu.Unlock()
	}
	for item := range qrs {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			w.linkMu.Lock()
			w.link.qr, w.link.qrExpires = item.Code, time.Now().Add(item.Timeout)
			w.linkMu.Unlock()
		case whatsmeow.QRChannelSuccess.Event:
			w.loggedOut.Store(nil)
			w.connectErr.Store(nil)
			ctx := context.Background()
			_ = w.store.SetMeta(ctx, "logged_out_reason", "")
			_ = w.store.SetMeta(ctx, "logged_out_at", "")
			w.linkMu.Lock()
			w.link = linkState{linkedAt: time.Now(), historyBase: w.historyMessages.Load()}
			w.linkMu.Unlock()
			w.log.Info().Str("account", cli.Store.ID.ToNonAD().String()).Msg("linked from the status page")
			return
		case whatsmeow.QRChannelTimeout.Event:
			end("The codes expired before one was scanned. Start again when your phone is ready.")
			return
		case whatsmeow.QRChannelClientOutdated.Event:
			end("WhatsApp rejected this client version. Update whatsmeow and rebuild.")
			return
		case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
			end("Your phone needs multi-device support turned on.")
			return
		case whatsmeow.QRChannelEventError:
			end(fmt.Sprintf("Pairing failed: %v", item.Error))
			return
		default:
			if item.Error != nil {
				end(fmt.Sprintf("Pairing failed (%s): %v", item.Event, item.Error))
				return
			}
		}
	}
	// Channel closed without a result: cancelled from the page.
	end("")
}

// CancelLink stops the linking session; whatsmeow disconnects the pairing socket.
func (w *WA) CancelLink() {
	w.linkMu.Lock()
	cancel := w.link.cancel
	w.linkMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// PairWithPhone returns an 8-character code to type on the phone instead of scanning.
func (w *WA) PairWithPhone(ctx context.Context, phone string) (string, error) {
	w.linkMu.Lock()
	ready := w.link.active && w.link.qr != ""
	w.linkMu.Unlock()
	if !ready {
		return "", errors.New("start linking first and wait for the QR code")
	}
	return w.Client().PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
}

// LinkSnapshot is what the page needs about the linking session.
type LinkSnapshot struct {
	QR        string
	QRExpires time.Time
	Err       string
	LinkedAt  time.Time
	History   int64 // history messages stored since the link
}

func (w *WA) LinkStatus() LinkSnapshot {
	w.linkMu.Lock()
	defer w.linkMu.Unlock()
	snap := LinkSnapshot{QR: w.link.qr, QRExpires: w.link.qrExpires, Err: w.link.err, LinkedAt: w.link.linkedAt}
	if !w.link.linkedAt.IsZero() {
		snap.History = w.historyMessages.Load() - w.link.historyBase
	}
	return snap
}
