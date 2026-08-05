package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateReadyForRouteRejectsStaleCache(t *testing.T) {
	rate := 0.1
	if rateReadyForRoute(RelayStationRate{Rate: &rate, Status: RelayRateStatusReady, UpdatedAt: time.Now().Add(-3 * relayStationRatePollInterval)}, time.Now()) {
		t.Fatal("stale relay rate was considered routeable")
	}
}

func TestRelayProxyTargetNormalizesOpenAIAliases(t *testing.T) {
	tests := []struct {
		base string
		path string
		want string
	}{
		{base: "https://relay.example", path: "/responses", want: "https://relay.example/v1/responses"},
		{base: "https://relay.example/v1", path: "/v1/chat/completions", want: "https://relay.example/v1/chat/completions"},
		{base: "https://relay.example/openai/v1", path: "/v1/embeddings", want: "https://relay.example/openai/v1/embeddings"},
		{base: "https://relay.example", path: "/backend-api/codex/responses", want: "https://relay.example/v1/responses"},
	}
	for _, test := range tests {
		got, err := relayProxyTarget(test.base, test.path, "")
		if err != nil {
			t.Fatalf("relayProxyTarget(%q, %q): %v", test.base, test.path, err)
		}
		if got != test.want {
			t.Fatalf("relayProxyTarget(%q, %q) = %q, want %q", test.base, test.path, got, test.want)
		}
	}
}

func TestValidateStationRequiresProxyTokenForEveryRelay(t *testing.T) {
	service := &RelayStationService{}
	for _, stationType := range []RelayStationType{RelayStationTypeNewAPI, RelayStationTypeSub2API} {
		station := &relayStation{
			ID:         "station",
			Type:       stationType,
			Name:       "relay",
			BaseURL:    "https://relay.example",
			ControlURL: "https://relay.example",
			Username:   "admin",
			Password:   "password",
		}
		if err := service.validateStation(station); err == nil {
			t.Fatalf("%s station without proxy_token was accepted", stationType)
		}
		station.ProxyToken = "upstream-api-key"
		if err := service.validateStation(station); err != nil {
			t.Fatalf("%s station with proxy_token was rejected: %v", stationType, err)
		}
	}
}

func TestTestStationProbesUnboundAIHub(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/ctl/group-prices" {
			t.Fatalf("unexpected test endpoint: %s", r.URL.Path)
		}
		if r.Header.Get("x-ui-password") != "ui-password" {
			t.Fatal("aihub test request did not authenticate")
		}
		_, _ = w.Write([]byte(`{"default":{"status":"ready","lowestRate":0.1},"groups":{}}`))
	}))
	defer upstream.Close()

	service := &RelayStationService{
		settingRepo: &fakeSettingRepo{},
		loaded:      true,
		config: relayStationConfig{Stations: []relayStation{{
			ID: "aihub", Type: RelayStationTypeAIHub, Name: "AIHub", BaseURL: upstream.URL,
			ControlURL: upstream.URL, UIPassword: "ui-password", ProxyToken: "proxy-token", Enabled: true,
		}}},
		rates:    relayRateCache{Rates: make(map[string]map[string]RelayStationRate)},
		routes:   make(map[string]relayRouteCacheEntry),
		sessions: make(map[string]*relayStationSession),
	}
	if _, err := service.TestStation(context.Background(), "aihub"); err != nil {
		t.Fatalf("TestStation: %v", err)
	}
	if requests != 1 {
		t.Fatalf("unbound aihub station received %d requests, want 1", requests)
	}
}
