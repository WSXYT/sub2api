package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	relayStationControlTimeout      = 15 * time.Second
	relayStationSessionFallbackTTL  = time.Hour
	relayStationLoginRetryDelay     = 20 * time.Minute
	relayStationTransientRetryDelay = time.Minute
	relayStationSessionExpirySkew   = time.Minute
	relayResponseSanitizeLimit      = 2 << 20
	relaySSEEventLimit              = 1 << 20
)

var (
	errNewAPIStationLoginRejected           = errors.New("newapi login was rejected")
	errRelayStationConfigChangedDuringLogin = errors.New("relay station configuration changed during login")
)

type relayStationSession struct {
	client      *http.Client
	token       string
	userID      string
	expiresAt   time.Time
	loginErr    error
	keysMu      sync.Mutex
	proxyTokens map[string]string
}

type relayHTTPStatusError struct {
	status int
}

func relayTestFailure(stationID string, err error) error {
	logger.L().Warn("relay station test failed", zap.String("station_id", stationID), zap.String("error_type", fmt.Sprintf("%T", err)))
	return infraerrors.New(http.StatusBadGateway, "RELAY_TEST_FAILED", "relay station test failed")
}

func (e *relayHTTPStatusError) Error() string {
	return fmt.Sprintf("relay upstream returned status %d", e.status)
}

func (s *RelayStationService) validateRelayURL(raw string) error {
	relayURL, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil {
		return parseErr
	}
	if relayURL.User != nil || relayURL.RawQuery != "" || relayURL.Fragment != "" {
		return errors.New("relay station url must not contain userinfo, query, or fragment")
	}

	allowInsecure := true
	if s != nil && s.cfg != nil {
		allowInsecure = s.cfg.Security.URLAllowlist.AllowInsecureHTTP
	}
	if s == nil || s.cfg == nil || !s.cfg.Security.URLAllowlist.Enabled {
		_, err := urlvalidator.ValidateURLFormat(raw, allowInsecure)
		return err
	}

	allowlist := s.cfg.Security.URLAllowlist
	normalized, err := urlvalidator.ValidateHTTPURL(raw, allowlist.AllowInsecureHTTP, urlvalidator.ValidationOptions{
		AllowedHosts:     allowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     allowlist.AllowPrivateHosts,
	})
	if err != nil || allowlist.AllowPrivateHosts {
		return err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return err
	}
	return urlvalidator.ValidateResolvedIP(parsed.Hostname())
}

func (s *RelayStationService) validateRelayTargetURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	parsed.RawQuery = ""
	return s.validateRelayURL(parsed.String())
}

func relayEndpoint(baseURL, suffix string) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", errors.New("relay control url is required")
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return baseURL + suffix, nil
}

func newRelayControlClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Jar:     jar,
		Timeout: relayStationControlTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func newRelayProxyClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (s *RelayStationService) fetchStationRates(ctx context.Context, station relayStation, required map[string]struct{}) (map[string]RelayStationRate, error) {
	switch station.Type {
	case RelayStationTypeAIHub:
		return s.fetchAIHubRates(ctx, station, required)
	case RelayStationTypeNewAPI:
		return s.fetchNewAPIRates(ctx, station, required)
	case RelayStationTypeSub2API:
		return s.fetchSub2APIRates(ctx, station, required)
	default:
		return nil, errors.New("unsupported relay station type")
	}
}

func (s *RelayStationService) fetchAIHubRates(ctx context.Context, station relayStation, required map[string]struct{}) (map[string]RelayStationRate, error) {
	s.applyAIHubConnectionDefaults(&station)
	result := make(map[string]RelayStationRate, len(required))
	for policyKey := range required {
		rate, err := s.fetchAIHubRate(ctx, station, policyKey)
		if err != nil {
			return nil, err
		}
		result[policyKey] = rate
	}
	return result, nil
}

