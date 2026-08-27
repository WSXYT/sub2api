package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRelaySelectedRateAcceptsOnlyFiniteNonNegativeValues(t *testing.T) {
	headers := make(http.Header)
	for value, expected := range map[string]*float64{
		"0.03": pointer(0.03),
		"":     nil,
		"-1":   nil,
		"NaN":  nil,
		"+Inf": nil,
		"bad":  nil,
	} {
		headers.Set("x-aihub-auto-rate", value)
		actual := RelaySelectedRate(headers)
		if expected == nil && actual != nil || expected != nil && (actual == nil || *actual != *expected) {
			t.Fatalf("RelaySelectedRate(%q) = %v, want %v", value, actual, expected)
		}
	}
}

func pointer(value float64) *float64 { return &value }

func TestPrepareRequestRateLimitUsesAIHubRawCeiling(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://relay/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	delta := 0.02
	adjust := true
	route := &RelayRoute{
		station: relayStation{Type: RelayStationTypeAIHub},
		source:  RelayStationSource{AdjustRate: &adjust, Delta: delta},
	}
	if err := (&RelayStationService{}).PrepareRequestRateLimit(req, route, 0.1); err != nil {
		t.Fatalf("PrepareRequestRateLimit() error = %v", err)
	}
	if got := req.Header.Get(relayMaxRateHeader); got != "0.08" {
		t.Fatalf("max-rate header = %q, want 0.08", got)
	}
}

func TestPrepareRequestRateLimitRejectsNonAIHubOverLimit(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://relay/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	rate := 0.2
	route := &RelayRoute{
		station:       relayStation{Type: RelayStationTypeNewAPI},
		effectiveRate: rate,
	}
	if err := (&RelayStationService{}).PrepareRequestRateLimit(req, route, 0.1); !errors.Is(err, ErrRelayPriceExceeded) {
		t.Fatalf("PrepareRequestRateLimit() error = %v, want ErrRelayPriceExceeded", err)
	}
	if got := req.Header.Get(relayMaxRateHeader); got != "" {
		t.Fatalf("non-AIHub request received max-rate header %q", got)
	}
}

func TestRateReadyForRouteRejectsStaleCache(t *testing.T) {
	rate := 0.1
	if rateReadyForRoute(RelayStationRate{Rate: &rate, Status: RelayRateStatusReady, UpdatedAt: time.Now().Add(-3 * relayStationRatePollInterval)}, time.Now()) {
		t.Fatal("stale relay rate was considered routeable")
	}
}

func TestRelayProxyTokenUsesPersistedKeyWithoutAdminLogin(t *testing.T) {
	service := &RelayStationService{}
	station := relayStation{
		ID:   "station",
		Type: RelayStationTypeSub2API,
		APIKeys: map[string]relayAPIKey{
			"plus": {Name: "saved-key", Key: "sk-saved"},
		},
	}
	got, err := service.relayProxyToken(context.Background(), station, RelayStationSource{
		SourceGroup: "plus",
		APIKeyName:  "saved-key",
	})
	if err != nil {
		t.Fatalf("relayProxyToken() error = %v", err)
	}
	if got != "sk-saved" {
		t.Fatalf("relayProxyToken() = %q, want persisted key", got)
	}
}

func TestRelayProxyTokenUsesCurrentConfigKeyForStaleRoute(t *testing.T) {
	service := &RelayStationService{
		loaded: true,
		config: relayStationConfig{Stations: []relayStation{{
			ID: "station",
			APIKeys: map[string]relayAPIKey{
				"plus": {Name: "saved-key", Key: "sk-current"},
			},
		}}},
	}
	staleRouteStation := relayStation{ID: "station", Type: RelayStationTypeSub2API}
	got, err := service.relayProxyToken(context.Background(), staleRouteStation, RelayStationSource{
		SourceGroup: "plus",
		APIKeyName:  "saved-key",
	})
	if err != nil {
		t.Fatalf("relayProxyToken() error = %v", err)
	}
	if got != "sk-current" {
		t.Fatalf("relayProxyToken() = %q, want current persisted key", got)
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
		{base: "https://relay.example/v1", path: "/v1beta/models/gemini-pro:generateContent", want: "https://relay.example/v1beta/models/gemini-pro:generateContent"},
		{base: "https://relay.example", path: "/v1beta/models/gemini-pro:generateContent", want: "https://relay.example/v1beta/models/gemini-pro:generateContent"},
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

func TestForwardAccountHidesUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Provider", "secret-provider")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":"secret upstream details"}`))
	}))
	defer upstream.Close()

	service := &RelayStationService{}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
		station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
		source:  RelayStationSource{SourceGroup: "default"},
	}, inbound)
	if response != nil || !errors.Is(err, ErrRelayUpstreamFailed) {
		t.Fatalf("ForwardAccount() = response %#v, error %v; want hidden upstream failure", response, err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "418") || strings.Contains(err.Error(), "private") {
		t.Fatalf("ForwardAccount() exposed upstream details: %q", err.Error())
	}
	statusCode, ok := RelayUpstreamStatus(err)
	if !ok || statusCode != http.StatusTeapot {
		t.Fatalf("ForwardAccount() lost internal retry status: %d, %v", statusCode, ok)
	}
}

func TestForwardAccountKeepsJSONResponseForStreamRequest(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	service := &RelayStationService{}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/responses", nil)
	inbound.Header.Set("X-Sub2API-Relay-Expected-Stream", "1")
	response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
		station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
		source:  RelayStationSource{SourceGroup: "default"},
	}, inbound)
	if err != nil {
		t.Fatalf("ForwardAccount() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !called || response.Header.Get("Content-Type") != "application/json" || string(payload) != `{"id":"resp_1","status":"completed","output":[]}` {
		t.Fatalf("stream request JSON response = called:%v content-type:%q payload:%q", called, response.Header.Get("Content-Type"), payload)
	}
}

func TestForwardAccountStripsUntrustedRelayRate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-aihub-auto-rate", "999")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[]}`))
	}))
	defer upstream.Close()

	service := &RelayStationService{}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
		station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
		source:  RelayStationSource{SourceGroup: "default"},
	}, inbound)
	if err != nil {
		t.Fatalf("ForwardAccount() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Header.Get("x-aihub-auto-rate") != "" {
		t.Fatalf("untrusted relay rate was retained: %#v", response.Header)
	}
}

