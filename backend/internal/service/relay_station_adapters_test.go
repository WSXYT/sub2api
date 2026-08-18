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
		ID: "aihub", Type: RelayStationTypeAIHub, Name: "legacy", Username: "legacy-email-only",
	}
	if err := service.validateStation(legacyAIHub); err != nil {
		t.Fatalf("legacy aihub station was rejected: %v", err)
	}
}

func TestValidateStationDefaultsManagedAIHubRouterAndControlURL(t *testing.T) {
	service := &RelayStationService{}
	aihub := &relayStation{
		ID: "aihub", Type: RelayStationTypeAIHub, Name: "managed", Username: "user@example.com",
	}
	if err := service.validateStation(aihub); err != nil {
		t.Fatalf("managed aihub station was rejected: %v", err)
	}
	if aihub.BaseURL != managedAIHubRouterURL || aihub.ControlURL != managedAIHubRouterURL {
		t.Fatalf("managed aihub urls = %q/%q, want %q", aihub.BaseURL, aihub.ControlURL, managedAIHubRouterURL)
	}

	newAPI := &relayStation{
		ID: "newapi", Type: RelayStationTypeNewAPI, Name: "newapi", BaseURL: "https://relay.example",
		Username: "admin", Password: "password",
	}
	if err := service.validateStation(newAPI); err != nil {
		t.Fatalf("newapi station without control URL was rejected: %v", err)
	}
	if newAPI.ControlURL != newAPI.BaseURL {
		t.Fatalf("newapi control URL = %q, want %q", newAPI.ControlURL, newAPI.BaseURL)
	}
}

func TestRelayAccountUsesUpstreamModelCapabilitySnapshot(t *testing.T) {
	account := &Account{Extra: map[string]any{
		relayAccountMarkerKey:          true,
		"relay_model_capability_known": true,
		"relay_supported_models":      []any{"gpt-5", "claude-*"},
	}}
	if !account.IsModelSupported("gpt-5") || !account.IsModelSupported("claude-sonnet-4") {
		t.Fatal("relay account rejected a model from the upstream capability snapshot")
	}
	if account.IsModelSupported("gemini-2.5-pro") {
		t.Fatal("relay account accepted a model outside the upstream capability snapshot")
	}
}

func TestRelayAccountRequiresMappingAndMappedCapability(t *testing.T) {
	account := &Account{
		Credentials: map[string]any{"model_mapping": map[string]any{
			"public-gpt":    "gpt-5",
			"public-claude": "claude-sonnet-4",
		}},
		Extra: map[string]any{
			relayAccountMarkerKey:          true,
			"relay_model_capability_known": true,
			"relay_supported_models":      []any{"gpt-5"},
		},
	}
	if !account.IsModelSupported("public-gpt") {
		t.Fatal("relay account rejected a mapped upstream capability")
	}
	if account.IsModelSupported("gpt-5") {
		t.Fatal("relay account accepted a model excluded by its mapping")
	}
	if account.IsModelSupported("public-claude") {
		t.Fatal("relay account accepted a mapped model outside the upstream capability snapshot")
	}
}

func TestRelayEffectiveRateHonorsMaximum(t *testing.T) {
	first := 0.09
	second := 0.12
	maxRate := 0.1
	got, ok := relayEffectiveRate(RelayStationRate{Rate: &first, Status: RelayRateStatusReady}, RelayStationSource{Delta: 0.02, MaxRate: &maxRate})
	if !ok || got != 0.1 {
		t.Fatalf("capped effective rate = %v/%v, want 0.1/true", got, ok)
	}
	adjustRate := false
	got, ok = relayEffectiveRate(RelayStationRate{Rate: &first, Status: RelayRateStatusReady}, RelayStationSource{Delta: 0.02, AdjustRate: &adjustRate, MaxRate: &maxRate})
	if !ok || got != first {
		t.Fatalf("disabled adjustment rate = %v/%v, want %.2f/true", got, ok, first)
	}
	if _, ok := relayEffectiveRate(RelayStationRate{Rate: &second, Status: RelayRateStatusReady}, RelayStationSource{Delta: -0.5, AdjustRate: &adjustRate, MaxRate: &maxRate}); ok {
		t.Fatal("source above maximum upstream rate remained routeable with adjustment disabled")
	}
}

