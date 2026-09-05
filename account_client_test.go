package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountFriendsSendsInternalKey(t *testing.T) {
	t.Setenv("NEXTENDO_INTERNAL_KEY", "test-internal-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Internal-Key"); got != "test-internal-key" {
			t.Fatalf("X-Internal-Key = %q", got)
		}
		if got := r.URL.Query().Get("pid"); got != "1800000001" {
			t.Fatalf("pid = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nplnAccountData{
			PID:      1800000001,
			UserID:   "u-1800000001",
			Verified: true,
		})
	}))
	defer server.Close()

	oldBaseURL, oldClient := accountBaseURL, accountHTTP
	accountBaseURL, accountHTTP = server.URL, server.Client()
	defer func() {
		accountBaseURL, accountHTTP = oldBaseURL, oldClient
	}()

	got, err := accountFriends(1800000001)
	if err != nil {
		t.Fatalf("accountFriends: %v", err)
	}
	if !got.Verified || got.UserID != "u-1800000001" {
		t.Fatalf("unexpected account response: %+v", got)
	}
}
