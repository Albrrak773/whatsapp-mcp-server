package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// The status page: connection state, archive summary, the latest messages, and linking with a
// QR code in the browser. Served next to /mcp on the HTTP transport.

//go:embed web/index.html
var pageHTML []byte

const stateReadOnly = "read_only" // another process owns the connection

type pageStatus struct {
	State    string        `json:"state"`
	Account  string        `json:"account,omitempty"`
	Name     string        `json:"name,omitempty"`
	Since    string        `json:"since,omitempty"` // when the current state began, if known
	Error    string        `json:"error,omitempty"` // last connect error, or why WhatsApp logged out
	Retry    *pageRetry    `json:"retry,omitempty"`
	Link     *pageLink     `json:"link,omitempty"`
	History  *pageHistory  `json:"history,omitempty"`
	Archive  pageArchive   `json:"archive"`
	Messages []pageMessage `json:"messages"`
	Endpoint string        `json:"endpoint"`
}

type pageRetry struct {
	Attempt int    `json:"attempt"`
	NextAt  string `json:"next_at"`
}

type pageLink struct {
	QR        string `json:"qr,omitempty"` // PNG data URL
	ExpiresAt string `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

type pageHistory struct {
	Stored int64  `json:"stored"`
	LastAt string `json:"last_at,omitempty"`
}

type pageArchive struct {
	Chats           int    `json:"chats"`
	Messages        int    `json:"messages"`
	Oldest          string `json:"oldest,omitempty"`
	Newest          string `json:"newest,omitempty"`
	LastHistorySync string `json:"last_history_sync,omitempty"`
}

type pageMessage struct {
	ChatJID  string `json:"chat_jid"`
	Chat     string `json:"chat"`
	Group    bool   `json:"group"`
	Sender   string `json:"sender"`
	FromMe   bool   `json:"from_me"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	FileName string `json:"file_name,omitempty"`
	Seconds  int    `json:"seconds,omitempty"`
	Time     string `json:"time"`
}

type page struct {
	app      *App
	endpoint string

	qrMu      sync.Mutex
	qrCode    string // last QR payload rendered, and its PNG data URL
	qrDataURL string
}

func newPageHandler(app *App, addr string) http.Handler {
	p := &page{app: app, endpoint: "http://" + addr + "/mcp"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(pageHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { p.reply(w, r, nil) })
	mux.HandleFunc("POST /api/link", p.action(func(r *http.Request, wa *WA) error {
		var in struct {
			FullHistory bool `json:"full_history"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		return wa.StartLink(in.FullHistory)
	}))
	mux.HandleFunc("POST /api/link/cancel", p.action(func(_ *http.Request, wa *WA) error { wa.CancelLink(); return nil }))
	mux.HandleFunc("POST /api/retry", p.action(func(_ *http.Request, wa *WA) error { wa.RetryNow(); return nil }))
	mux.HandleFunc("POST /api/link/phone", func(w http.ResponseWriter, r *http.Request) {
		if app.wa == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": errReadOnly.Error()})
			return
		}
		var in struct {
			Phone string `json:"phone"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		code, err := app.wa.PairWithPhone(ctx, in.Phone)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"code": code})
	})
	return mux
}

var errReadOnly = errors.New("this instance is read-only: another whatsapp-mcp process owns the WhatsApp connection")

// action runs fn against the connection owner, then answers with the fresh status.
func (p *page) action(fn func(*http.Request, *WA) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.app.wa == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": errReadOnly.Error()})
			return
		}
		p.reply(w, r, fn(r, p.app.wa))
	}
}