func TestManagedAIHubConnectionUsesSharedSecrets(t *testing.T) {
	t.Setenv(managedAIHubUIPasswordEnv, "managed-console-password")
	t.Setenv(managedAIHubProxyTokenEnv, "managed-proxy-token")
	station := relayStation{Type: RelayStationTypeAIHub, UIPassword: "stale-password", ProxyToken: "stale-token"}
	(&RelayStationService{}).applyAIHubConnectionDefaults(&station)
	if station.UIPassword != "managed-console-password" || station.ProxyToken != "managed-proxy-token" {
		t.Fatalf("managed aihub secrets = %#v", station)
	}
}

func TestAggregateAIHubRouterRateUsesHighestRouteableMultiplier(t *testing.T) {
	low, high := 0.08, 0.2
	rate := aggregateAIHubRouterRate([]relayAIHubStatusCandidate{
		{Rate: &low, Models: []string{"gpt-5"}},
		{Rate: &high},
		{Rate: &high, Excluded: true, Models: []string{"claude-*"}},
	})
	if rate.Status != RelayRateStatusReady || rate.Rate == nil || *rate.Rate != high {
		t.Fatalf("aggregated aihub rate = %#v, want highest eligible %.2f", rate, high)
	}
	if rate.SupportedModels != nil {
		t.Fatalf("mixed known and unknown model metadata must remain unrestricted: %#v", rate.SupportedModels)
	}
}

func TestListGroupsUsesStationRateSources(t *testing.T) {
	tests := []struct {
		name        string
		stationType RelayStationType
		groupsBody  string
		want        []string
	}{
		{
			name:        "newapi pricing",
			stationType: RelayStationTypeNewAPI,
			groupsBody:  `{"success":true,"group_ratio":{"vip":0.8,"default":1}}`,
			want:        []string{"default", "vip"},
		},
		{
			name:        "sub2api available groups",
			stationType: RelayStationTypeSub2API,
			groupsBody:  `{"code":0,"data":[{"name":"vip","rate_multiplier":0.8},{"name":"default","rate_multiplier":1}]}`,
			want:        []string{"default", "vip"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/login":
					_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"dashboard"}}`))
				case "/api/pricing", "/api/v1/groups/available":
					_, _ = w.Write([]byte(test.groupsBody))
				case "/api/v1/auth/login":
					_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"dashboard"}}`))
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
				}
			}))
			defer upstream.Close()

			service := &RelayStationService{
				settingRepo: &fakeSettingRepo{},
				loaded:      true,
				config: relayStationConfig{Stations: []relayStation{{
					ID: "station", Type: test.stationType, Name: "Station", BaseURL: upstream.URL,
					ControlURL: upstream.URL, Username: "admin", Password: "password",
				}}},
				sessions: make(map[string]*relayStationSession),
			}
			groups, err := service.ListGroups(context.Background(), "station")
			if err != nil {
				t.Fatalf("ListGroups: %v", err)
			}
			if len(groups) != len(test.want) {
				t.Fatalf("group count = %d, want %d", len(groups), len(test.want))
			}
			for index, want := range test.want {
				if groups[index].Name != want {
					t.Fatalf("group[%d] = %q, want %q", index, groups[index].Name, want)
				}
			}
		})
	}
}

