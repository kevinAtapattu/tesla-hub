package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kevin/tesla-hub/internal/store"
)

// artSearch is Apple's public iTunes search, which needs no key. Tests point
// it elsewhere.
var artSearch = "https://itunes.apple.com/search"

// coverArt fills in Art for the tracks from the cache, looking up the ones it
// hasn't seen. The car doesn't send artwork, only the title and artist, so this
// works the same for Spotify, radio apps and phones on Bluetooth. Lookups are
// capped at a couple of seconds so a slow search never holds up the page; a
// track that misses out gets its art on the next view.
func (s *Server) coverArt(ctx context.Context, tracks ...*Track) {
	var keys []store.ArtKey
	seen := map[store.ArtKey]bool{}
	for _, t := range tracks {
		if t == nil || t.Title == "" { // a radio station on its own has no cover
			continue
		}
		k := store.ArtKey{Title: t.Title, Artist: t.Artist}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	found, err := s.Store.MediaArt(ctx, keys)
	if err != nil {
		return
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	lookCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	for _, k := range keys {
		if _, ok := found[k]; ok {
			continue
		}
		wg.Add(1)
		go func(k store.ArtKey) {
			defer wg.Done()
			u, err := lookupArt(lookCtx, k)
			if err != nil {
				return // try again next time
			}
			s.Store.SaveMediaArt(context.WithoutCancel(ctx), k, u)
			mu.Lock()
			found[k] = u
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	for _, t := range tracks {
		if t != nil && t.Title != "" {
			t.Art = found[store.ArtKey{Title: t.Title, Artist: t.Artist}]
		}
	}
}

// lookupArt asks the iTunes search for the song and returns a 300px cover, or
// "" when there's no match.
func lookupArt(ctx context.Context, k store.ArtKey) (string, error) {
	q := url.Values{"term": {strings.TrimSpace(k.Title + " " + k.Artist)}, "entity": {"song"}, "limit": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artSearch+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", &url.Error{Op: "search", URL: artSearch, Err: http.ErrNotSupported}
	}
	var v struct {
		Results []struct {
			Artwork string `json:"artworkUrl100"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return "", err
	}
	if len(v.Results) == 0 || v.Results[0].Artwork == "" {
		return "", nil
	}
	return strings.Replace(v.Results[0].Artwork, "100x100bb", "300x300bb", 1), nil
}