func TestForwardAccountRequiresManagedAIHubRate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[]}`))
	}))
	defer upstream.Close()

	service := &RelayStationService{activeAIHubStations: map[string]struct{}{"aihub": {}}}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
		station: relayStation{ID: "aihub", Type: RelayStationTypeAIHub, BaseURL: upstream.URL},
		source:  RelayStationSource{SourceGroup: "default"},
	}, inbound)
	if response != nil || !errors.Is(err, ErrRelayUpstreamFailed) {
		t.Fatalf("ForwardAccount() = response %#v, error %v; want missing-rate failure", response, err)
	}
}

func TestForwardAccountKeepsRawAIHubRateForAccountCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-aihub-auto-rate", "0.2")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[]}`))
	}))
	defer upstream.Close()

	adjust := true
	account := &Account{Type: AccountTypeRelay}
	service := &RelayStationService{activeAIHubStations: map[string]struct{}{"aihub": {}}}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.ForwardAccount(context.Background(), account, &RelayRoute{
		station: relayStation{ID: "aihub", Type: RelayStationTypeAIHub, BaseURL: upstream.URL},
		source:  RelayStationSource{SourceGroup: "default", AdjustRate: &adjust, Delta: 0.005},
	}, inbound)
	if err != nil {
		t.Fatalf("ForwardAccount() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.Header.Get("x-aihub-auto-rate"); got != "0.2" {
		t.Fatalf("selected upstream rate header = %q, want raw rate 0.2", got)
	}
	if got, ok := account.RelayUpstreamRate(); !ok || got != 0.2 {
		t.Fatalf("relay upstream rate = %v/%v, want 0.2/true", got, ok)
	}
	wantEffective := 0.2
	wantEffective += 0.005
	if got, ok := account.RelayEffectiveRate(); !ok || got != wantEffective {
		t.Fatalf("relay effective rate = %v/%v, want %v/true", got, ok, wantEffective)
	}
}

func TestRelayTestFailureHidesUpstreamDetails(t *testing.T) {
	err := relayTestFailure("private", errors.New("secret-provider at https://secret.example"))
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatalf("relay test exposed upstream details: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "relay station test failed") {
		t.Fatalf("relay test did not return its generic failure: %q", err.Error())
	}
}

func TestForwardAccountRejectsOversizedUnlabeledResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, strings.Repeat("x", relayResponseSanitizeLimit+1))
	}))
	defer upstream.Close()

	service := &RelayStationService{}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
		station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
		source:  RelayStationSource{SourceGroup: "default"},
	}, inbound)
	if response != nil || !errors.Is(err, ErrRelayUpstreamFailed) {
		t.Fatalf("ForwardAccount() = response %#v, error %v; want hidden upstream failure", response, err)
	}
}