func (s *RelayStationService) fetchAIHubRate(ctx context.Context, station relayStation, policyKey string) (RelayStationRate, error) {
	runtimeID := relayAIHubRuntimeID(station.ID, policyKey)
	policy, policyConfigured := s.aiHubConfigForKey(station.ID, policyKey)
	var initialPolicy *relayAIHubConfig
	if policyConfigured {
		initialPolicy = &policy
	}
	if _, err := s.activateAIHubStation(ctx, station, runtimeID, initialPolicy, false); err != nil {
		return RelayStationRate{}, err
	}

	endpoint, err := relayEndpoint(station.ControlURL, "/ctl/status")
	if err != nil {
		return RelayStationRate{}, err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return RelayStationRate{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RelayStationRate{}, err
	}
	req.Header.Set("x-ui-password", station.UIPassword)
	req.Header.Set(relayAIHubAccountHeader, runtimeID)
	client, err := newRelayControlClient()
	if err != nil {
		return RelayStationRate{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return RelayStationRate{}, fmt.Errorf("request aihub status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return RelayStationRate{}, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		SuggestedGroupID *int64                      `json:"suggestedGroupId"`
		SuggestedCode    string                      `json:"suggestedCode"`
		SuggestedRate    *float64                    `json:"suggestedRate"`
		Candidates       []relayAIHubStatusCandidate `json:"candidates"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return RelayStationRate{}, err
	}
	rate := RelayStationRate{Status: RelayRateStatusUnavailable}
	if policyConfigured || policyKey == "" || policyKey == "default" {
		rate = aggregateAIHubRouterRate(payload.Candidates)
	} else if candidate, found := func() (relayAIHubStatusCandidate, bool) {
		for _, item := range payload.Candidates {
			if item.Code == policyKey {
				return item, true
			}
		}
		return relayAIHubStatusCandidate{}, false
	}(); found && candidate.Rate != nil && !candidate.Excluded {
		rate = RelayStationRate{Rate: cloneFloat64(candidate.Rate), Status: RelayRateStatusReady, SupportedModels: append([]string(nil), candidate.Models...)}
	}
	rate.SuggestedGroupID = cloneInt64Pointer(payload.SuggestedGroupID)
	rate.SuggestedGroupCode = payload.SuggestedCode
	rate.SuggestedRate = cloneFloat64(payload.SuggestedRate)
	return rate, nil
}

func (s *RelayStationService) fetchStationBalance(ctx context.Context, station relayStation) (*float64, error) {
	switch station.Type {
	case RelayStationTypeAIHub:
		return s.fetchAIHubBalance(ctx, station)
	case RelayStationTypeNewAPI:
		return s.fetchNewAPIBalance(ctx, station)
	case RelayStationTypeSub2API:
		return s.fetchSub2APIBalance(ctx, station)
	default:
		return nil, errors.New("relay station type does not expose a balance")
	}
}

func (s *RelayStationService) fetchAIHubBalance(ctx context.Context, station relayStation) (*float64, error) {
	s.applyAIHubConnectionDefaults(&station)
	policyKey, policy, policyConfigured := s.aiHubPolicyForStation(station.ID)
	runtimeID := relayAIHubRuntimeID(station.ID, policyKey)
	var initialPolicy *relayAIHubConfig
	if policyConfigured {
		initialPolicy = &policy
	}
	if _, err := s.activateAIHubStation(ctx, station, runtimeID, initialPolicy, false); err != nil {
		return nil, err
	}

	endpoint, err := relayEndpoint(station.ControlURL, "/ctl/account")
	if err != nil {
		return nil, err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-ui-password", station.UIPassword)
	req.Header.Set(relayAIHubAccountHeader, runtimeID)
	client, err := newRelayControlClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request aihub account: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Balance *float64 `json:"balance"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	return cloneFloat64(payload.Balance), nil
}

func (s *RelayStationService) activateAIHubStation(ctx context.Context, station relayStation, runtimeID string, policy *relayAIHubConfig, forceConfig bool) (bool, error) {
	s.aihubMu.Lock()
	if s.aihubLocks == nil {
		s.aihubLocks = make(map[string]*contextMutex)
	}
	lock := s.aihubLocks[runtimeID]
	if lock == nil {
		lock = newContextMutex()
		s.aihubLocks[runtimeID] = lock
	}
	s.aihubMu.Unlock()

	if err := lock.Lock(ctx); err != nil {
		return false, err
	}
	defer lock.Unlock()

	s.aihubMu.Lock()
	if s.activeAIHubStations == nil {
		s.activeAIHubStations = make(map[string]struct{})
	}
	if s.aihubRuntimeEpoch == nil {
		s.aihubRuntimeEpoch = make(map[string]uint64)
	}
	_, active := s.activeAIHubStations[runtimeID]
	if active && !forceConfig {
		s.aihubMu.Unlock()
		return false, nil
	}
	globalEpoch := s.aihubEpoch
	runtimeEpoch := s.aihubRuntimeEpoch[runtimeID]
	s.aihubMu.Unlock()

	if !active && station.Username != "" && station.Password != "" {
		if err := s.loginAIHubStation(ctx, station, runtimeID); err != nil {
			return false, err
		}
	}
	if policy != nil {
		if err := s.postAIHubConfigRequest(ctx, station, *policy, runtimeID); err != nil {
			s.aihubMu.Lock()
			delete(s.activeAIHubStations, runtimeID)
			s.aihubMu.Unlock()
			return false, err
		}
	}
	s.aihubMu.Lock()
	if s.aihubEpoch != globalEpoch || s.aihubRuntimeEpoch[runtimeID] != runtimeEpoch {
		s.aihubMu.Unlock()
		return false, errors.New("aihub activation was invalidated")
	}
	s.activeAIHubStations[runtimeID] = struct{}{}
	s.aihubMu.Unlock()
	return !active, nil
}

func (s *RelayStationService) clearActiveAIHubStation(stationID string) {
	s.aihubMu.Lock()
	defer s.aihubMu.Unlock()
	if s.aihubRuntimeEpoch == nil {
		s.aihubRuntimeEpoch = make(map[string]uint64)
	}
	if stationID == "" {
		s.aihubEpoch++
		s.activeAIHubStations = make(map[string]struct{})
		return
	}
	for runtimeID := range s.aihubLocks {
		if runtimeID == stationID || strings.HasPrefix(runtimeID, stationID+":") {
			s.aihubRuntimeEpoch[runtimeID]++
			delete(s.activeAIHubStations, runtimeID)
		}
	}
}

func (s *RelayStationService) loginAIHubStation(ctx context.Context, station relayStation, runtimeID string) error {
	endpoint, err := relayEndpoint(station.ControlURL, "/ctl/login")
	if err != nil {
		return err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"email": station.Username, "password": station.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ui-password", station.UIPassword)
	req.Header.Set(relayAIHubAccountHeader, runtimeID)
	client, err := newRelayControlClient()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("login aihub station: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &relayHTTPStatusError{status: resp.StatusCode}
	}
	return nil
}

func (s *RelayStationService) aiHubPolicyForStation(stationID string) (string, relayAIHubConfig, bool) {
	for _, binding := range s.snapshotConfig().Bindings {
		for _, source := range binding.Sources {
			if source.StationID == stationID && source.Enabled {
				return source.PolicyKey, relayAIHubPolicyForSource(source), true
			}
		}
	}
	return "", relayAIHubConfig{}, false
}

func (s *RelayStationService) aiHubConfigForStation(stationID string) (relayAIHubConfig, bool) {
	_, policy, ok := s.aiHubPolicyForStation(stationID)
	return policy, ok
}

func (s *RelayStationService) aiHubConfigForKey(stationID, policyKey string) (relayAIHubConfig, bool) {
	for _, binding := range s.snapshotConfig().Bindings {
		for _, source := range binding.Sources {
			if source.StationID == stationID && source.Enabled && (source.PolicyKey == policyKey || (source.PolicyKey == "" && policyKey == source.SourceGroup)) {
				return relayAIHubPolicyForSource(source), true
			}
		}
	}
	return relayAIHubConfig{}, false
}

func aggregateAIHubRouterRate(candidates []relayAIHubStatusCandidate) RelayStationRate {
	var highest *float64
	modelsKnown := true
	models := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate.Excluded || candidate.Rate == nil {
			continue
		}
		if highest == nil || *candidate.Rate > *highest {
			highest = cloneFloat64(candidate.Rate)
		}
		if candidate.Models == nil {
			modelsKnown = false
			continue
		}
		for _, model := range candidate.Models {
			if model = strings.TrimSpace(model); model != "" {
				models[model] = struct{}{}
			}
		}
	}
	if highest == nil {
		return RelayStationRate{Status: RelayRateStatusUnavailable}
	}
	result := RelayStationRate{Rate: highest, Status: RelayRateStatusReady}
	if modelsKnown {
		result.SupportedModels = make([]string, 0, len(models))
		for model := range models {
			result.SupportedModels = append(result.SupportedModels, model)
		}
		sort.Strings(result.SupportedModels)
	}
	return result
}

type relayAIHubStatusCandidate struct {
	GroupID  int64    `json:"groupId"`
	Code     string   `json:"code"`
	Rate     *float64 `json:"rate"`
	Models   []string `json:"models"`
	Excluded bool     `json:"excluded"`
}

func (s *RelayStationService) fetchNewAPIRates(ctx context.Context, station relayStation, required map[string]struct{}) (map[string]RelayStationRate, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		session, err := s.relaySession(ctx, station)
		if err != nil {
			return nil, err
		}
		rates, status, err := s.requestNewAPIRates(ctx, station, session)
		if err == nil {
			return selectRelayRates(rates, required), nil
		}
		lastErr = err
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			break
		}
		s.clearRelaySession(station.ID)
	}
	return nil, lastErr
}

func (s *RelayStationService) requestNewAPIRates(ctx context.Context, station relayStation, session *relayStationSession) (map[string]float64, int, error) {
	endpoint, err := relayEndpoint(station.ControlURL, "/api/pricing")
	if err != nil {
		return nil, 0, err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	setNewAPIAuth(req, session)
	resp, err := session.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request newapi pricing: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Success    *bool              `json:"success"`
		GroupRatio map[string]float64 `json:"group_ratio"`
		Data       json.RawMessage    `json:"data"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, resp.StatusCode, err
	}
	if payload.Success != nil && !*payload.Success {
		return nil, resp.StatusCode, errors.New("newapi pricing request was rejected")
	}
	if len(payload.GroupRatio) == 0 && len(payload.Data) > 0 {
		var nested struct {
			GroupRatio map[string]float64 `json:"group_ratio"`
		}
		_ = json.Unmarshal(payload.Data, &nested)
		payload.GroupRatio = nested.GroupRatio
	}
	if len(payload.GroupRatio) == 0 {
		return nil, resp.StatusCode, errors.New("newapi pricing response has no group ratios")
	}
	return payload.GroupRatio, resp.StatusCode, nil
}

func (s *RelayStationService) fetchSub2APIRates(ctx context.Context, station relayStation, required map[string]struct{}) (map[string]RelayStationRate, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		session, err := s.relaySession(ctx, station)
		if err != nil {
			return nil, err
		}
		rates, status, err := s.requestSub2APIRates(ctx, station, session.token)
		if err == nil {
			return selectRelayRates(rates, required), nil
		}
		lastErr = err
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			break
		}
		s.clearRelaySession(station.ID)
	}
	return nil, lastErr
}

func (s *RelayStationService) fetchNewAPIBalance(ctx context.Context, station relayStation) (*float64, error) {
	session, err := s.relaySession(ctx, station)
	if err != nil {
		return nil, err
	}
	endpoint, err := relayEndpoint(station.ControlURL, "/api/user/self")
	if err != nil {
		return nil, err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	setNewAPIAuth(req, session)
	resp, err := session.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request newapi balance: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Data struct {
			Balance *float64 `json:"balance"`
			Quota   *float64 `json:"quota"`
		} `json:"data"`
		Balance *float64 `json:"balance"`
		Quota   *float64 `json:"quota"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Data.Balance != nil {
		return cloneFloat64(payload.Data.Balance), nil
	}
	if payload.Balance != nil {
		return cloneFloat64(payload.Balance), nil
	}
	if payload.Data.Quota != nil {
		value := *payload.Data.Quota / 500000
		return &value, nil
	}
	if payload.Quota != nil {
		value := *payload.Quota / 500000
		return &value, nil
	}
	return nil, errors.New("newapi balance was not present")
}

func (s *RelayStationService) fetchSub2APIBalance(ctx context.Context, station relayStation) (*float64, error) {
	session, err := s.relaySession(ctx, station)
	if err != nil {
		return nil, err
	}
	endpoint, err := relayEndpoint(station.ControlURL, "/api/v1/auth/me")
	if err != nil {
		return nil, err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+session.token)
	resp, err := newRelayProxyClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("request sub2api balance: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Data struct {
			Balance *float64 `json:"balance"`
			Quota   *float64 `json:"quota"`
		} `json:"data"`
		Balance *float64 `json:"balance"`
		Quota   *float64 `json:"quota"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Data.Balance != nil {
		return cloneFloat64(payload.Data.Balance), nil
	}
	if payload.Balance != nil {
		return cloneFloat64(payload.Balance), nil
	}
	if payload.Data.Quota != nil {
		return cloneFloat64(payload.Data.Quota), nil
	}
	if payload.Quota != nil {
		return cloneFloat64(payload.Quota), nil
	}
	return nil, errors.New("sub2api balance was not present")
}

func (s *RelayStationService) requestSub2APIRates(ctx context.Context, station relayStation, token string) (map[string]float64, int, error) {
	endpoint, err := relayEndpoint(station.ControlURL, "/api/v1/groups/available")
	if err != nil {
		return nil, 0, err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := newRelayProxyClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request sub2api groups: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.StatusCode, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Code int `json:"code"`
		Data []struct {
			Name           string  `json:"name"`
			RateMultiplier float64 `json:"rate_multiplier"`
		} `json:"data"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, resp.StatusCode, err
	}
	if payload.Code != 0 {
		return nil, resp.StatusCode, errors.New("sub2api groups request was rejected")
	}
	rates := make(map[string]float64, len(payload.Data))
	for _, group := range payload.Data {
		if name := strings.TrimSpace(group.Name); name != "" {
			rates[name] = group.RateMultiplier
		}
	}
	if len(rates) == 0 {
		return nil, resp.StatusCode, errors.New("sub2api groups response has no rates")
	}
	return rates, resp.StatusCode, nil
}

func selectRelayRates(all map[string]float64, required map[string]struct{}) map[string]RelayStationRate {
	result := make(map[string]RelayStationRate, len(required))
	for sourceGroup := range required {
		rate, ok := all[sourceGroup]
		if !ok {
			result[sourceGroup] = RelayStationRate{Status: RelayRateStatusUnavailable}
			continue
		}
		result[sourceGroup] = RelayStationRate{Rate: &rate, Status: RelayRateStatusReady}
	}
	return result
}

func (s *RelayStationService) relaySession(ctx context.Context, station relayStation) (*relayStationSession, error) {
	s.mu.RLock()
	existing := s.sessions[station.ID]
	s.mu.RUnlock()
	if existing != nil && existing.expiresAt.After(time.Now()) {
		if existing.loginErr != nil {
			return nil, existing.loginErr
		}
		return existing, nil
	}

	actual, _ := s.sessionLocks.LoadOrStore(station.ID, newContextMutex())
	lock, ok := actual.(*contextMutex)
	if !ok {
		return nil, errors.New("relay station session lock is invalid")
	}
	if err := lock.Lock(ctx); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	s.mu.RLock()
	existing = s.sessions[station.ID]
	loginRevision := s.revision
	s.mu.RUnlock()
	if existing != nil && existing.expiresAt.After(time.Now()) {
		if existing.loginErr != nil {
			return nil, existing.loginErr
		}
		return existing, nil
	}

	session, err := s.loginRelayStation(ctx, station)
	retryDelay := relayStationTransientRetryDelay
	var statusErr *relayHTTPStatusError
	if errors.Is(err, errNewAPIStationLoginRejected) || (errors.As(err, &statusErr) && statusErr.status == http.StatusTooManyRequests) {
		retryDelay = relayStationLoginRetryDelay
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != loginRevision {
		return nil, errRelayStationConfigChangedDuringLogin
	}
	if err != nil {
		s.sessions[station.ID] = &relayStationSession{expiresAt: time.Now().Add(retryDelay), loginErr: err}
		return nil, err
	}
	s.sessions[station.ID] = session
	return session, nil
}

func (s *RelayStationService) clearRelaySession(stationID string) {
	s.mu.Lock()
	delete(s.sessions, stationID)
	s.mu.Unlock()
}

func (s *RelayStationService) loginRelayStation(ctx context.Context, station relayStation) (*relayStationSession, error) {
	switch station.Type {
	case RelayStationTypeNewAPI:
		return loginNewAPIStation(ctx, s, station)
	case RelayStationTypeSub2API:
		return loginSub2APIStation(ctx, s, station)
	default:
		return nil, errors.New("relay station type does not use login")
	}
}

func loginNewAPIStation(ctx context.Context, service *RelayStationService, station relayStation) (*relayStationSession, error) {
	client, err := newRelayControlClient()
	if err != nil {
		return nil, err
	}
	endpoint, err := relayEndpoint(station.ControlURL, "/api/user/login")
	if err != nil {
		return nil, err
	}
	if err := service.validateRelayURL(endpoint); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"username": station.Username, "password": station.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login newapi station: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Success *bool `json:"success"`
		Data    struct {
			ID              int64  `json:"id"`
			AccessToken     string `json:"access_token"`
			AccessExpiresAt int64  `json:"access_expires_at"`
			User            struct {
				ID int64 `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Success != nil && !*payload.Success {
		return nil, errNewAPIStationLoginRejected
	}
	userID := payload.Data.ID
	if userID == 0 {
		userID = payload.Data.User.ID
	}
	expiresAt := time.Now().Add(relayStationSessionFallbackTTL)
	if payload.Data.AccessExpiresAt > time.Now().Add(relayStationSessionExpirySkew).Unix() {
		expiresAt = time.Unix(payload.Data.AccessExpiresAt, 0).Add(-relayStationSessionExpirySkew)
	}
	return &relayStationSession{
		client:      client,
		token:       strings.TrimSpace(payload.Data.AccessToken),
		userID:      strconv.FormatInt(userID, 10),
		expiresAt:   expiresAt,
		proxyTokens: make(map[string]string),
	}, nil
}

func setNewAPIAuth(req *http.Request, session *relayStationSession) {
	if session == nil {
		return
	}
	if session.token != "" {
		req.Header.Set("Authorization", "Bearer "+session.token)
	}
	if session.userID != "" && session.userID != "0" {
		req.Header.Set("New-Api-User", session.userID)
	}
}

func loginSub2APIStation(ctx context.Context, service *RelayStationService, station relayStation) (*relayStationSession, error) {
	endpoint, err := relayEndpoint(station.ControlURL, "/api/v1/auth/login")
	if err != nil {
		return nil, err
	}
	if err := service.validateRelayURL(endpoint); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"email": station.Username, "password": station.Password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := newRelayControlClient()
	if err != nil {
		return nil, err
	}
	response, err := resp.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login sub2api station: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &relayHTTPStatusError{status: response.StatusCode}
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := decodeRelayJSON(response.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Code != 0 || strings.TrimSpace(payload.Data.AccessToken) == "" {
		return nil, errors.New("sub2api login was rejected")
	}
	return &relayStationSession{
		token:       strings.TrimSpace(payload.Data.AccessToken),
		expiresAt:   time.Now().Add(10 * time.Minute),
		proxyTokens: make(map[string]string),
	}, nil
}

// SyncAIHubConfig applies each binding policy to its isolated managed runtime.
func (s *RelayStationService) SyncAIHubConfig(ctx context.Context) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	configSnapshot := s.snapshotConfig()
	var syncErrors []error
	for _, station := range configSnapshot.Stations {
		if station.Type != RelayStationTypeAIHub || !station.Enabled {
			continue
		}
		policies := make(map[string]relayAIHubConfig)
		for _, binding := range configSnapshot.Bindings {
			for _, source := range binding.Sources {
				if source.StationID == station.ID && source.Enabled {
					policies[source.PolicyKey] = relayAIHubPolicyForSource(source)
				}
			}
		}
		for policyKey, policy := range policies {
			runtimeID := relayAIHubRuntimeID(station.ID, policyKey)
			s.applyAIHubConnectionDefaults(&station)
			if err := s.postAIHubConfig(ctx, station, policy, runtimeID); err != nil {
				syncErrors = append(syncErrors, err)
			}
		}
	}
	return errors.Join(syncErrors...)
}

type relayAIHubConfig struct {
	Mode             string          `json:"mode,omitempty"`
	AccountPoolPlans []string        `json:"accountPoolPlans"`
	PriceBand        *RelayPriceBand `json:"priceBand"`
}

func (s *RelayStationService) postAIHubConfig(ctx context.Context, station relayStation, policy relayAIHubConfig, runtimeID string) error {
	s.applyAIHubConnectionDefaults(&station)
	if station.Type != RelayStationTypeAIHub || !station.Enabled {
		return nil
	}
	_, err := s.activateAIHubStation(ctx, station, runtimeID, &policy, true)
	return err
}

func (s *RelayStationService) postAIHubConfigRequest(ctx context.Context, station relayStation, policy relayAIHubConfig, runtimeID string) error {
	endpoint, err := relayEndpoint(station.ControlURL, "/ctl/config")
	if err != nil {
		return err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return err
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ui-password", station.UIPassword)
	req.Header.Set(relayAIHubAccountHeader, runtimeID)
	client, err := newRelayControlClient()
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sync aihub configuration: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &relayHTTPStatusError{status: response.StatusCode}
	}
	return nil
}

func (s *RelayStationService) relayProxyToken(ctx context.Context, station relayStation, source RelayStationSource) (string, error) {
	sourceGroup := normalizeRelaySourceGroup(source.SourceGroup)
	keyName := strings.TrimSpace(source.APIKeyName)
	if keyName == "" {
		keyName = strings.TrimSpace(station.APIKeyName)
	}
	if keyName == "" {
		keyName = strings.TrimSpace(station.Name) + " - " + sourceGroup
	}
	// A persisted source-group key is already sufficient for proxy traffic. Do
	// not require an administrator login just to reuse it: login outages must
	// not force needless key creation or take a previously working relay down.
	if stored := station.APIKeys[sourceGroup]; stored.Key != "" && stored.Name == keyName {
		return stored.Key, nil
	}
	if stored, ok := s.currentRelayAPIKey(station.ID, sourceGroup, keyName); ok {
		return stored.Key, nil
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		session, err := s.relaySession(ctx, station)
		if err != nil {
			return "", err
		}
		session.keysMu.Lock()
		if session.proxyTokens == nil {
			session.proxyTokens = make(map[string]string)
		}
		if stored, ok := s.currentRelayAPIKey(station.ID, sourceGroup, keyName); ok {
			session.proxyTokens[sourceGroup] = stored.Key
		}
		if stored := station.APIKeys[sourceGroup]; stored.Key != "" && stored.Name == keyName {
			session.proxyTokens[sourceGroup] = stored.Key
		}
		if token := session.proxyTokens[sourceGroup]; token != "" {
			session.keysMu.Unlock()
			return token, nil
		}
		switch station.Type {
		case RelayStationTypeNewAPI:
			lastErr = nil
			var token string
			token, err = s.createNewAPIProxyToken(ctx, station, session, sourceGroup, keyName)
			if err == nil {
				session.proxyTokens[sourceGroup] = token
			}
		case RelayStationTypeSub2API:
			lastErr = nil
			var token string
			token, err = s.createSub2APIProxyToken(ctx, station, session.token, sourceGroup, keyName)
			if err == nil {
				session.proxyTokens[sourceGroup] = token
			}
		default:
			err = errors.New("relay station type does not use platform API keys")
		}
		if err == nil {
			lastErr = s.persistRelayAPIKey(ctx, station.ID, sourceGroup, relayAPIKey{Name: keyName, Key: session.proxyTokens[sourceGroup]})
		}
		token := session.proxyTokens[sourceGroup]
		session.keysMu.Unlock()
		if lastErr == nil && err == nil {
			return token, nil
		}
		if lastErr == nil {
			lastErr = err
		}
		var statusErr *relayHTTPStatusError
		if !errors.As(lastErr, &statusErr) || (statusErr.status != http.StatusUnauthorized && statusErr.status != http.StatusForbidden) {
			break
		}
		s.clearRelaySession(station.ID)
	}
	return "", lastErr
}

func (s *RelayStationService) currentRelayAPIKey(stationID, sourceGroup, keyName string) (relayAPIKey, bool) {
	if s == nil {
		return relayAPIKey{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	station, found := findRelayStation(s.config.Stations, stationID)
	if !found {
		return relayAPIKey{}, false
	}
	stored := station.APIKeys[sourceGroup]
	return stored, stored.Key != "" && stored.Name == keyName
}

type relayNewAPIToken struct {
	ID     int    `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Group  string `json:"group"`
	Status int    `json:"status"`
}

func (s *RelayStationService) createNewAPIProxyToken(ctx context.Context, station relayStation, session *relayStationSession, sourceGroup, keyName string) (string, error) {
	endpoint, err := relayEndpoint(station.ControlURL, "/api/token/")
	if err != nil {
		return "", err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"name": keyName, "group": sourceGroup, "unlimited_quota": true, "expired_time": -1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	setNewAPIAuth(req, session)
	resp, err := session.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create newapi token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &relayHTTPStatusError{status: resp.StatusCode}
	}
	var created struct {
		Success *bool `json:"success"`
	}
	if err := decodeRelayJSON(resp.Body, &created); err != nil {
		return "", err
	}
	if created.Success != nil && !*created.Success {
		return "", errors.New("newapi token creation was rejected")
	}

	return s.findNewAPIProxyToken(ctx, station, session, sourceGroup, keyName)
}

func (s *RelayStationService) findNewAPIProxyToken(ctx context.Context, station relayStation, session *relayStationSession, sourceGroup, keyName string) (string, error) {
	endpoint, err := relayEndpoint(station.ControlURL, "/api/token/")
	if err != nil {
		return "", err
	}
	parsed, _ := url.Parse(endpoint)
	query := parsed.Query()
	query.Set("p", "1")
	query.Set("size", "100")
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	setNewAPIAuth(req, session)
	resp, err := session.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("list newapi tokens: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return "", err
	}
	var page struct {
		Items []relayNewAPIToken `json:"items"`
	}
	_ = json.Unmarshal(payload.Data, &page)
	if len(page.Items) == 0 {
		_ = json.Unmarshal(payload.Data, &page.Items)
	}
	for _, token := range page.Items {
		group := strings.TrimSpace(token.Group)
		if group == "" {
			group = "default"
		}
		if group != sourceGroup || token.Name != keyName || token.Status != 1 {
			continue
		}
		key := strings.TrimSpace(token.Key)
		if key == "" || strings.Contains(key, "*") {
			keyEndpoint, e := relayEndpoint(station.ControlURL, fmt.Sprintf("/api/token/%d/key", token.ID))
			if e != nil {
				return "", e
			}
			keyReq, e := http.NewRequestWithContext(ctx, http.MethodPost, keyEndpoint, nil)
			if e != nil {
				return "", e
			}
			setNewAPIAuth(keyReq, session)
			keyResp, e := session.client.Do(keyReq)
			if e != nil {
				return "", fmt.Errorf("get newapi token key: %w", e)
			}
			var keyPayload struct {
				Data struct {
					Key string `json:"key"`
				} `json:"data"`
			}
			decodeErr := decodeRelayJSON(keyResp.Body, &keyPayload)
			_ = keyResp.Body.Close()
			if keyResp.StatusCode < http.StatusOK || keyResp.StatusCode >= http.StatusMultipleChoices {
				return "", &relayHTTPStatusError{status: keyResp.StatusCode}
			}
			if decodeErr != nil {
				return "", decodeErr
			}
			key = keyPayload.Data.Key
		}
		if key != "" {
			return "sk-" + strings.TrimPrefix(key, "sk-"), nil
		}
	}
	return "", errors.New("newapi created token was not returned")
}

func (s *RelayStationService) createSub2APIProxyToken(ctx context.Context, station relayStation, token, sourceGroup, keyName string) (string, error) {
	groupsEndpoint, err := relayEndpoint(station.ControlURL, "/api/v1/groups/available")
	if err != nil {
		return "", err
	}
	groupsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, groupsEndpoint, nil)
	if err != nil {
		return "", err
	}
	groupsReq.Header.Set("Authorization", "Bearer "+token)
	groupsResp, err := newRelayProxyClient().Do(groupsReq)
	if err != nil {
		return "", fmt.Errorf("list sub2api groups: %w", err)
	}
	var groupsPayload struct {
		Code int `json:"code"`
		Data []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	decodeErr := decodeRelayJSON(groupsResp.Body, &groupsPayload)
	_ = groupsResp.Body.Close()
	if groupsResp.StatusCode < http.StatusOK || groupsResp.StatusCode >= http.StatusMultipleChoices {
		return "", &relayHTTPStatusError{status: groupsResp.StatusCode}
	}
	if decodeErr != nil {
		return "", decodeErr
	}
	var groupID int64
	for _, group := range groupsPayload.Data {
		if strings.TrimSpace(group.Name) == sourceGroup {
			groupID = group.ID
			break
		}
	}
	if groupID == 0 {
		return "", errors.New("sub2api source group was not found")
	}
	endpoint, err := relayEndpoint(station.ControlURL, "/api/v1/keys")
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"name": keyName, "group_id": groupID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := newRelayProxyClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("create sub2api API key: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &relayHTTPStatusError{status: resp.StatusCode}
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return "", err
	}
	if payload.Code != 0 || strings.TrimSpace(payload.Data.Key) == "" {
		return "", errors.New("sub2api API key creation was rejected")
	}
	return strings.TrimSpace(payload.Data.Key), nil
}

type relayForwardError struct {
	stage string
	err   error
}

func (e *relayForwardError) Error() string {
	if e == nil || e.err == nil {
		return "relay forwarding failed"
	}
	return e.err.Error()
}

func (e *relayForwardError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func wrapRelayForwardError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &relayForwardError{stage: stage, err: err}
}

func relayForwardFailureStage(err error) string {
	var forwardErr *relayForwardError
	if errors.As(err, &forwardErr) && forwardErr.stage != "" {
		return forwardErr.stage
	}
	return "unknown"
}

func relaySafeForwardErrorMessage(err error) string {
	var statusErr *relayHTTPStatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("http status %d", statusErr.status)
	}
	if err == nil {
		return ""
	}
	return "internal relay failure"
}

// Forward keeps the legacy adapter entry point for station-level tests.
func (s *RelayStationService) Forward(ctx context.Context, route *RelayRoute, inbound *http.Request) (*http.Response, error) {
	return s.ForwardAccount(ctx, nil, route, inbound)
}

// ForwardAccount applies native account header overrides before forwarding the
// selected relay account through its station adapter. Upstream failures are
// logged internally and collapsed so callers cannot expose station details.
func (s *RelayStationService) ForwardAccount(ctx context.Context, account *Account, route *RelayRoute, inbound *http.Request) (*http.Response, error) {
	startedAt := time.Now()
	response, err := s.forward(ctx, account, route, inbound)
	if err != nil {
		fields := relayForwardDiagnosticFields(account, route, inbound)
		fields = append(fields,
			zap.String("failure_stage", relayForwardFailureStage(err)),
			zap.String("error_message", relaySafeForwardErrorMessage(err)),
			zap.String("error_type", fmt.Sprintf("%T", err)),
			zap.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
		)
		logger.L().Warn("relay upstream request failed", fields...)
		return nil, relayUpstreamFailure(0)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		if route.station.Type == RelayStationTypeAIHub {
			rawRate := RelaySelectedRate(response.Header)
			if rawRate == nil {
				_ = response.Body.Close()
				logger.L().Warn("managed AIHub response omitted valid selected rate",
					zap.String("station_id", relayRouteStationID(route)),
					zap.String("failure_stage", "validate_aihub_rate"),
					zap.Int("upstream_status", response.StatusCode),
					zap.String("upstream_content_type", response.Header.Get("Content-Type")),
				)
				return nil, relayUpstreamFailure(0)
			}
			effectiveRate, routeable := relayEffectiveRate(RelayStationRate{Rate: rawRate, Status: RelayRateStatusReady}, route.source)
			if !routeable {
				_ = response.Body.Close()
				logger.L().Warn("managed AIHub selected rate is not routeable",
					zap.String("station_id", relayRouteStationID(route)),
					zap.String("failure_stage", "validate_aihub_rate"),
					zap.Int("upstream_status", response.StatusCode),
					zap.String("upstream_content_type", response.Header.Get("Content-Type")),
				)
				return nil, relayUpstreamFailure(0)
			}
			response.Header.Set("x-aihub-auto-rate", strconv.FormatFloat(*rawRate, 'g', -1, 64))
			if account != nil {
				updatedAt := time.Now()
				account.setRelayEffectiveRate(effectiveRate, updatedAt)
				account.setRelayUpstreamRate(*rawRate, updatedAt)
			}
		} else {
			response.Header.Del("x-aihub-auto-rate")
		}
		if encoding := strings.TrimSpace(strings.ToLower(response.Header.Get("Content-Encoding"))); encoding != "" && encoding != "identity" {
			_ = response.Body.Close()
			logger.L().Warn("relay upstream returned unsupported content encoding",
				zap.String("station_id", relayRouteStationID(route)),
				zap.String("failure_stage", "validate_response_encoding"),
				zap.Int("upstream_status", response.StatusCode),
				zap.String("content_encoding", encoding),
			)
			return nil, relayUpstreamFailure(0)
		}
		response.Header.Del("Content-Encoding")
		contentType := strings.ToLower(response.Header.Get("Content-Type"))
		if (relayRequestExpectsSSE(inbound) && !strings.Contains(contentType, "application/json")) || strings.Contains(contentType, "text/event-stream") {
			response.Header.Set("Content-Type", "text/event-stream")
			response.Body = newRelaySanitizedSSEBody(response.Body, inbound.URL.Path, relayRouteStationID(route))
			return response, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, relayResponseSanitizeLimit+1))
		_ = response.Body.Close()
		if readErr != nil {
			logger.L().Warn("relay upstream response read failed",
				zap.String("station_id", relayRouteStationID(route)),
				zap.String("failure_stage", "read_response"),
				zap.Int("upstream_status", response.StatusCode),
				zap.String("upstream_content_type", response.Header.Get("Content-Type")),
				zap.String("error_type", fmt.Sprintf("%T", readErr)),
			)
			return nil, relayUpstreamFailure(0)
		}
		if len(body) > relayResponseSanitizeLimit {
			logger.L().Warn("relay upstream success response exceeded sanitization limit",
				zap.String("station_id", relayRouteStationID(route)),
				zap.String("failure_stage", "validate_response_size"),
				zap.Int("upstream_status", response.StatusCode),
				zap.String("upstream_content_type", response.Header.Get("Content-Type")),
			)
			return nil, relayUpstreamFailure(0)
		}
		if !relayValidJSONSuccess(body, inbound.URL.Path) {
			logger.L().Warn("relay upstream returned an invalid or failed successful-status payload",
				zap.String("station_id", relayRouteStationID(route)),
				zap.String("failure_stage", "validate_response_payload"),
				zap.Int("upstream_status", response.StatusCode),
				zap.String("upstream_content_type", response.Header.Get("Content-Type")),
			)
			return nil, relayUpstreamFailure(0)
		}
		response.Header.Set("Content-Type", "application/json")
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		return response, nil
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
	fields := relayForwardDiagnosticFields(account, route, inbound)
	fields = append(fields,
		zap.String("failure_stage", "upstream_http_status"),
		zap.Int("upstream_status", response.StatusCode),
		zap.String("upstream_content_type", response.Header.Get("Content-Type")),
		zap.String("upstream_request_id", response.Header.Get("x-request-id")),
		zap.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
	)
	logger.L().Warn("relay upstream returned an error", fields...)
	return nil, relayUpstreamFailure(response.StatusCode)
}

func relayRequestExpectsSSE(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	if request.Header.Get("X-Sub2API-Relay-Expected-Stream") == "1" {
		return true
	}
	if strings.Contains(strings.ToLower(request.Header.Get("Accept")), "text/event-stream") {
		return true
	}
	if strings.EqualFold(request.URL.Query().Get("alt"), "sse") {
		return true
	}
	return strings.Contains(strings.ToLower(request.URL.Path), "streamgeneratecontent")
}

func relayValidJSONSuccess(payload []byte, path string) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	parsed := gjson.ParseBytes(payload)
	if parsed.IsArray() || relayJSONErrorPayload(payload) {
		return false
	}
	path = strings.ToLower(path)
	switch {
	case strings.Contains(path, "/chat/completions"):
		return parsed.Get("choices").Exists()
	case strings.Contains(path, "/embeddings"):
		return parsed.Get("data").Exists()
	case strings.Contains(path, "/messages/count_tokens"):
		return parsed.Get("input_tokens").Exists() || parsed.Get("inputTokens").Exists()
	case strings.Contains(path, "/messages"):
		return strings.EqualFold(parsed.Get("type").String(), "message") || parsed.Get("content").Exists()
	case strings.Contains(path, "/responses"):
		return strings.HasPrefix(strings.ToLower(parsed.Get("type").String()), "response") || parsed.Get("output").Exists() || strings.HasPrefix(parsed.Get("id").String(), "resp_")
	case strings.Contains(path, "/v1beta/models/") && strings.Contains(path, ":"):
		return parsed.Get("candidates").Exists() || parsed.Get("usageMetadata").Exists() || parsed.Get("promptFeedback").Exists()
	case strings.HasSuffix(path, "/v1beta/models"):
		return parsed.Get("models").Exists()
	case strings.HasSuffix(path, "/v1/models"):
		return parsed.Get("data").Exists() || parsed.Get("models").Exists()
	case strings.Contains(path, "/v1/models/"):
		return parsed.Get("id").Exists() || parsed.Get("name").Exists()
	case strings.Contains(path, "/v1beta/models/"):
		return parsed.Get("name").Exists()
	default:
		return false
	}
}

func relayRouteStationID(route *RelayRoute) string {
	if route == nil {
		return ""
	}
	return route.station.ID
}

func relayForwardDiagnosticFields(account *Account, route *RelayRoute, inbound *http.Request) []zap.Field {
	fields := []zap.Field{
		zap.String("station_id", relayRouteStationID(route)),
		zap.String("source_group", routeSourceGroup(route)),
	}
	if account != nil {
		fields = append(fields, zap.Int64("account_id", account.ID))
	}
	if inbound != nil && inbound.URL != nil {
		fields = append(fields,
			zap.String("request_method", inbound.Method),
			zap.String("request_path", inbound.URL.Path),
		)
	}
	return fields
}

func routeSourceGroup(route *RelayRoute) string {
	if route == nil {
		return ""
	}
	return route.source.SourceGroup
}

type relaySanitizedSSEBody struct {
	body                  io.ReadCloser
	reader                *bufio.Reader
	stationID             string
	requestPath           string
	bufferResponsesOutput bool
	clientOutputStarted   bool
	preamble              []byte
	pending               []byte
	terminalErr           error
	done                  bool
}

func newRelaySanitizedSSEBody(body io.ReadCloser, path, stationID string) io.ReadCloser {
	return &relaySanitizedSSEBody{
		body:                  body,
		reader:                bufio.NewReader(body),
		stationID:             stationID,
		requestPath:           path,
		bufferResponsesOutput: strings.Contains(strings.ToLower(path), "/responses"),
	}
}

func (r *relaySanitizedSSEBody) Read(destination []byte) (int, error) {
	for len(r.pending) == 0 && !r.done {
		event, err := readRelaySSEEvent(r.reader)
		if err != nil && !errors.Is(err, io.EOF) {
			logger.L().Warn("relay upstream stream read failed",
				zap.String("station_id", r.stationID),
				zap.String("request_path", r.requestPath),
				zap.String("failure_stage", "read_stream"),
				zap.String("error_type", fmt.Sprintf("%T", err)),
			)
			r.failBeforeClientError()
		} else if len(event) > 0 {
			if relaySSEErrorEvent(event) {
				logger.L().Warn("relay upstream returned a streaming error",
					zap.String("station_id", r.stationID),
					zap.String("request_path", r.requestPath),
					zap.String("failure_stage", "stream_error_event"),
				)
				r.failBeforeClientError()
			} else if r.bufferResponsesOutput && !r.clientOutputStarted {
				data, eventType := relayOpenAISSEEventData(event)
				if relayResponsesSSETerminatedWithoutOutput(data, eventType) {
					logger.L().Warn("relay upstream returned an empty Responses stream",
						zap.String("station_id", r.stationID),
						zap.String("request_path", r.requestPath),
						zap.String("failure_stage", "empty_responses_stream"),
					)
					r.failBeforeClientError()
				} else if openAIStreamDataStartsClientOutput(string(data), eventType) {
					r.clientOutputStarted = true
					r.pending = append(r.pending, r.preamble...)
					r.pending = append(r.pending, event...)
					r.preamble = nil
				} else if len(r.preamble)+len(event) > relaySSEEventLimit {
					logger.L().Warn("relay upstream Responses preamble exceeded sanitization limit",
						zap.String("station_id", r.stationID),
						zap.String("request_path", r.requestPath),
						zap.String("failure_stage", "responses_preamble_limit"),
					)
					r.failBeforeClientError()
				} else {
					r.preamble = append(r.preamble, event...)
					if errors.Is(err, io.EOF) {
						r.failBeforeClientError()
					}
				}
			} else {
				r.pending = event
				if errors.Is(err, io.EOF) {
					r.done = true
				}
			}
		} else if err != nil {
			if r.bufferResponsesOutput && !r.clientOutputStarted {
				r.failBeforeClientError()
			} else {
				r.done = true
				return 0, err
			}
		}
	}
	if len(r.pending) == 0 {
		if r.terminalErr != nil {
			err := r.terminalErr
			r.terminalErr = nil
			return 0, err
		}
		return 0, io.EOF
	}
	count := copy(destination, r.pending)
	r.pending = r.pending[count:]
	return count, nil
}

func (r *relaySanitizedSSEBody) failBeforeClientError() {
	r.preamble = nil
	r.terminalErr = ErrRelayUpstreamFailed
	r.done = true
	_ = r.body.Close()
}

func (r *relaySanitizedSSEBody) Close() error {
	r.done = true
	return r.body.Close()
}

func readRelaySSEEvent(reader *bufio.Reader) ([]byte, error) {
	var event []byte
	for {
		line, err := reader.ReadBytes('\n')
		event = append(event, line...)
		if len(event) > relaySSEEventLimit {
			return nil, errors.New("relay SSE event exceeded sanitization limit")
		}
		if len(line) > 0 && len(bytes.TrimSpace(line)) == 0 {
			return event, err
		}
		if err != nil {
			return event, err
		}
	}
}

func relayOpenAISSEEventData(event []byte) ([]byte, string) {
	var data []byte
	eventType := ""
	for _, line := range bytes.Split(event, []byte{'\n'}) {
		trimmed := strings.TrimSpace(string(line))
		if value, ok := extractOpenAISSEDataLine(trimmed); ok {
			data = append(data, value...)
			continue
		}
		if value, ok := extractOpenAISSEEventLine(trimmed); ok {
			eventType = value
		}
	}
	return data, effectiveOpenAISSEEventType(data, eventType)
}

func relayResponsesSSETerminatedWithoutOutput(data []byte, eventType string) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return true
	}
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done":
		return openAIResponsesCompletedEventIsEmpty(data, nil)
	default:
		return false
	}
}