func (p *page) reply(w http.ResponseWriter, r *http.Request, actionErr error) {
	if actionErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": actionErr.Error()})
		return
	}
	st, err := p.status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (p *page) status(ctx context.Context) (*pageStatus, error) {
	a := p.app
	stats, err := a.store.Stats(ctx)
	if err != nil {
		return nil, err
	}
	out := &pageStatus{
		State:    stateReadOnly,
		Endpoint: p.endpoint,
		Archive:  pageArchive{Chats: stats.Chats, Messages: stats.Messages, LastHistorySync: stats.LastHistorySync},
		Messages: []pageMessage{},
	}
	if !stats.Oldest.IsZero() {
		out.Archive.Oldest, out.Archive.Newest = stats.Oldest.Format(time.RFC3339), stats.Newest.Format(time.RFC3339)
	}
	if wa := a.wa; wa != nil {
		out.State = wa.State()
		cli := wa.Client()
		if cli.Store.ID != nil {
			out.Account = "+" + cli.Store.ID.User
			out.Name = cli.Store.PushName
		}
		link := wa.LinkStatus()
		switch out.State {
		case stateConnected:
			out.Since = nanosRFC3339(wa.connectedAt.Load())
		case stateSyncing:
			out.Since = link.LinkedAt.Format(time.RFC3339)
			out.History = &pageHistory{Stored: link.History}
			if last := wa.lastHistoryEvt.Load(); last > 0 && time.Unix(0, last).After(link.LinkedAt) {
				out.History.LastAt = nanosRFC3339(last)
			}
		case stateReconnecting:
			out.Since = nanosRFC3339(wa.offlineSince.Load())
			if e := wa.connectErr.Load(); e != nil {
				out.Error = *e
			}
			if next := wa.nextRetryAt.Load(); next > time.Now().UnixNano() {
				out.Retry = &pageRetry{Attempt: int(wa.retryAttempt.Load()), NextAt: nanosRFC3339(next)}
			}
		case stateLoggedOut:
			out.Error = a.store.Meta(ctx, "logged_out_reason")
			if r := wa.loggedOut.Load(); r != nil {
				out.Error = *r
			}
			out.Since = a.store.Meta(ctx, "logged_out_at")
		}
		if out.State == stateLinking || link.Err != "" {
			out.Link = &pageLink{Error: link.Err}
			if link.QR != "" {
				out.Link.QR = p.qrImage(link.QR)
				out.Link.ExpiresAt = link.QRExpires.Format(time.RFC3339)
			}
		}
	}
	rows, _, err := a.store.Messages(ctx, MessageFilter{Limit: 8})
	if err != nil {
		return nil, err
	}
	names := a.store.Names(ctx)
	for i := len(rows) - 1; i >= 0; i-- { // newest first
		m := rows[i]
		chat := names[m.ChatJID]
		if chat == "" {
			chat = a.personName(names, m.ChatJID, "", false)
		}
		out.Messages = append(out.Messages, pageMessage{
			ChatJID: m.ChatJID, Chat: chat, Group: strings.HasSuffix(m.ChatJID, "@g.us"),
			Sender: a.personName(names, m.SenderJID, m.PushName, m.FromMe), FromMe: m.FromMe,
			Kind: m.Kind, Text: truncate(m.Text, 200), FileName: m.FileName, Seconds: m.Seconds,
			Time: m.Timestamp.Format(time.RFC3339),
		})
	}
	return out, nil
}

// qrImage renders the QR payload as a PNG data URL, reusing the last one while it's unchanged.
func (p *page) qrImage(code string) string {
	p.qrMu.Lock()
	defer p.qrMu.Unlock()
	if code != p.qrCode {
		png, err := qrcode.Encode(code, qrcode.Medium, 512)
		if err != nil {
			return ""
		}
		p.qrCode, p.qrDataURL = code, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png)
	}
	return p.qrDataURL
}

func nanosRFC3339(n int64) string {
	if n == 0 {
		return ""
	}
	return time.Unix(0, n).Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

const tokenCookie = "whatsapp_mcp_token"

// guardPage protects the page, which shows private messages and can link a device:
//   - on a loopback address, the Host header must be loopback too (blocks DNS rebinding);
//   - with a token, the browser proves it once with /?token=… and then carries a cookie;
//   - POSTs must be JSON and not cross-site, so other websites can't trigger actions.
func guardPage(addr, token string, next http.Handler) http.Handler {
	loopback := isLoopback(addr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopback && !isLoopback(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if token != "" {
			if q := r.URL.Query().Get("token"); q != "" && subtle.ConstantTimeCompare([]byte(q), []byte(token)) == 1 {
				http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
				return
			}
			c, err := r.Cookie(tokenCookie)
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) != 1 {
				http.Error(w, "unauthorized: open this page once as /?token=<your token>", http.StatusUnauthorized)
				return
			}
		}
		if r.Method == http.MethodPost {
			site := r.Header.Get("Sec-Fetch-Site")
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || (site != "" && site != "same-origin") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
