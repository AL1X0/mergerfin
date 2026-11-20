package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/AL1X0/mergerfin/internal/db"
)

func TestNewClientTransportPoolSettings(t *testing.T) {
	c := NewClient()
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("expected *http.Transport on Client, got %T", c.httpClient.Transport)
	}
	if tr.MaxIdleConns != 256 {
		t.Fatalf("MaxIdleConns = %d, want 256", tr.MaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != 64 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 64", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Fatalf("IdleConnTimeout = %s, want 90s", tr.IdleConnTimeout)
	}
}

func TestFetchImageForwardsResizeParamsAndStripsTag(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "image/webp")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("RIFFwebp"))
	}))
	defer srv.Close()

	c := NewClient()
	inQuery := url.Values{
		"fillWidth":       {"300"},
		"fillHeight":      {"450"},
		"maxWidth":        {"400"},
		"maxHeight":       {"600"},
		"width":           {"320"},
		"height":          {"480"},
		"quality":         {"90"},
		"format":          {"webp"},
		"blur":            {"2"},
		"backgroundColor": {"000000"},
		"foregroundLayer": {"0.5"},
		"tag":             {"synthetic-virtual-tag"},
		"Tag":             {"synthetic-virtual-tag-upper"},
		"api_key":         {"secret-should-not-forward"},
	}

	data, ct, status, err := c.FetchImage(context.Background(), db.UpstreamServer{
		ID:          "srv1",
		BaseURL:     srv.URL,
		AccessToken: "tok",
	}, "remote-1", "Primary", inQuery)
	if err != nil {
		t.Fatalf("FetchImage error: %v", err)
	}
	if status != http.StatusOK || ct != "image/webp" || string(data) != "RIFFwebp" {
		t.Fatalf("unexpected FetchImage result: status=%d ct=%q data=%q", status, ct, string(data))
	}

	for _, want := range []struct {
		k, v string
	}{
		{"fillWidth", "300"},
		{"fillHeight", "450"},
		{"maxWidth", "400"},
		{"maxHeight", "600"},
		{"width", "320"},
		{"height", "480"},
		{"quality", "90"},
		{"format", "webp"},
		{"blur", "2"},
		{"backgroundColor", "000000"},
		{"foregroundLayer", "0.5"},
	} {
		if got := gotQuery.Get(want.k); got != want.v {
			t.Fatalf("forwarded query %s = %q, want %q", want.k, got, want.v)
		}
	}

	if gotQuery.Has("tag") || gotQuery.Has("Tag") {
		t.Fatalf("expected synthetic tag param not to be forwarded upstream, got %v", gotQuery)
	}
	if gotQuery.Has("api_key") {
		t.Fatalf("expected api_key param not to be forwarded upstream, got %v", gotQuery)
	}
}