func relaySSEErrorEvent(event []byte) bool {
	trimmedEvent := bytes.TrimSpace(event)
	if len(trimmedEvent) == 0 {
		return false
	}
	if bytes.HasPrefix(trimmedEvent, []byte("{")) || bytes.HasPrefix(trimmedEvent, []byte("[")) {
		return true
	}

	var data []byte
	hasFrame := false
	for _, line := range bytes.Split(event, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) {
			continue
		}
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			hasFrame = true
			eventName := strings.ToLower(strings.TrimSpace(string(bytes.TrimPrefix(trimmed, []byte("event:")))))
			if eventName == "error" || strings.Contains(eventName, "failed") {
				return true
			}
		case bytes.HasPrefix(trimmed, []byte("data:")):
			hasFrame = true
			data = append(data, bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))...)
		case bytes.HasPrefix(trimmed, []byte("id:")), bytes.HasPrefix(trimmed, []byte("retry:")):
			hasFrame = true
		default:
			return true
		}
	}
	if !hasFrame || len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return false
	}
	return !gjson.ValidBytes(data) || relayJSONErrorPayload(data)
}

func relayJSONErrorPayload(payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	parsed := gjson.ParseBytes(payload)
	typeName := strings.ToLower(strings.TrimSpace(parsed.Get("type").String()))
	status := strings.ToLower(strings.TrimSpace(parsed.Get("response.status").String()))
	return parsed.Get("error").Exists() || parsed.Get("response.error").Exists() ||
		typeName == "error" || strings.Contains(typeName, "failed") || status == "failed"
}

