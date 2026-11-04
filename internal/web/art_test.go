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
			w.Write([]byte(`{"resultCount":1,"results":[{"artworkUrl100":"https://img/x/100x100bb.jpg"}]}`))
			return
		}
		w.Write([]byte(`{"resultCount":0,"results":[]}`))
	}))
	defer srv.Close()
	defer func(old string) { artSearch = old }(artSearch)
	artSearch = srv.URL

	if u, err := lookupArt(context.Background(), store.ArtKey{Title: "Nightcall", Artist: "Kavinsky"}); err != nil || u != "https://img/x/300x300bb.jpg" {
		t.Errorf("got %q, %v", u, err)
	}
	if u, err := lookupArt(context.Background(), store.ArtKey{Title: "Nothing"}); err != nil || u != "" {
		t.Errorf("no match: got %q, %v", u, err)
	}
}