func TestRelaySanitizedSSEBodyRejectsOversizedEvent(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", relaySSEEventLimit) + "\n\n"))
	payload, err := io.ReadAll(newRelaySanitizedSSEBody(body, "/v1/chat/completions", "private"))
	if !errors.Is(err, ErrRelayUpstreamFailed) {
		t.Fatalf("read sanitized stream: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("oversized stream wrote client bytes before failover: %q", payload)
	}
}

func TestForwardAccountHidesSuccessfulStatusErrorPayloads(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		path        string
		stream      bool
		wantError   bool
		wantSafe    bool
	}{
		{
			name:        "json",
			contentType: "application/json",
			body:        `{"error":{"message":"secret-provider at https://secret.example"}}`,
			path:        "/v1/chat/completions",
			wantError:   true,
		},
		{
			name:        "mislabeled json",
			contentType: "text/plain",
			body:        `{"error":{"message":"secret-provider at https://secret.example"}}`,
			path:        "/v1/chat/completions",
			wantError:   true,
		},
		{
			name:        "plain html diagnostic",
			contentType: "text/html",
			body:        `<html>secret-provider at https://secret.example</html>`,
			path:        "/v1/chat/completions",
			wantError:   true,
		},
		{
			name:        "responses incomplete",
			contentType: "application/json",
			body:        `{"type":"response.incomplete","response":{"status":"incomplete","error":{"message":"secret-provider"}}}`,
			path:        "/v1/responses",
			wantError:   true,
		},
		{
			name:        "mislabeled sse",
			contentType: "text/plain",
			body:        "event: error\ndata: {\"error\":{\"message\":\"secret-provider\"}}\n\n",
			path:        "/v1/chat/completions",
			stream:      true,
		},
		{
			name:        "openai sse",
			contentType: "text/event-stream",
			body:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"secret-provider at https://secret.example\"}}\n\n",
			path:        "/v1/chat/completions",
		},
		{
			name:        "empty responses stream",
			contentType: "text/event-stream",
			body:        "",
			path:        "/v1/responses",
		},
		{
			name:        "responses keepalive then eof",
			contentType: "text/event-stream",
			body:        ": keepalive\n\n",
			path:        "/v1/responses",
		},
		{
			name:        "responses preamble then failed",
			contentType: "text/event-stream",
			body:        "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\",\"status\":\"in_progress\"}}\n\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"secret-provider\"}}}\n\n",
			path:        "/v1/responses",
		},
		{
			name:        "responses empty completed",
			contentType: "text/event-stream",
			body:        "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\",\"status\":\"in_progress\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[]}}\n\n",
			path:        "/v1/responses",
		},
		{
			name:        "sse raw json error",
			contentType: "text/event-stream",
			body:        `{"error":{"message":"secret-provider at https://secret.example"}}`,
			path:        "/v1/chat/completions",
		},
		{
			name:        "anthropic sse",
			contentType: "text/event-stream",
			body:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"secret-provider at https://secret.example\"}}\n\n",
			path:        "/v1/messages",
		},
		{
			name:        "late sse error",
			contentType: "text/event-stream",
			body:        "data: {\"choices\":[{\"delta\":{\"content\":\"safe\"}}]}\n\nevent: error\ndata: {\"error\":{\"message\":\"secret-provider\"}}\n\n",
			path:        "/v1/chat/completions",
			wantSafe:    true,
		},
		{
			name:        "gemini sse",
			contentType: "text/event-stream",
			body:        "data: {\"error\":{\"message\":\"secret-provider at https://secret.example\"}}\n\n",
			path:        "/v1beta/models/gemini-pro:streamGenerateContent",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = w.Write([]byte(test.body))
			}))
			defer upstream.Close()

			service := &RelayStationService{}
			inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test"+test.path, nil)
			if test.stream {
				inbound.Header.Set("Accept", "text/event-stream")
			}
			response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
				station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
				source:  RelayStationSource{SourceGroup: "default"},
			}, inbound)
			if test.wantError {
				if response != nil || !errors.Is(err, ErrRelayUpstreamFailed) {
					t.Fatalf("ForwardAccount() = response %#v, error %v; want hidden upstream failure", response, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ForwardAccount(): %v", err)
			}
			payload, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if !errors.Is(readErr, ErrRelayUpstreamFailed) {
				t.Fatalf("read sanitized response: %v", readErr)
			}
			text := string(payload)
			if strings.Contains(text, "secret-provider") || strings.Contains(text, "secret.example") {
				t.Fatalf("sanitized stream exposed upstream details: %s", text)
			}
			if test.wantSafe {
				if !strings.Contains(text, "safe") {
					t.Fatalf("sanitized stream lost prior safe output: %s", text)
				}
			} else if text != "" {
				t.Fatalf("sanitized stream wrote client bytes before failover: %s", text)
			}
		})
	}
}

func TestForwardAccountBuffersResponsesPreambleUntilOutput(t *testing.T) {
	body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
	stream := newRelaySanitizedSSEBody(io.NopCloser(strings.NewReader(body)), "/v1/responses", "private")
	payload, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatalf("read valid Responses stream: %v", err)
	}
	text := string(payload)
	for _, eventType := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(text, eventType) {
			t.Fatalf("valid Responses stream lost %s: %s", eventType, text)
		}
	}
}

func TestForwardAccountPreservesValidIncompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		stream      bool
	}{
		{
			name:        "json",
			contentType: "application/json",
			body:        `{"id":"resp_test","type":"response.incomplete","status":"incomplete","output":[],"usage":{"input_tokens":12,"output_tokens":4},"incomplete_details":{"reason":"max_output_tokens"}}`,
		},
		{
			name:        "sse",
			contentType: "text/event-stream",
			body:        "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_test\",\"status\":\"incomplete\",\"output\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":4},\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n",
			stream:      true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()

			service := &RelayStationService{}
			inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/responses", nil)
			if test.stream {
				inbound.Header.Set("Accept", "text/event-stream")
			}
			response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
				station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
				source:  RelayStationSource{SourceGroup: "default"},
			}, inbound)
			if err != nil {
				t.Fatalf("ForwardAccount(): %v", err)
			}
			payload, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || !strings.Contains(string(payload), "max_output_tokens") {
				t.Fatalf("valid incomplete response was rejected: payload %q, error %v", payload, readErr)
			}
		})
	}
}