// forward constructs an OpenAI-compatible upstream request with only
// server-side station credentials. It intentionally never forwards the buyer's
// Authorization or cookie headers to an upstream relay.
func (s *RelayStationService) forward(ctx context.Context, account *Account, route *RelayRoute, inbound *http.Request) (*http.Response, error) {
	if route == nil || inbound == nil || inbound.URL == nil {
		return nil, wrapRelayForwardError("validate_request", errors.New("relay request is invalid"))
	}
	station := route.station
	if station.Type == RelayStationTypeAIHub {
		s.applyAIHubConnectionDefaults(&station)
	}
	target, err := relayProxyTarget(station.BaseURL, inbound.URL.Path, relayForwardQuery(inbound.URL.Query()).Encode())
	if err != nil {
		return nil, wrapRelayForwardError("build_target", err)
	}
	if err := s.validateRelayTargetURL(target); err != nil {
		return nil, wrapRelayForwardError("validate_target", err)
	}
	outbound, err := http.NewRequestWithContext(ctx, inbound.Method, target, inbound.Body)
	if err != nil {
		return nil, wrapRelayForwardError("build_request", err)
	}
	outbound.ContentLength = inbound.ContentLength
	copyRelayHeaders(outbound.Header, inbound.Header)
	if account != nil {
		account.ApplyHeaderOverrides(outbound.Header)
	}

	if station.Type == RelayStationTypeAIHub {
		runtimeID := relayAIHubRuntimeID(station.ID, route.source.PolicyKey)
		policy := relayAIHubPolicyForSource(route.source)
		if _, err := s.activateAIHubStation(ctx, station, runtimeID, &policy, false); err != nil {
			return nil, wrapRelayForwardError("activate_aihub", err)
		}
		if token := strings.TrimSpace(station.ProxyToken); token != "" {
			outbound.Header.Set("Authorization", "Bearer "+token)
		}
		if maxRate := strings.TrimSpace(inbound.Header.Get(relayMaxRateHeader)); maxRate != "" {
			outbound.Header.Set(relayMaxRateHeader, maxRate)
		}
		outbound.Header.Set(relayAIHubAccountHeader, runtimeID)
		outbound.Header.Set("X-Sub2api-Group", route.source.SourceGroup)
		route.runtimeID = runtimeID
		// #nosec G704 -- target passed validateRelayTargetURL before request construction.
		response, err := newRelayProxyClient().Do(outbound)
		if err != nil {
			return nil, wrapRelayForwardError("send_request", err)
		}
		return response, nil
	}

	proxyToken := strings.TrimSpace(station.ProxyToken)
	if proxyToken == "" {
		proxyToken, err = s.relayProxyToken(ctx, station, route.source)
		if err != nil {
			return nil, wrapRelayForwardError("resolve_proxy_token", infraerrors.New(http.StatusBadGateway, "RELAY_PROXY_TOKEN_UNAVAILABLE", err.Error()))
		}
	}
	if proxyToken == "" {
		return nil, wrapRelayForwardError("resolve_proxy_token", infraerrors.New(http.StatusBadGateway, "RELAY_PROXY_TOKEN_REQUIRED", "relay station has no usable API key"))
	}
	outbound.Header.Set("Authorization", "Bearer "+proxyToken)
	// #nosec G704 -- target passed validateRelayTargetURL before request construction.
	response, err := newRelayProxyClient().Do(outbound)
	if err != nil {
		return nil, wrapRelayForwardError("send_request", err)
	}
	return response, nil
}

