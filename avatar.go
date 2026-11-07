package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Profile pictures for the status page. They aren't in the archive: each one is a live request
// to WhatsApp, so the small preview is cached under <data-dir>/avatars and checked again after
// avatarTTL. "Hidden from you" and "no picture" answers are cached too, so they aren't re-asked.

const avatarTTL = 24 * time.Hour

var errNoAvatar = errors.New("no profile picture")

type avatarMeta struct {
	ID      string    `json:"id,omitempty"` // WhatsApp's picture ID, to ask only for changes
	Missing bool      `json:"missing,omitempty"`
	Checked time.Time `json:"checked"`
}

type avatarCache struct {
	dir string
	// Seams for tests; wired to whatsmeow and plain HTTP by newAvatarCache.
	online   func() bool
	fetch    func(ctx context.Context, jid types.JID, existingID string) (*types.ProfilePictureInfo, error)
	download func(ctx context.Context, url string) ([]byte, error)

	mu sync.Mutex // one fetch at a time: the page asks for a handful, and it keeps WhatsApp calm
}

func newAvatarCache(dataDir string, wa *WA) *avatarCache {
	c := &avatarCache{dir: filepath.Join(dataDir, "avatars"), online: func() bool { return false }}
	if wa != nil {
		c.online = func() bool { return wa.Client().IsConnected() && wa.Client().IsLoggedIn() }
		c.fetch = func(ctx context.Context, jid types.JID, existingID string) (*types.ProfilePictureInfo, error) {
			return wa.Client().GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: true, ExistingID: existingID})
		}
	}
	httpc := &http.Client{Timeout: 15 * time.Second}
	c.download = func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		res, err := httpc.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download profile picture: HTTP %d", res.StatusCode)
		}
		return io.ReadAll(io.LimitReader(res.Body, 2<<20))
	}
	return c
}

// Get returns the picture for a chat, from the cache when it is fresh, or errNoAvatar.
// A stale picture is still returned when WhatsApp can't be asked right now.
func (c *avatarCache) Get(ctx context.Context, jid types.JID) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	base := filepath.Join(c.dir, safeName(jid.ToNonAD().String()))
	var meta avatarMeta
	if data, err := os.ReadFile(base + ".json"); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	cached := func() ([]byte, error) {
		if meta.Missing {
			return nil, errNoAvatar
		}
		if img, err := os.ReadFile(base + ".jpg"); err == nil {
			return img, nil
		}
		return nil, errNoAvatar
	}
	if time.Since(meta.Checked) < avatarTTL || !c.online() || c.fetch == nil {
		return cached()
	}

	info, err := c.fetch(ctx, jid, meta.ID)
	switch {
	case errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized), errors.Is(err, whatsmeow.ErrProfilePictureNotSet):
		_ = os.Remove(base + ".jpg")
		meta = avatarMeta{Missing: true}
	case err != nil:
		return cached() // try again on the next request
	case info == nil: // unchanged since meta.ID
	default:
		img, err := c.download(ctx, info.URL)
		if err != nil {
			return cached()
		}
		if err := os.MkdirAll(c.dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(base+".jpg", img, 0o600); err != nil {
			return nil, err
		}
		meta = avatarMeta{ID: info.ID}
	}
	meta.Checked = time.Now()
	if err := os.MkdirAll(c.dir, 0o700); err == nil {
		data, _ := json.Marshal(meta)
		_ = os.WriteFile(base+".json", data, 0o600)
	}
	return cached()
}

func (c *avatarCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	jid, err := types.ParseJID(r.PathValue("jid"))
	if err != nil || jid.IsEmpty() {
		http.Error(w, "bad chat id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	img, err := c.Get(ctx, jid)
	// Let the browser keep answers for an hour so re-renders don't ask again.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if err != nil {
		http.Error(w, "no picture", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(img))
	_, _ = w.Write(img)
}