func TestForwardAccountRejectsResidualContentEncoding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"message":"secret-provider"}}`)
	}))
	defer upstream.Close()

	service := &RelayStationService{}
	inbound, _ := http.NewRequest(http.MethodPost, "http://sub2api.test/v1/chat/completions", nil)
	response, err := service.ForwardAccount(context.Background(), &Account{}, &RelayRoute{
		station: relayStation{ID: "private", Type: RelayStationTypeSub2API, BaseURL: upstream.URL, ProxyToken: "token"},
		source:  RelayStationSource{SourceGroup: "default"},
	}, inbound)
	if response != nil || !errors.Is(err, ErrRelayUpstreamFailed) {
		t.Fatalf("encoded relay error was not hidden: response %#v, error %v", response, err)
	}
}

func TestRelayForwardQueryDropsBuyerCredentials(t *testing.T) {
	query := make(url.Values)
	query.Set("key", "buyer-secret")
	query.Set("access_token", "buyer-token")
	query.Set("authorization", "buyer-auth")
	query.Set("x-api-key", "buyer-api-key")
	query.Set("refresh_token", "buyer-refresh")
	query.Set("alt", "sse")
	query.Set("page", "2")

	clean := relayForwardQuery(query)
	if clean.Get("key") != "" || clean.Get("access_token") != "" || clean.Get("authorization") != "" || clean.Get("x-api-key") != "" || clean.Get("refresh_token") != "" {
		t.Fatalf("buyer credentials were retained: %v", clean)
	}
	if clean.Get("alt") != "sse" || clean.Get("page") != "2" {
		t.Fatalf("non-authentication query values were removed: %v", clean)
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
		"relay_supported_models":       []any{"gpt-5", "claude-*"},
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
			"relay_supported_models":       []any{"gpt-5"},
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
					_, _ = w.Write([]byte(`{"success":true,"data":{"id":6015,"access_token":"dashboard"}}`))
				case "/api/pricing", "/api/v1/groups/available":
					if test.stationType == RelayStationTypeNewAPI && r.Header.Get("New-Api-User") != "6015" {
						t.Errorf("newapi user header = %q, want 6015", r.Header.Get("New-Api-User"))
					}
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

func TestRelaySessionSerializesNewAPILogin(t *testing.T) {
	accessExpiresAt := time.Now().Add(15 * time.Minute).Unix()
	loginStarted := make(chan struct{})
	releaseLogin := make(chan struct{})
	loginCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		close(loginStarted)
		<-releaseLogin
		_, _ = fmt.Fprintf(w, `{"success":true,"data":{"access_token":"dashboard","access_expires_at":%d,"user":{"id":6015}}}`, accessExpiresAt)
	}))
	defer upstream.Close()

	service := NewRelayStationService(nil, nil, nil, nil, nil)
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, ControlURL: upstream.URL, Username: "admin", Password: "password"}
	firstResult := make(chan *relayStationSession, 1)
	firstErr := make(chan error, 1)
	go func() {
		session, err := service.relaySession(context.Background(), station)
		firstResult <- session
		firstErr <- err
	}()
	<-loginStarted

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.relaySession(canceled, station); !errors.Is(err, context.Canceled) {
		t.Fatalf("contending relaySession error = %v, want context cancellation", err)
	}
	close(releaseLogin)
	if err := <-firstErr; err != nil {
		t.Fatalf("relaySession: %v", err)
	}
	session := <-firstResult
	if session == nil || session.userID != "6015" {
		t.Fatalf("session = %#v, want nested NewAPI user id", session)
	}
	wantExpiry := time.Unix(accessExpiresAt, 0).Add(-relayStationSessionExpirySkew)
	if !session.expiresAt.Equal(wantExpiry) {
		t.Fatalf("session expiry = %v, want %v", session.expiresAt, wantExpiry)
	}
	if loginCount != 1 {
		t.Fatalf("NewAPI login count = %d, want 1", loginCount)
	}
}

func TestRelaySessionBacksOffRejectedNewAPILogin(t *testing.T) {
	var loginCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		_, _ = w.Write([]byte(`{"success":false,"message":"Too many attempts. Please try again later."}`))
	}))
	defer upstream.Close()

	service := NewRelayStationService(nil, nil, nil, nil, nil)
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, ControlURL: upstream.URL, Username: "admin", Password: "password"}
	for range 2 {
		if _, err := service.relaySession(context.Background(), station); !errors.Is(err, errNewAPIStationLoginRejected) {
			t.Fatalf("relaySession error = %v, want login rejected", err)
		}
	}
	if loginCount != 1 {
		t.Fatalf("NewAPI rejected login count = %d, want 1 during backoff", loginCount)
	}
}

func TestRelaySessionBacksOffHTTPRateLimit(t *testing.T) {
	loginCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	service := NewRelayStationService(nil, nil, nil, nil, nil)
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, ControlURL: upstream.URL, Username: "admin", Password: "password"}
	for range 2 {
		if _, err := service.relaySession(context.Background(), station); err == nil {
			t.Fatal("relaySession unexpectedly accepted a rate-limited login")
		}
	}
	if loginCount != 1 {
		t.Fatalf("NewAPI rate-limited login count = %d, want 1 during backoff", loginCount)
	}
}

func TestRelaySessionRejectsStaleLoginPublication(t *testing.T) {
	loginStarted := make(chan struct{})
	releaseLogin := make(chan struct{})
	loginCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCount++
		if loginCount == 1 {
			close(loginStarted)
			<-releaseLogin
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":6015,"access_token":"dashboard"}}`))
	}))
	defer upstream.Close()

	service := NewRelayStationService(nil, nil, nil, nil, nil)
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, ControlURL: upstream.URL, Username: "admin", Password: "old-password"}
	loginErr := make(chan error, 1)
	go func() {
		_, err := service.relaySession(context.Background(), station)
		loginErr <- err
	}()
	<-loginStarted

	service.mu.Lock()
	service.clearRoutesLocked()
	delete(service.sessions, station.ID)
	service.mu.Unlock()
	close(releaseLogin)
	if err := <-loginErr; !errors.Is(err, errRelayStationConfigChangedDuringLogin) {
		t.Fatalf("stale login error = %v, want configuration change", err)
	}
	service.mu.RLock()
	staleSession := service.sessions[station.ID]
	service.mu.RUnlock()
	if staleSession != nil {
		t.Fatal("stale login session was published after configuration change")
	}

	station.Password = "new-password"
	if _, err := service.relaySession(context.Background(), station); err != nil {
		t.Fatalf("relaySession after configuration change: %v", err)
	}
	if loginCount != 2 {
		t.Fatalf("NewAPI login count = %d, want a fresh login after configuration change", loginCount)
	}
}

func TestSyncAIHubConfigPostsNativePolicy(t *testing.T) {
	localMax := 0.4
	bandMin := 0.1
	bandMax := 0.8
	var received relayAIHubConfig
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ctl/config" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			return
		}
		if r.Header.Get("x-ui-password") != "ui-password" {
			t.Error("aihub config request did not authenticate")
		}
		if r.Header.Get(relayAIHubAccountHeader) != "aihub" {
			t.Errorf("aihub config request account header = %q", r.Header.Get(relayAIHubAccountHeader))
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
					AccountPools: []string{"plus", "team"}, MaxRate: &localMax,
					PriceBand: &RelayPriceBand{Min: &bandMin, Max: &bandMax},
				}},
			}},
		},
	}
	if err := service.SyncAIHubConfig(context.Background()); err != nil {
		t.Fatalf("SyncAIHubConfig: %v", err)
	}
	if received.Mode != "economy" || !sameRelayStringSlice(received.AccountPoolPlans, []string{"plus", "team"}) || received.PriceBand == nil || received.PriceBand.Min == nil || *received.PriceBand.Min != bandMin || received.PriceBand.Max == nil || *received.PriceBand.Max != bandMax {
		t.Fatalf("received policy = %#v, want economy plus+team band %.1f-%.1f", received, bandMin, bandMax)
	}
}