// RelaySelectedRate returns trusted same-decision metadata after ForwardAccount
// has validated its managed AIHub source and stripped the header for other stations.
func RelaySelectedRate(headers http.Header) *float64 {
	value := strings.TrimSpace(headers.Get("x-aihub-auto-rate"))
	rate, err := strconv.ParseFloat(value, 64)
	if value == "" || err != nil || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return nil
	}
	return &rate
}

func relayProxyTarget(baseURL, inboundPath, rawQuery string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("relay base url is invalid")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	requestPath := "/" + strings.TrimLeft(inboundPath, "/")
	if strings.HasPrefix(requestPath, "/backend-api/codex/") {
		requestPath = strings.TrimPrefix(requestPath, "/backend-api/codex")
	}
	if strings.HasPrefix(requestPath, "/v1beta/") && strings.HasSuffix(basePath, "/v1") {
		basePath = strings.TrimSuffix(basePath, "/v1")
	} else if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(requestPath, "/v1/") {
		requestPath = strings.TrimPrefix(requestPath, "/v1")
	} else if basePath == "" && !strings.HasPrefix(requestPath, "/v1/") && !strings.HasPrefix(requestPath, "/v1beta/") {
		requestPath = "/v1" + requestPath
	}
	parsed.Path = basePath + requestPath
	parsed.RawPath = ""
	parsed.RawQuery = rawQuery
	return parsed.String(), nil
}

