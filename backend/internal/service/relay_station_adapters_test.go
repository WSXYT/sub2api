package service

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestValidateRelayURLRejectsEmbeddedCredentials(t *testing.T) {
	service := &RelayStationService{}
	for _, raw := range []string{
		"https://user:secret@relay.example",
		"https://relay.example?token=secret",
		"https://relay.example#secret",
	} {
		if err := service.validateRelayURL(raw); err == nil {
			t.Fatalf("credential-bearing relay URL %q was accepted", raw)
		}
	}
	if err := service.validateRelayURL("https://relay.example/v1"); err != nil {
		t.Fatalf("normal relay URL was rejected: %v", err)
	}
}

func TestValidateStationAllowsPlatformKeyDiscovery(t *testing.T) {
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
		if err := service.validateStation(station); err != nil {
			t.Fatalf("%s station without proxy_token was rejected: %v", stationType, err)
		}
	}
	legacyAIHub := &relayStation{
		ID: "aihub", Type: RelayStationTypeAIHub, Name: "legacy",
		BaseURL: "https://relay.example", ControlURL: "https://relay.example", Username: "legacy-email-only",
	}
	if err := service.validateStation(legacyAIHub); err != nil {
		t.Fatalf("legacy aihub station was rejected: %v", err)
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

func TestFetchAIHubRatesSwitchesAccounts(t *testing.T) {
	activeEmail := ""
	loginCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ctl/login":
			var input struct {
				Email string `json:"email"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode aihub login: %v", err)
			}
			activeEmail = input.Email
			loginCount++
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/ctl/config":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/ctl/group-prices":
			rate := 0.1
			if activeEmail == "two@example.com" {
				rate = 0.2
			}
			_, _ = w.Write([]byte(`{"default":{"status":"ready","lowestRate":` + fmt.Sprint(rate) + `},"groups":{}}`))
		default:
			t.Errorf("unexpected aihub endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	service := &RelayStationService{}
	stations := []relayStation{
		{ID: "one", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "one@example.com", Password: "one"},
		{ID: "two", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "two@example.com", Password: "two"},
		{ID: "one", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "one@example.com", Password: "one"},
	}
	for index, station := range stations {
		rates, err := service.fetchAIHubRates(context.Background(), station, map[string]struct{}{"default": {}})
		if err != nil {
			t.Fatalf("fetch account %s rates: %v", station.ID, err)
		}
		want := 0.1
		if index == 1 {
			want = 0.2
		}
		if rate := rates["default"].Rate; rate == nil || *rate != want {
			t.Fatalf("account %s rate = %v, want %v", station.ID, rate, want)
		}
	}
	if loginCount != 3 {
		t.Fatalf("aihub login count = %d, want 3", loginCount)
	}
}

func TestForwardDiscoversNewAPIGroupKey(t *testing.T) {
	forwarded := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"dashboard"}}`))
		case "/api/token/":
			_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"id":7,"key":"masked****key","group":"vip","status":1}]}}`))
		case "/api/token/7/key":
			_, _ = w.Write([]byte(`{"success":true,"data":{"key":"upstream-key"}}`))
		case "/v1/chat/completions":
			forwarded = true
			if got := r.Header.Get("Authorization"); got != "Bearer sk-upstream-key" {
				t.Errorf("newapi authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected newapi endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	service := &RelayStationService{sessions: make(map[string]*relayStationSession)}
	station := relayStation{
		ID: "newapi", Type: RelayStationTypeNewAPI, BaseURL: upstream.URL,
		ControlURL: upstream.URL, Username: "admin", Password: "password",
	}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.Forward(context.Background(), &RelayRoute{
		station: station,
		source:  RelayStationSource{SourceGroup: "vip"},
	}, inbound)
	if err != nil {
		t.Fatalf("forward through newapi: %v", err)
	}
	_ = response.Body.Close()
	if !forwarded {
		t.Fatal("newapi request was not forwarded")
	}
}

func TestForwardDiscoversSub2APIGroupKey(t *testing.T) {
	forwarded := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"dashboard"}}`))
		case "/api/v1/groups/available":
			_, _ = w.Write([]byte(`{"code":0,"data":[{"id":9,"name":"vip"}]}`))
		case "/api/v1/keys":
			if r.URL.Query().Get("group_id") != "9" {
				t.Errorf("sub2api group filter = %q", r.URL.Query().Get("group_id"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"key":"sk-upstream-key","status":"active"}]}}`))
		case "/v1/chat/completions":
			forwarded = true
			if got := r.Header.Get("Authorization"); got != "Bearer sk-upstream-key" {
				t.Errorf("sub2api authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected sub2api endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	service := &RelayStationService{sessions: make(map[string]*relayStationSession)}
	station := relayStation{
		ID: "sub2api", Type: RelayStationTypeSub2API, BaseURL: upstream.URL,
		ControlURL: upstream.URL, Username: "admin@example.com", Password: "password",
	}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.Forward(context.Background(), &RelayRoute{
		station: station,
		source:  RelayStationSource{SourceGroup: "vip"},
	}, inbound)
	if err != nil {
		t.Fatalf("forward through sub2api: %v", err)
	}
	_ = response.Body.Close()
	if !forwarded {
		t.Fatal("sub2api request was not forwarded")
	}
}
