package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kevin/tesla-hub/internal/store"
)

func TestLookupArt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("term") == "Nightcall Kavinsky" {
			w.Write([]byte(`{"resultCount":1,"results":[{"artworkUrl100":"https://img/x/100x100bb.jpg","previewUrl":"https://audio/x.m4a"}]}`))
			return
		}
		w.Write([]byte(`{"resultCount":0,"results":[]}`))
	}))
	defer srv.Close()
	defer func(old string) { artSearch = old }(artSearch)
	artSearch = srv.URL

	if v, err := lookupArt(context.Background(), store.ArtKey{Title: "Nightcall", Artist: "Kavinsky"}); err != nil ||
		v != (store.Song{Art: "https://img/x/300x300bb.jpg", Preview: "https://audio/x.m4a"}) {
		t.Errorf("got %+v, %v", v, err)
	}
	if v, err := lookupArt(context.Background(), store.ArtKey{Title: "Nothing"}); err != nil || v != (store.Song{}) {
		t.Errorf("no match: got %+v, %v", v, err)
	}
}