func TestSyncAIHubConfigPostsNativePolicy(t *testing.T) {
	max := 0.8
	var received relayAIHubConfig
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ctl/config" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			return
		}
		if r.Header.Get("x-ui-password") != "ui-password" {
			t.Error("aihub config request did not authenticate")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode config: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	service := &RelayStationService{
		settingRepo: &fakeSettingRepo{},
		loaded:      true,
		config: relayStationConfig{
			Stations: []relayStation{{
				ID: "aihub", Type: RelayStationTypeAIHub, Name: "AIHub", BaseURL: upstream.URL,
				ControlURL: upstream.URL, UIPassword: "ui-password", Enabled: true,
			}},
			Bindings: []RelayGroupBinding{{
				GroupID: 1,
				Sources: []RelayStationSource{{
					StationID: "aihub", Enabled: true, SourceGroup: "local-group", Mode: "economy",
					AccountPools: []string{"plus", "team"}, MaxRate: &max,
				}},
			}},
		},
	}
	if err := service.SyncAIHubConfig(context.Background()); err != nil {
		t.Fatalf("SyncAIHubConfig: %v", err)
	}
	if received.Mode != "economy" || !sameRelayStringSlice(received.AccountPoolPlans, []string{"plus", "team"}) || received.PriceBand == nil || received.PriceBand.Min == nil || *received.PriceBand.Min != 0 || received.PriceBand.Max == nil || *received.PriceBand.Max != max {
		t.Fatalf("received policy = %#v, want economy plus+team max %.1f", received, max)
	}
}

func TestAIHubConfigForStationUsesEmptyPoolListForNoFilter(t *testing.T) {
	service := &RelayStationService{config: relayStationConfig{Bindings: []RelayGroupBinding{{
		GroupID: 1,
		Sources: []RelayStationSource{{StationID: "aihub", Enabled: true}},
	}}}}
	policy, ok := service.aiHubConfigForStation("aihub")
	if !ok || policy.AccountPoolPlans == nil || len(policy.AccountPoolPlans) != 0 || policy.PriceBand != nil {
		t.Fatalf("empty pool policy = %#v/%v, want an unfiltered, unbounded policy", policy, ok)
	}
}

func TestTestStationProbesUnboundAIHub(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/ctl/status" {
			t.Fatalf("unexpected test endpoint: %s", r.URL.Path)
		}
		if r.Header.Get("x-ui-password") != "ui-password" {
			t.Fatal("aihub test request did not authenticate")
		}
		_, _ = w.Write([]byte(`{"currentGroupId":1,"currentCode":"default","candidates":[{"groupId":1,"code":"default","rate":0.1}]}`))
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

func TestResolveRoutePrefersRelayPriority(t *testing.T) {
	fastRate, preferredRate := 0.1, 0.2
	service := &RelayStationService{
		settingRepo: &fakeSettingRepo{},
		loaded:      true,
		config: relayStationConfig{
			Stations: []relayStation{
				{ID: "low", Type: RelayStationTypeAIHub, Name: "Low", BaseURL: "http://router-low.example", ControlURL: "http://router-low.example", Enabled: true},
				{ID: "high", Type: RelayStationTypeAIHub, Name: "High", BaseURL: "http://router-high.example", ControlURL: "http://router-high.example", Enabled: true},
			},
			Bindings: []RelayGroupBinding{{GroupID: 1, Sources: []RelayStationSource{
				{StationID: "low", SourceGroup: "default", Enabled: true, Priority: 0},
				{StationID: "high", SourceGroup: "default", Enabled: true, Priority: 100},
			}}},
		},
		rates: relayRateCache{Rates: map[string]map[string]RelayStationRate{
			"low":  {"default": {Rate: &fastRate, Status: RelayRateStatusReady, UpdatedAt: time.Now()}},
			"high": {"default": {Rate: &preferredRate, Status: RelayRateStatusReady, UpdatedAt: time.Now()}},
		}},
		routes:   make(map[string]relayRouteCacheEntry),
		sessions: make(map[string]*relayStationSession),
	}

	route, err := service.ResolveRoute(context.Background(), 1, "session")
	if err != nil {
		t.Fatalf("ResolveRoute: %v", err)
	}
	if route.StationID() != "high" {
		t.Fatalf("selected station = %q, want high-priority station", route.StationID())
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
		case "/ctl/status":
			rate := 0.1
			if activeEmail == "two@example.com" {
				rate = 0.2
			}
			_, _ = w.Write([]byte(`{"currentGroupId":1,"currentCode":"default","candidates":[{"groupId":1,"code":"default","rate":` + fmt.Sprint(rate) + `}]}`))
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

func TestFetchAIHubRatesReadsCurrentAndNamedCandidates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ctl/status" {
			t.Fatalf("unexpected aihub endpoint: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"currentGroupId":2,"currentCode":"premium","candidates":[{"groupId":1,"code":"standard","rate":0.1},{"groupId":2,"code":"premium","rate":0.2},{"groupId":3,"code":"blocked","rate":0.3,"excluded":true}]}`))
	}))
	defer upstream.Close()

	rates, err := (&RelayStationService{}).fetchAIHubRates(context.Background(), relayStation{
		ID: "aihub", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL,
	}, map[string]struct{}{"default": {}, "premium": {}, "blocked": {}, "missing": {}})
	if err != nil {
		t.Fatalf("fetchAIHubRates: %v", err)
	}
	for _, sourceGroup := range []string{"default", "premium"} {
		if rate := rates[sourceGroup].Rate; rate == nil || *rate != 0.2 || rates[sourceGroup].Status != RelayRateStatusReady {
			t.Fatalf("%s rate = %#v, want ready 0.2", sourceGroup, rates[sourceGroup])
		}
	}
	for _, sourceGroup := range []string{"blocked", "missing"} {
		if rates[sourceGroup].Rate != nil || rates[sourceGroup].Status != RelayRateStatusUnavailable {
			t.Fatalf("%s rate = %#v, want unavailable", sourceGroup, rates[sourceGroup])
		}
	}
}