func relayForwardQuery(query url.Values) url.Values {
	clean := make(url.Values, len(query))
	for key, values := range query {
		if relaySensitiveQueryParameter(key) {
			continue
		}
		clean[key] = append([]string(nil), values...)
	}
	return clean
}

func relaySensitiveQueryParameter(key string) bool {
	name := strings.ToLower(strings.TrimSpace(key))
	normalized := strings.NewReplacer("-", "_", ".", "_").Replace(name)
	if normalized == "key" || normalized == "auth" || normalized == "authorization" || normalized == "apikey" || normalized == "api_key" || normalized == "x_api_key" || normalized == "sig" || normalized == "hmac" {
		return true
	}
	for _, marker := range []string{"token", "secret", "credential", "password", "signature", "access", "refresh"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return strings.HasSuffix(normalized, "_key")
}

func copyRelayHeaders(destination, source http.Header) {
	allowed := map[string]struct{}{
		"accept":              {},
		"accept-language":     {},
		"anthropic-beta":      {},
		"anthropic-version":   {},
		"content-type":        {},
		"openai-beta":         {},
		"user-agent":          {},
		"x-client-request-id": {},
		"x-request-id":        {},
	}
	for key, values := range source {
		if _, ok := allowed[strings.ToLower(key)]; !ok {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func decodeRelayJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 2<<20))
	if err := decoder.Decode(target); err != nil {
		return errors.New("relay upstream returned invalid JSON")
	}
	return nil
}