func TestValidateAIHubPriceBandRejectsInvalidRange(t *testing.T) {
	min := 0.4
	max := 0.2
	service := &RelayStationService{}
	candidate := relayStationConfig{
		Stations: []relayStation{{
			ID: "aihub", Type: RelayStationTypeAIHub, Name: "AIHub", BaseURL: managedAIHubRouterURL,
			Username: "user@example.com", Enabled: true,
		}},
		Bindings: []RelayGroupBinding{{
			GroupID: 1,
			Sources: []RelayStationSource{{
				StationID: "aihub", Enabled: true, PriceBand: &RelayPriceBand{Min: &min, Max: &max},
			}},
		}},
	}
	if err := service.validateConfig(&candidate); err == nil {
		t.Fatal("invalid AIHub price band was accepted")
	}
}

func TestValidateConfigAllowsMultipleManagedAIHubStations(t *testing.T) {
	service := &RelayStationService{}
	candidate := relayStationConfig{
		Stations: []relayStation{
			{ID: "aihub-1", Type: RelayStationTypeAIHub, Name: "AIHub 1", Username: "one@example.com"},
			{ID: "aihub-2", Type: RelayStationTypeAIHub, Name: "AIHub 2", Username: "two@example.com"},
		},
		Bindings: []RelayGroupBinding{
			{GroupID: 1, Sources: []RelayStationSource{{StationID: "aihub-1", Enabled: true}}},
			{GroupID: 2, Sources: []RelayStationSource{{StationID: "aihub-2", Enabled: true}}},
		},
	}
	if err := service.validateConfig(&candidate); err != nil {
		t.Fatalf("managed AIHub stations sharing the local router were rejected: %v", err)
	}
}

func TestValidateConfigAllowsDifferentPoliciesPerAIHubBinding(t *testing.T) {
	min := 0.01
	max := 0.09
	candidate := relayStationConfig{
		Stations: []relayStation{{ID: "aihub", Type: RelayStationTypeAIHub, Name: "AIHub", BaseURL: managedAIHubRouterURL, Username: "user@example.com"}},
		Bindings: []RelayGroupBinding{
			{GroupID: 1, Sources: []RelayStationSource{{StationID: "aihub", Enabled: true, Mode: "economy", PriceBand: &RelayPriceBand{Min: &min, Max: &max}}}},
			{GroupID: 2, Sources: []RelayStationSource{{StationID: "aihub", Enabled: true, Mode: "speed"}}},
		},
	}
	service := &RelayStationService{}
	if err := service.validateConfig(&candidate); err != nil {
		t.Fatalf("different AIHub binding policies were rejected: %v", err)
	}
	if candidate.Bindings[0].Sources[0].PolicyKey == candidate.Bindings[1].Sources[0].PolicyKey {
		t.Fatal("different AIHub policies received the same runtime policy key")
	}
}

func TestAIHubConfigForStationUsesEmptyPoolListForNoFilter(t *testing.T) {
	localMax := 0.4
	service := &RelayStationService{config: relayStationConfig{Bindings: []RelayGroupBinding{{
		GroupID: 1,
		Sources: []RelayStationSource{{StationID: "aihub", Enabled: true, MaxRate: &localMax}},
	}}}}
	policy, ok := service.aiHubConfigForStation("aihub")
	if !ok || policy.AccountPoolPlans == nil || len(policy.AccountPoolPlans) != 0 || policy.PriceBand != nil {
		t.Fatalf("empty pool policy = %#v/%v, want an unfiltered, unbounded policy independent of local max_rate", policy, ok)
	}
}

