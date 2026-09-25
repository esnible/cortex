package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// localStatsUp is what stops localEditTargets handing the edit flow an address
// nothing serves.
//
// The failure it prevents is not a harmless wrong guess: a bad stats URL costs
// the poll 5 transport errors, reaches PollFailure, and RollbackCmd then REVERTS
// an edit that had already applied and hot-reloaded — while blaming a healthy
// proxy. Declining up front is strictly better.
func TestLocalStatsUp(t *testing.T) {
	t.Run("a real reload-status endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/reload/status" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"reloads_ok":3,"reloads_failed":0}`))
		}))
		defer srv.Close()
		if !localStatsUp(srv.URL) {
			t.Error("want true for a server answering /reload/status")
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		// A port that answered and then closed, so the dial is refused rather
		// than hanging until the probe timeout.
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		if localStatsUp(url) {
			t.Error("want false when nothing is listening")
		}
	})

	t.Run("something else holds the port", func(t *testing.T) {
		// The exact shape of the bug: config.Load defaults an absent
		// stats.address to :9093, so abctl can be pointed at a port owned by an
		// unrelated service. A 200 from it must not count.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>some other service</html>"))
		}))
		defer srv.Close()
		if localStatsUp(srv.URL) {
			t.Error("want false for a 200 that is not a reload status")
		}
	})

	t.Run("a stats server without WithReloadStatus", func(t *testing.T) {
		// Serves JSON, but not this endpoint — 404 is the honest answer and must
		// not be read as available.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()
		if localStatsUp(srv.URL) {
			t.Error("want false when /reload/status 404s")
		}
	})
}
