package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type fakeWhatsApp struct {
	online    bool
	info      *types.ProfilePictureInfo
	err       error
	fetches   int
	lastID    string
	downloads int
}

func (f *fakeWhatsApp) cache(t *testing.T) *avatarCache {
	return &avatarCache{
		dir:    t.TempDir(),
		online: func() bool { return f.online },
		fetch: func(_ context.Context, _ types.JID, existingID string) (*types.ProfilePictureInfo, error) {
			f.fetches++
			f.lastID = existingID
			return f.info, f.err
		},
		download: func(context.Context, string) ([]byte, error) { f.downloads++; return []byte("\xff\xd8\xffJPEG"), nil },
	}
}

// age pretends the cached answer for jid was checked d ago.
func age(t *testing.T, c *avatarCache, jid types.JID, d time.Duration) {
	t.Helper()
	path := filepath.Join(c.dir, safeName(jid.String())+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m avatarMeta
	_ = json.Unmarshal(data, &m)
	m.Checked = m.Checked.Add(-d)
	data, _ = json.Marshal(m)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAvatarFetchedOnceThenCached(t *testing.T) {
	f := &fakeWhatsApp{online: true, info: &types.ProfilePictureInfo{ID: "pic1", URL: "https://cdn.example/p.jpg"}}
	c := f.cache(t)
	ctx := context.Background()
	for range 3 {
		img, err := c.Get(ctx, alice)
		if err != nil || string(img) != "\xff\xd8\xffJPEG" {
			t.Fatalf("Get = %q, %v", img, err)
		}
	}
	if f.fetches != 1 || f.downloads != 1 {
		t.Fatalf("fetches=%d downloads=%d, want 1/1", f.fetches, f.downloads)
	}

	// After the TTL it asks again with the known ID; "unchanged" (nil) keeps the picture.
	age(t, c, alice, avatarTTL+time.Minute)
	f.info = nil
	if img, err := c.Get(ctx, alice); err != nil || len(img) == 0 {
		t.Fatalf("after refresh: %q, %v", img, err)
	}
	if f.fetches != 2 || f.lastID != "pic1" || f.downloads != 1 {
		t.Fatalf("refresh: fetches=%d lastID=%q downloads=%d", f.fetches, f.lastID, f.downloads)
	}
}

func TestAvatarHiddenIsCached(t *testing.T) {
	f := &fakeWhatsApp{online: true, err: whatsmeow.ErrProfilePictureUnauthorized}
	c := f.cache(t)
	for range 2 {
		if _, err := c.Get(context.Background(), bob); !errors.Is(err, errNoAvatar) {
			t.Fatalf("hidden picture: %v", err)
		}
	}
	if f.fetches != 1 {
		t.Fatalf("fetches=%d, want the 'hidden' answer cached", f.fetches)
	}
}

func TestAvatarOfflineServesStaleAndDoesNotAsk(t *testing.T) {
	f := &fakeWhatsApp{online: true, info: &types.ProfilePictureInfo{ID: "g1", URL: "u"}}
	c := f.cache(t)
	if _, err := c.Get(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	age(t, c, group, avatarTTL+time.Minute)
	f.online = false
	if img, err := c.Get(context.Background(), group); err != nil || len(img) == 0 {
		t.Fatalf("offline with a stale picture: %q, %v", img, err)
	}
	if _, err := c.Get(context.Background(), alice); !errors.Is(err, errNoAvatar) {
		t.Fatalf("offline without a picture: %v", err)
	}
	if f.fetches != 1 {
		t.Fatalf("fetches=%d, want none while offline", f.fetches)
	}
}

func TestAvatarTransientErrorIsNotCached(t *testing.T) {
	f := &fakeWhatsApp{online: true, err: errors.New("timeout")}
	c := f.cache(t)
	_, _ = c.Get(context.Background(), alice)
	f.err, f.info = nil, &types.ProfilePictureInfo{ID: "p", URL: "u"}
	if _, err := c.Get(context.Background(), alice); err != nil {
		t.Fatalf("second try after a transient error: %v", err)
	}
	if f.fetches != 2 {
		t.Fatalf("fetches=%d, want a retry", f.fetches)
	}
}

func TestAvatarHTTP(t *testing.T) {
	f := &fakeWhatsApp{online: true, err: whatsmeow.ErrProfilePictureNotSet}
	c := f.cache(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/avatar/x", nil)
	r.SetPathValue("jid", "not a jid@@")
	c.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("bad jid: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r.SetPathValue("jid", alice.String())
	c.ServeHTTP(w, r)
	if w.Code != 404 || w.Header().Get("Cache-Control") == "" {
		t.Fatalf("no picture: %d, cache %q", w.Code, w.Header().Get("Cache-Control"))
	}
}