func TestNormalizeRelayAccountPoolsSupportsEveryCombination(t *testing.T) {
	for _, test := range []struct {
		input []string
		want  []string
	}{
		{input: nil, want: []string{}},
		{input: []string{"plus"}, want: []string{"plus"}},
		{input: []string{"pro", "team"}, want: []string{"pro", "team"}},
		{input: []string{"TEAM", "plus", "pro", "plus"}, want: []string{"plus", "pro", "team"}},
	} {
		got, err := normalizeRelayAccountPools(test.input)
		if err != nil || !sameRelayStringSlice(got, test.want) {
			t.Fatalf("normalizeRelayAccountPools(%v) = %v/%v, want %v", test.input, got, err, test.want)
		}
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

func TestResolveRouteKeepsUnadjustedSourceRouteable(t *testing.T) {
	fastRate, preferredRate := 0.1, 0.2
	adjustRate := false
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
				{StationID: "high", SourceGroup: "default", Enabled: true, Priority: 100, Delta: 0.5, AdjustRate: &adjustRate},
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
	if route.EffectiveRate() != preferredRate {
		t.Fatalf("unadjusted route rate = %v, want raw rate %v", route.EffectiveRate(), preferredRate)
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

	stations := []relayStation{
		{ID: "one", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "one@example.com", Password: "one"},
		{ID: "two", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "two@example.com", Password: "two"},
		{ID: "one", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "one@example.com", Password: "one"},
	}
	for index, station := range stations {
		rates, err := (&RelayStationService{}).fetchAIHubRates(context.Background(), station, map[string]struct{}{"default": {}})
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
		_, _ = w.Write([]byte(`{"currentGroupId":2,"currentCode":"premium","suggestedGroupId":2,"suggestedCode":"premium","suggestedRate":0.2,"candidates":[{"groupId":1,"code":"standard","rate":0.1},{"groupId":2,"code":"premium","rate":0.2},{"groupId":3,"code":"blocked","rate":0.3,"excluded":true}]}`))
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
		if rates[sourceGroup].SuggestedGroupID == nil || *rates[sourceGroup].SuggestedGroupID != 2 || rates[sourceGroup].SuggestedGroupCode != "premium" || rates[sourceGroup].SuggestedRate == nil || *rates[sourceGroup].SuggestedRate != 0.2 {
			t.Fatalf("%s suggestion = %#v, want group 2 rate 0.2", sourceGroup, rates[sourceGroup])
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

type relayCredentialSettingRepo struct {
	fakeSettingRepo
	mu               sync.Mutex
	setMultipleCalls int
	setMultipleErr   error
}

func (*relayCredentialSettingRepo) Set(context.Context, string, string) error { return nil }

func (r *relayCredentialSettingRepo) SetMultiple(context.Context, map[string]string) error {
	r.mu.Lock()
	r.setMultipleCalls++
	err := r.setMultipleErr
	r.mu.Unlock()
	return err
}

type blockingRelayRateAccountRepo struct {
	AccountRepository
	mu                 sync.Mutex
	account            Account
	updates            []map[string]any
	applied            []map[string]any
	firstUpdateStarted chan struct{}
	releaseFirstUpdate chan struct{}
}

func (r *blockingRelayRateAccountRepo) FindByExtraField(context.Context, string, any) ([]Account, error) {
	return []Account{r.account}, nil
}

func (r *blockingRelayRateAccountRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.mu.Lock()
	call := len(r.updates)
	copy := make(map[string]any, len(updates))
	for key, value := range updates {
		copy[key] = value
	}
	r.updates = append(r.updates, copy)
	r.mu.Unlock()
	if call == 0 {
		close(r.firstUpdateStarted)
		<-r.releaseFirstUpdate
	}
	r.mu.Lock()
	r.applied = append(r.applied, copy)
	r.mu.Unlock()
	return nil
}

func TestForwardDiscoversNewAPIGroupKey(t *testing.T) {
	forwarded := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"dashboard"}}`))
		case "/api/token/":
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(`{"success":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"id":7,"key":"masked****key","name":"NewAPI","group":"vip","status":1}]}}`))
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
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[]}`))
		default:
			t.Errorf("unexpected newapi endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	station := relayStation{
		ID: "newapi", Type: RelayStationTypeNewAPI, BaseURL: upstream.URL,
		ControlURL: upstream.URL, Username: "admin", Password: "password", APIKeyName: "NewAPI",
	}
	service := &RelayStationService{
		settingRepo: &relayCredentialSettingRepo{}, sessions: make(map[string]*relayStationSession),
		config: relayStationConfig{Stations: []relayStation{station}},
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
			if r.Method != http.MethodPost {
				t.Errorf("sub2api key method = %s, want POST", r.Method)
			}
			var payload struct {
				Name    string `json:"name"`
				GroupID int64  `json:"group_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode sub2api key request: %v", err)
			}
			if payload.GroupID != 9 || payload.Name != "Sub2API" {
				t.Errorf("sub2api key payload = %+v", payload)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"key":"sk-upstream-key"}}`))
		case "/v1/chat/completions":
			forwarded = true
			if got := r.Header.Get("Authorization"); got != "Bearer sk-upstream-key" {
				t.Errorf("sub2api authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[]}`))
		default:
			t.Errorf("unexpected sub2api endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	station := relayStation{
		ID: "sub2api", Type: RelayStationTypeSub2API, BaseURL: upstream.URL,
		ControlURL: upstream.URL, Username: "admin@example.com", Password: "password", APIKeyName: "Sub2API",
	}
	service := &RelayStationService{
		settingRepo: &relayCredentialSettingRepo{}, sessions: make(map[string]*relayStationSession),
		config: relayStationConfig{Stations: []relayStation{station}},
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

func TestRatePublicationDoesNotRegressAccountSnapshot(t *testing.T) {
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, Enabled: true}
	source := RelayStationSource{StationID: station.ID, SourceGroup: "vip", Enabled: true}
	accountKey := relaySourceAccountKey(station.ID, 1, source)
	repo := &blockingRelayRateAccountRepo{
		account:            Account{ID: 1, Extra: map[string]any{relayAccountKeyKey: accountKey}},
		firstUpdateStarted: make(chan struct{}),
		releaseFirstUpdate: make(chan struct{}),
	}
	service := &RelayStationService{accountRepo: repo}
	oldRate := 0.2
	newRate := 0.1
	oldTime := time.Now().Add(-time.Minute).UTC()
	newTime := time.Now().UTC()
	oldSnapshot := relayRateCache{Rates: map[string]map[string]RelayStationRate{
		station.ID: {"vip": {Rate: &oldRate, Status: RelayRateStatusReady, UpdatedAt: oldTime}},
	}}
	newSnapshot := relayRateCache{Rates: map[string]map[string]RelayStationRate{
		station.ID: {"vip": {Rate: &newRate, Status: RelayRateStatusReady, UpdatedAt: newTime}},
	}}
	config := relayStationConfig{
		Stations: []relayStation{station},
		Bindings: []RelayGroupBinding{{GroupID: 1, Sources: []RelayStationSource{source}}},
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- service.syncNativeRelayRates(context.Background(), config, oldSnapshot) }()
	<-repo.firstUpdateStarted
	secondDone := make(chan error, 1)
	go func() { secondDone <- service.syncNativeRelayRates(context.Background(), config, newSnapshot) }()
	select {
	case err := <-secondDone:
		t.Fatalf("newer publication bypassed serialization: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(repo.releaseFirstUpdate)
	if err := <-firstDone; err != nil {
		t.Fatalf("publish old snapshot: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("publish new snapshot: %v", err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.applied) != 2 || repo.applied[1]["relay_rate_updated_at"] != newTime.Format(time.RFC3339Nano) {
		t.Fatalf("applied relay snapshots = %#v, want newer timestamp last", repo.applied)
	}
}

func TestStationPollingIdentityUpdateInvalidatesRates(t *testing.T) {
	oldRate := 0.08
	station := relayStation{
		ID: "station", Type: RelayStationTypeNewAPI, Name: "Station", BaseURL: "https://old.example.com", ControlURL: "https://old.example.com",
		Username: "admin", Password: "old-password", Enabled: true,
	}
	repo := &relayNativeAccountRepo{accounts: []Account{{
		ID: 1, Extra: map[string]any{relayAccountMarkerKey: true, relayAccountKeyKey: relaySourceAccountKey(station.ID, 1, RelayStationSource{StationID: station.ID, SourceGroup: "vip"})},
	}}}
	settingRepo := &relayCredentialSettingRepo{}
	service := &RelayStationService{
		settingRepo: settingRepo,
		groupRepo:   &relayGroupMultiplierRepo{groups: map[int64]*Group{1: {ID: 1, Platform: PlatformOpenAI}}},
		accountRepo: repo,
		sessions:    make(map[string]*relayStationSession),
		loaded:      true,
		config: relayStationConfig{
			Stations: []relayStation{station},
			Bindings: []RelayGroupBinding{{GroupID: 1, Sources: []RelayStationSource{{StationID: station.ID, SourceGroup: "vip", Enabled: true}}}},
		},
		rates: relayRateCache{Rates: map[string]map[string]RelayStationRate{
			station.ID: {"vip": {Rate: &oldRate, Status: RelayRateStatusReady, UpdatedAt: time.Now().UTC()}},
		}},
	}
	newBaseURL := "https://new.example.com"
	if _, err := service.UpdateStation(context.Background(), station.ID, RelayStationUpdateInput{BaseURL: &newBaseURL}); err != nil {
		t.Fatalf("update station endpoint: %v", err)
	}
	if _, found := service.snapshotRates().Rates[station.ID]; found {
		t.Fatal("old station rates remained after polling identity update")
	}
	settingRepo.mu.Lock()
	setMultipleCalls := settingRepo.setMultipleCalls
	settingRepo.mu.Unlock()
	if setMultipleCalls != 1 {
		t.Fatalf("SetMultiple calls = %d, want one atomic config/rate update", setMultipleCalls)
	}
	if got := repo.extraUpdates[1]["relay_effective_rate"]; got != nil {
		t.Fatalf("relay effective rate = %v, want unavailable after station endpoint update", got)
	}
	if got := repo.extraUpdates[1]["relay_upstream_rate"]; got != nil {
		t.Fatalf("relay upstream rate = %v, want unavailable after station endpoint update", got)
	}
}

func TestStationPollingIdentityUpdateIsAtomicOnPersistenceFailure(t *testing.T) {
	oldRate := 0.08
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, Name: "Station", BaseURL: "https://old.example.com", ControlURL: "https://old.example.com", Enabled: true}
	settingRepo := &relayCredentialSettingRepo{setMultipleErr: errors.New("database unavailable")}
	service := &RelayStationService{
		settingRepo: settingRepo,
		loaded:      true,
		config:      relayStationConfig{Stations: []relayStation{station}},
		rates: relayRateCache{Rates: map[string]map[string]RelayStationRate{
			station.ID: {"vip": {Rate: &oldRate, Status: RelayRateStatusReady, UpdatedAt: time.Now().UTC()}},
		}},
	}
	newBaseURL := "https://new.example.com"
	if _, err := service.UpdateStation(context.Background(), station.ID, RelayStationUpdateInput{BaseURL: &newBaseURL}); err == nil {
		t.Fatal("polling identity update succeeded despite atomic persistence failure")
	}
	if got := service.snapshotConfig().Stations[0].BaseURL; got != station.BaseURL {
		t.Fatalf("station base URL = %q, want original %q after failed atomic update", got, station.BaseURL)
	}
	if rate := service.snapshotRates().Rates[station.ID]["vip"]; rate.Rate == nil || *rate.Rate != oldRate {
		t.Fatalf("station rate = %#v, want original rate after failed atomic update", rate)
	}
}

func TestRateRefreshRejectsSupersededConfiguration(t *testing.T) {
	pricingStarted := make(chan struct{})
	releasePricing := make(chan struct{})
	var releasePricingOnce sync.Once
	defer releasePricingOnce.Do(func() { close(releasePricing) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"dashboard"}}`))
		case "/api/pricing":
			close(pricingStarted)
			<-releasePricing
			_, _ = w.Write([]byte(`{"success":true,"group_ratio":{"vip":0.08}}`))
		default:
			t.Errorf("unexpected station endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	station := relayStation{ID: "station", Type: RelayStationTypeNewAPI, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "admin", Password: "password", Enabled: true}
	service := &RelayStationService{
		settingRepo: &relayCredentialSettingRepo{}, sessions: make(map[string]*relayStationSession), loaded: true,
		config: relayStationConfig{
			Stations: []relayStation{station},
			Bindings: []RelayGroupBinding{{GroupID: 1, Sources: []RelayStationSource{{StationID: station.ID, SourceGroup: "vip", Enabled: true}}}},
		},
	}
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- service.refreshRates(context.Background(), "") }()
	<-pricingStarted
	service.mu.Lock()
	service.revision++
	service.mu.Unlock()
	releasePricingOnce.Do(func() { close(releasePricing) })
	if err := <-refreshDone; err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("refresh error = %v, want superseded configuration rejection", err)
	}
	if _, found := service.snapshotRates().Rates[station.ID]; found {
		t.Fatal("superseded station rates were published")
	}
}

func TestAIHubActivationPublishesPolicyBeforeBecomingActive(t *testing.T) {
	configStarted := make(chan struct{})
	releaseConfig := make(chan struct{})
	var releaseConfigOnce sync.Once
	defer releaseConfigOnce.Do(func() { close(releaseConfig) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ctl/login":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/ctl/config":
			close(configStarted)
			<-releaseConfig
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected AIHub endpoint: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	station := relayStation{ID: "station", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL, Username: "user", Password: "password", Enabled: true}
	service := NewRelayStationService(nil, nil, nil, nil, nil)
	policy := relayAIHubConfig{Mode: "economy"}
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.activateAIHubStation(context.Background(), station, "runtime", &policy, false)
		firstDone <- err
	}()
	<-configStarted
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := service.activateAIHubStation(waitCtx, station, "runtime", &policy, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("activation while initial policy is pending = %v, want deadline exceeded", err)
	}
	releaseConfigOnce.Do(func() { close(releaseConfig) })
	if err := <-firstDone; err != nil {
		t.Fatalf("activate runtime with initial policy: %v", err)
	}
}

func TestAIHubActivationLocksAreRuntimeScopedAndCancellable(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeID := r.Header.Get(relayAIHubAccountHeader)
		started <- runtimeID
		if runtimeID == "blocked" {
			<-release
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	station := relayStation{
		ID: "station", Type: RelayStationTypeAIHub, BaseURL: upstream.URL, ControlURL: upstream.URL,
		Username: "user", Password: "password", Enabled: true,
	}
	service := NewRelayStationService(nil, nil, nil, nil, nil)
	blockedDone := make(chan error, 1)
	go func() {
		_, err := service.activateAIHubStation(context.Background(), station, "blocked", nil, false)
		blockedDone <- err
	}()
	if runtimeID := <-started; runtimeID != "blocked" {
		t.Fatalf("first runtime = %q, want blocked", runtimeID)
	}

	otherDone := make(chan error, 1)
	go func() {
		_, err := service.activateAIHubStation(context.Background(), station, "other", nil, false)
		otherDone <- err
	}()
	select {
	case runtimeID := <-started:
		if runtimeID != "other" {
			t.Fatalf("concurrent runtime = %q, want other", runtimeID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("distinct AIHub runtime was blocked by another runtime login")
	}
	if err := <-otherDone; err != nil {
		t.Fatalf("activate other runtime: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := service.activateAIHubStation(waitCtx, station, "blocked", nil, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-runtime canceled waiter error = %v, want deadline exceeded", err)
	}
	close(release)
	released = true
	if err := <-blockedDone; err != nil {
		t.Fatalf("activate blocked runtime: %v", err)
	}
}

func TestBackgroundRateRefreshIsolatesStationTimeouts(t *testing.T) {
	started := make(chan string, 2)
	releaseSlow := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- "slow"
		select {
		case <-r.Context().Done():
		case <-releaseSlow:
		}
	}))
	defer slow.Close()
	defer close(releaseSlow)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			started <- "fast"
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"dashboard"}}`))
		case "/api/pricing":
			_, _ = w.Write([]byte(`{"success":true,"group_ratio":{"vip":0.08}}`))
		default:
			t.Errorf("unexpected fast station endpoint: %s", r.URL.Path)
		}
	}))
	defer fast.Close()

	previousRate := 0.07
	previousUpdatedAt := time.Now().UTC()
	service := &RelayStationService{
		settingRepo: &relayCredentialSettingRepo{},
		sessions:    make(map[string]*relayStationSession),
		loaded:      true,
		rates: relayRateCache{Rates: map[string]map[string]RelayStationRate{
			"slow": {"vip": {Rate: &previousRate, Status: RelayRateStatusReady, UpdatedAt: previousUpdatedAt}},
		}},
		config: relayStationConfig{
			Stations: []relayStation{
				{ID: "slow", Type: RelayStationTypeNewAPI, Name: "Slow", BaseURL: slow.URL, ControlURL: slow.URL, Username: "admin", Password: "password", Enabled: true},
				{ID: "fast", Type: RelayStationTypeNewAPI, Name: "Fast", BaseURL: fast.URL, ControlURL: fast.URL, Username: "admin", Password: "password", Enabled: true},
			},
			Bindings: []RelayGroupBinding{{
				GroupID: 1,
				Sources: []RelayStationSource{
					{StationID: "slow", SourceGroup: "vip", Enabled: true},
					{StationID: "fast", SourceGroup: "vip", Enabled: true},
				},
			}},
		},
	}

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- service.refreshRatesWithStationTimeout(context.Background(), "", time.Second)
	}()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case station := <-started:
			seen[station] = true
		case <-time.After(500 * time.Millisecond):
			t.Fatal("station polls did not start concurrently")
		}
	}
	if err := <-refreshDone; err == nil {
		t.Fatal("slow station timeout should be reported")
	}

	rate := service.snapshotRates().Rates["fast"]["vip"]
	if rate.Status != RelayRateStatusReady || rate.Rate == nil || *rate.Rate != 0.08 {
		t.Fatalf("fast station rate = %#v, want ready 0.08 after slow station timeout", rate)
	}
	slowRate := service.snapshotRates().Rates["slow"]["vip"]
	if slowRate.Status != RelayRateStatusReady || slowRate.Rate == nil || *slowRate.Rate != previousRate || !slowRate.UpdatedAt.Equal(previousUpdatedAt) {
		t.Fatalf("slow station rate = %#v, want the still-fresh previous rate after timeout", slowRate)
	}
}