func TestListStationsReadsSeparateAIHubBalances(t *testing.T) {
	activeEmail := ""
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
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/ctl/config":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/ctl/account":
			if r.Header.Get("x-ui-password") != "ui-password" {
				t.Fatal("aihub account request did not authenticate")
			}
			balance := 1.25
			if activeEmail == "two@example.com" {
				balance = 2.5
			}
			_, _ = w.Write([]byte(`{"email":"` + activeEmail + `","balance":` + fmt.Sprint(balance) + `}`))
		default:
			t.Errorf("unexpected aihub endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	service := &RelayStationService{
		settingRepo: &fakeSettingRepo{},
		loaded:      true,
		config: relayStationConfig{Stations: []relayStation{
			{ID: "one", Type: RelayStationTypeAIHub, Name: "One", BaseURL: upstream.URL, ControlURL: upstream.URL, UIPassword: "ui-password", Username: "one@example.com", Password: "one"},
			{ID: "two", Type: RelayStationTypeAIHub, Name: "Two", BaseURL: upstream.URL, ControlURL: upstream.URL, UIPassword: "ui-password", Username: "two@example.com", Password: "two"},
			{ID: "legacy", Type: RelayStationTypeAIHub, Name: "Legacy", BaseURL: upstream.URL, ControlURL: upstream.URL, UIPassword: "ui-password", Username: "legacy@example.com"},
			{ID: "newapi", Type: RelayStationTypeNewAPI, Name: "NewAPI", BaseURL: upstream.URL, ControlURL: upstream.URL},
		}},
	}
	stations, err := service.ListStations(context.Background())
	if err != nil {
		t.Fatalf("ListStations: %v", err)
	}
	balances := make(map[string]*float64, len(stations))
	for _, station := range stations {
		balances[station.ID] = station.Balance
	}
	if balance := balances["one"]; balance == nil || *balance != 1.25 {
		t.Fatalf("first aihub balance = %v, want 1.25", balance)
	}
	if balance := balances["two"]; balance == nil || *balance != 2.5 {
		t.Fatalf("second aihub balance = %v, want 2.5", balance)
	}
	if balances["legacy"] != nil {
		t.Fatalf("legacy aihub balance = %v, want nil", balances["legacy"])
	}
	if balances["newapi"] != nil {
		t.Fatalf("newapi balance = %v, want nil", balances["newapi"])
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
			if got := r.URL.Query().Get("stream_options"); got != "include_usage" {
				t.Errorf("forwarded query = %q, want include_usage", got)
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
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions?stream_options=include_usage", nil)
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
