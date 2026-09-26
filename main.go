// whatsapp-mcp: an MCP server giving read access to a WhatsApp account.
//
// It links to the account as a companion device (like WhatsApp Web) using whatsmeow,
// mirrors live messages and history syncs into a local SQLite archive, and serves
// that archive over MCP (stdio or streamable HTTP).
package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/mdp/qrterminal/v3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

const usage = `whatsapp-mcp — read-only WhatsApp MCP server

Usage:
  whatsapp-mcp login  [--phone +4915112345678] [--full-history] [--history-days N] [--sync-idle 90s]
  whatsapp-mcp serve  [--transport stdio|http] [--addr 127.0.0.1:8765] [--token T]
  whatsapp-mcp status
  whatsapp-mcp logout
  whatsapp-mcp version

Common flags:
  --data-dir DIR    state directory (default $WHATSAPP_MCP_DATA_DIR or ~/.local/share/whatsapp-mcp)
  --log-level LVL   debug, info, warn, error (default info)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// The archive holds private conversations: keep every file we create (db, WAL, media) owner-only.
	syscall.Umask(0o077)
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dataDir := fs.String("data-dir", defaultDataDir(), "state directory")
	logLevel := fs.String("log-level", "info", "log level")

	var err error
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "login":
		phone := fs.String("phone", "", "link with a pairing code for this phone number instead of a QR code")
		full := fs.Bool("full-history", false, "ask the phone for its full history instead of the recent default (only effective at link time)")
		days := fs.Uint("history-days", 0, "with --full-history: limit to this many days (0 = phone default)")
		idle := fs.Duration("sync-idle", 90*time.Second, "after linking, keep syncing until no history arrived for this long")
		_ = fs.Parse(args)
		err = runLogin(ctx, *dataDir, newLogger(*logLevel), *phone, *full, uint32(*days), *idle)
	case "serve":
		transport := fs.String("transport", "stdio", "stdio or http")
		addr := fs.String("addr", "127.0.0.1:8765", "listen address for --transport http")
		token := fs.String("token", os.Getenv("WHATSAPP_MCP_TOKEN"), "bearer token required for --transport http (default $WHATSAPP_MCP_TOKEN)")
		_ = fs.Parse(args)
		err = runServe(ctx, *dataDir, newLogger(*logLevel), *transport, *addr, *token)
	case "status":
		_ = fs.Parse(args)
		err = runStatus(ctx, *dataDir)
	case "logout":
		_ = fs.Parse(args)
		err = runLogout(ctx, *dataDir, newLogger(*logLevel))
	case "version", "--version":
		fmt.Println("whatsapp-mcp", version)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultDataDir() string {
	if d := os.Getenv("WHATSAPP_MCP_DATA_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "whatsapp-mcp")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "whatsapp-mcp")
}

// Logs always go to stderr: stdout is the MCP stdio channel.
func newLogger(level string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	w := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.DateTime, NoColor: !isatty.IsTerminal(os.Stderr.Fd())}
	return zerolog.New(w).Level(lvl).With().Timestamp().Logger()
}

func dbPath(dataDir string) string { return filepath.Join(dataDir, "whatsapp.db") }

// acquireLock makes sure only one process holds the WhatsApp connection. A second linked
// session with the same keys would fight the first one for the socket.
func acquireLock(dataDir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dataDir, "connection.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openOwned(ctx context.Context, dataDir string, log zerolog.Logger) (*sql.DB, *Store, *WA, func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, nil, nil, err
	}
	lock, err := acquireLock(dataDir)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("another whatsapp-mcp process is connected with this data dir (%s)", dataDir)
	}
	db, err := openDB(dbPath(dataDir), false)
	if err != nil {
		lock.Close()
		return nil, nil, nil, nil, err
	}
	st := NewStore(db)
	wa, err := newWA(ctx, db, st, dataDir, log)
	if err != nil {
		db.Close()
		lock.Close()
		return nil, nil, nil, nil, err
	}
	cleanup := func() {
		wa.Client.Disconnect()
		db.Close()
		lock.Close()
	}
	return db, st, wa, cleanup, nil
}

// ---------------------------------------------------------------------------

func runLogin(ctx context.Context, dataDir string, log zerolog.Logger, phone string, full bool, days uint32, idle time.Duration) error {
	if full {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		if days > 0 {
			store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(days)
		}
	}
	_, _, wa, cleanup, err := openOwned(ctx, dataDir, log)
	if err != nil {
		return err
	}
	defer cleanup()
	if wa.Client.Store.ID != nil {
		fmt.Fprintf(os.Stderr, "Already linked as %s. Run `whatsapp-mcp logout` first to link again.\n", wa.Client.Store.ID.ToNonAD())
		return nil
	}

	qrChan, err := wa.Client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := wa.Client.Connect(); err != nil {
		return err
	}
	qrPNG := filepath.Join(dataDir, "login-qr.png")
	defer os.Remove(qrPNG)
	codeShown := false
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if phone != "" {
				if codeShown {
					continue
				}
				code, err := wa.Client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
				if err != nil {
					return fmt.Errorf("request pairing code: %w", err)
				}
				codeShown = true
				fmt.Fprintf(os.Stderr, "\nOn your phone: WhatsApp > Settings > Linked devices > Link a device > Link with phone number instead\nEnter this code: %s\n\n", code)
				continue
			}
			fmt.Fprintln(os.Stderr, "\nScan with WhatsApp > Settings > Linked devices > Link a device:")
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stderr)
			if err := qrcode.WriteFile(item.Code, qrcode.Medium, 512, qrPNG); err == nil {
				fmt.Fprintf(os.Stderr, "(also saved as %s; it refreshes every ~20s)\n", qrPNG)
			}
		case "success":
			fmt.Fprintf(os.Stderr, "\nLinked as %s.\n", wa.Client.Store.ID.ToNonAD())
		case "timeout":
			return errors.New("pairing timed out; run login again")
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing failed: %v", item.Error)
		case "err-client-outdated":
			return errors.New("WhatsApp rejected the client version; update whatsmeow (go get go.mau.fi/whatsmeow@latest) and rebuild")
		default:
			if item.Error != nil {
				return fmt.Errorf("pairing failed (%s): %v", item.Event, item.Error)
			}
			log.Debug().Str("event", item.Event).Msg("pairing event")
		}
	}
	if wa.Client.Store.ID == nil {
		return errors.New("pairing did not complete")
	}

	// The phone pushes the initial history right after linking. Stay connected until it goes quiet.
	fmt.Fprintf(os.Stderr, "Receiving history from the phone (keep it online).\n"+
		"This finishes by itself once no history has arrived for %s. Ctrl-C is safe too: everything received is already saved.\n", idle)
	start := time.Now()
	tty := isatty.IsTerminal(os.Stderr.Fd())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastPrinted := int64(-1)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\nStopped; %d history messages stored. More history keeps arriving while `whatsapp-mcp serve` runs.\n",
				wa.historyMessages.Load())
			return nil
		case <-tick.C:
			ref := start
			if last := wa.lastHistoryEvt.Load(); last > 0 {
				ref = time.Unix(0, last)
			}
			remaining := idle - time.Since(ref)
			stored := wa.historyMessages.Load()
			if remaining <= 0 {
				fmt.Fprintf(os.Stderr, "\nDone: %d history messages stored. Start the server with `whatsapp-mcp serve` (or `make service`).\n", stored)
				return nil
			}
			if tty {
				fmt.Fprintf(os.Stderr, "\r%d history messages stored; finishing in %ds unless more arrives\033[K",
					stored, int(remaining.Seconds())+1)
			} else if stored != lastPrinted {
				fmt.Fprintf(os.Stderr, "%d history messages stored\n", stored)
			}
			lastPrinted = stored
		}
	}
}

func runLogout(ctx context.Context, dataDir string, log zerolog.Logger) error {
	_, _, wa, cleanup, err := openOwned(ctx, dataDir, log)
	if err != nil {
		return err
	}
	defer cleanup()
	if wa.Client.Store.ID == nil {
		fmt.Fprintln(os.Stderr, "Not linked.")
		return nil
	}
	if err := wa.Client.Connect(); err != nil {
		return err
	}
	if err := wa.Client.Logout(ctx); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Unlinked. The message archive in", dbPath(dataDir), "was kept; delete it yourself if you want it gone.")
	return nil
}

func runStatus(ctx context.Context, dataDir string) error {
	db, err := openDB(dbPath(dataDir), true)
	if err != nil {
		return err
	}
	defer db.Close()
	st := NewStore(db)
	if err := st.checkInitialized(ctx); err != nil {
		return err
	}
	app := &App{store: st, mode: "cli"}
	_, out, err := app.status(ctx, nil, StatusIn{})
	if err != nil {
		return err
	}
	fmt.Printf("linked:   %v %s\nchats:    %d\nmessages: %d (%s .. %s)\nlast history sync: %s\n",
		out.LoggedIn, out.Account, out.Chats, out.Messages, out.Oldest, out.Newest, out.LastHistorySync)
	return nil
}

// ---------------------------------------------------------------------------

func runServe(ctx context.Context, dataDir string, log zerolog.Logger, transport, addr, token string) error {
	app, cleanup, err := openForServe(ctx, dataDir, log)
	if err != nil {
		return err
	}
	defer cleanup()
	server := newMCPServer(app)

	switch transport {
	case "stdio":
		return server.Run(ctx, &mcp.StdioTransport{})
	case "http":
		if !isLoopback(addr) && token == "" {
			return errors.New("refusing to serve private messages on a non-loopback address without --token")
		}
		handler := http.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
		if token != "" {
			handler = requireToken(token, handler)
		}
		mux := http.NewServeMux()
		mux.Handle("/mcp", handler)
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		log.Info().Str("url", "http://"+addr+"/mcp").Str("mode", app.mode).Msg("serving MCP over streamable HTTP")
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown transport %q", transport)
	}
}

// openForServe connects to WhatsApp if this process can take the connection lock; otherwise
// (e.g. a second MCP client spawned another stdio instance) it serves the archive read-only.
func openForServe(ctx context.Context, dataDir string, log zerolog.Logger) (*App, func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, err
	}
	_, st, wa, cleanup, err := openOwned(ctx, dataDir, log)
	if err == nil {
		app := &App{store: st, wa: wa, mode: "connected"}
		if wa.Client.Store.ID == nil {
			log.Warn().Msg("no WhatsApp account linked yet: run `whatsapp-mcp login`; serving the (empty) archive")
			app.mode = "not linked"
			return app, cleanup, nil
		}
		if err := wa.Client.Connect(); err != nil {
			log.Error().Err(err).Msg("initial connect failed; will serve the archive only")
		}
		return app, cleanup, nil
	}
	if _, statErr := os.Stat(dbPath(dataDir)); statErr != nil {
		return nil, nil, err
	}
	log.Warn().Msg("another instance owns the WhatsApp connection; serving the archive read-only")
	db, dbErr := openDB(dbPath(dataDir), true)
	if dbErr != nil {
		return nil, nil, dbErr
	}
	st = NewStore(db)
	if err := st.checkInitialized(ctx); err != nil {
		db.Close()
		return nil, nil, err
	}
	return &App{store: st, mode: "read-only (another instance is connected)"}, func() { db.Close() }, nil
}

func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
