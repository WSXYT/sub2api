package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const relayStationControlTimeout = 15 * time.Second

type relayStationSession struct {
	client      *http.Client
	token       string
	expiresAt   time.Time
	keysMu      sync.Mutex
	proxyTokens map[string]string
}

type relayHTTPStatusError struct {
	status int
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
	s.aihubMu.Lock()
	defer s.aihubMu.Unlock()
	switched, err := s.activateAIHubStationLocked(ctx, station)
	if err != nil {
		return nil, err
	}
	if switched && station.Username != "" {
		if err := s.postAIHubConfigRequest(ctx, station, s.aiHubGroupsForStation(station.ID)); err != nil {
			s.activeAIHubStationID = ""
			return nil, err
		}
	}

	endpoint, err := relayEndpoint(station.ControlURL, "/ctl/status")
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
	client, err := newRelayControlClient()
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request aihub status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &relayHTTPStatusError{status: resp.StatusCode}
	}

	var payload struct {
		CurrentGroupID *int64                      `json:"currentGroupId"`
		CurrentCode    string                      `json:"currentCode"`
		Candidates     []relayAIHubStatusCandidate `json:"candidates"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, err
	}

	candidates := make(map[string]relayAIHubStatusCandidate, len(payload.Candidates))
	candidatesByGroupID := make(map[int64]relayAIHubStatusCandidate, len(payload.Candidates))
	for _, candidate := range payload.Candidates {
		candidates[candidate.Code] = candidate
		candidatesByGroupID[candidate.GroupID] = candidate
	}
	current, currentFound := candidates[payload.CurrentCode]
	if !currentFound && payload.CurrentGroupID != nil {
		current, currentFound = candidatesByGroupID[*payload.CurrentGroupID]
	}

	result := make(map[string]RelayStationRate, len(required))
	for sourceGroup := range required {
		candidate, found := candidates[sourceGroup]
		if sourceGroup == "default" {
			candidate, found = current, currentFound
		}
		if !found || candidate.Rate == nil || candidate.Excluded {
			result[sourceGroup] = RelayStationRate{Status: RelayRateStatusUnavailable}
			continue
		}
		result[sourceGroup] = RelayStationRate{Rate: cloneFloat64(candidate.Rate), Status: RelayRateStatusReady}
	}
	return result, nil
}

func (s *RelayStationService) fetchAIHubBalance(ctx context.Context, station relayStation) (*float64, error) {
	s.applyAIHubConnectionDefaults(&station)
	s.aihubMu.Lock()
	defer s.aihubMu.Unlock()
	switched, err := s.activateAIHubStationLocked(ctx, station)
	if err != nil {
		return nil, err
	}
	if switched && station.Username != "" {
		if err := s.postAIHubConfigRequest(ctx, station, s.aiHubGroupsForStation(station.ID)); err != nil {
			s.activeAIHubStationID = ""
			return nil, err
		}
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

func (s *RelayStationService) activateAIHubStationLocked(ctx context.Context, station relayStation) (bool, error) {
	if s.activeAIHubStationID == station.ID {
		return false, nil
	}
	if station.Username != "" && station.Password != "" {
		if err := s.loginAIHubStation(ctx, station); err != nil {
			return false, err
		}
	}
	s.activeAIHubStationID = station.ID
	return true, nil
}

func (s *RelayStationService) clearActiveAIHubStation(stationID string) {
	s.aihubMu.Lock()
	if stationID == "" || s.activeAIHubStationID == stationID {
		s.activeAIHubStationID = ""
	}
	s.aihubMu.Unlock()
}

func (s *RelayStationService) loginAIHubStation(ctx context.Context, station relayStation) error {
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

func (s *RelayStationService) aiHubGroupsForStation(stationID string) map[string]relayAIHubGroupConfig {
	groups := make(map[string]relayAIHubGroupConfig)
	for _, binding := range s.snapshotConfig().Bindings {
		for _, source := range binding.Sources {
			if source.StationID != stationID || !source.Enabled {
				continue
			}
			groups[source.SourceGroup] = relayAIHubGroupConfig{
				Mode:      source.Mode,
				PriceBand: cloneRelayPriceBand(source.PriceBand),
			}
		}
	}
	return groups
}

type relayAIHubStatusCandidate struct {
	GroupID  int64    `json:"groupId"`
	Code     string   `json:"code"`
	Rate     *float64 `json:"rate"`
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
	if session.token != "" {
		req.Header.Set("Authorization", "Bearer "+session.token)
	}
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
		return existing, nil
	}

	session, err := s.loginRelayStation(ctx, station)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessions[station.ID] = session
	s.mu.Unlock()
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
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := decodeRelayJSON(resp.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Success != nil && !*payload.Success {
		return nil, errors.New("newapi login was rejected")
	}
	return &relayStationSession{
		client:      client,
		token:       strings.TrimSpace(payload.Data.AccessToken),
		expiresAt:   time.Now().Add(10 * time.Minute),
		proxyTokens: make(map[string]string),
	}, nil
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

// SyncAIHubConfig applies the bound aihub source-group policies through its
// authenticated control plane. It only sends policy fields, never credentials.
func (s *RelayStationService) SyncAIHubConfig(ctx context.Context) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	configSnapshot := s.snapshotConfig()
	stationByID := relayStationMap(configSnapshot.Stations)
	byStation := make(map[string]map[string]relayAIHubGroupConfig)
	for _, station := range configSnapshot.Stations {
		if station.Type == RelayStationTypeAIHub {
			byStation[station.ID] = make(map[string]relayAIHubGroupConfig)
		}
	}
	for _, binding := range configSnapshot.Bindings {
		for _, source := range binding.Sources {
			station, ok := stationByID[source.StationID]
			if !ok || !station.Enabled || !source.Enabled || station.Type != RelayStationTypeAIHub {
				continue
			}
			if byStation[station.ID] == nil {
				byStation[station.ID] = make(map[string]relayAIHubGroupConfig)
			}
			byStation[station.ID][source.SourceGroup] = relayAIHubGroupConfig{
				Mode:      source.Mode,
				PriceBand: cloneRelayPriceBand(source.PriceBand),
			}
		}
	}

	var syncErrors []error
	for stationID, groups := range byStation {
		station := stationByID[stationID]
		if err := s.postAIHubConfig(ctx, station, groups); err != nil {
			syncErrors = append(syncErrors, err)
		}
	}
	return errors.Join(syncErrors...)
}

type relayAIHubGroupConfig struct {
	Mode      string          `json:"mode,omitempty"`
	PriceBand *RelayPriceBand `json:"priceBand,omitempty"`
}

func (s *RelayStationService) postAIHubConfig(ctx context.Context, station relayStation, groups map[string]relayAIHubGroupConfig) error {
	s.applyAIHubConnectionDefaults(&station)
	s.aihubMu.Lock()
	defer s.aihubMu.Unlock()
	if _, err := s.activateAIHubStationLocked(ctx, station); err != nil {
		return err
	}
	if err := s.postAIHubConfigRequest(ctx, station, groups); err != nil {
		s.activeAIHubStationID = ""
		return err
	}
	return nil
}

func (s *RelayStationService) postAIHubConfigRequest(ctx context.Context, station relayStation, groups map[string]relayAIHubGroupConfig) error {
	endpoint, err := relayEndpoint(station.ControlURL, "/ctl/config")
	if err != nil {
		return err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Groups map[string]relayAIHubGroupConfig `json:"groups"`
	}{Groups: groups})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ui-password", station.UIPassword)
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

func (s *RelayStationService) relayProxyToken(ctx context.Context, station relayStation, sourceGroup string) (string, error) {
	sourceGroup = normalizeRelaySourceGroup(sourceGroup)
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
		if token := session.proxyTokens[sourceGroup]; token != "" {
			session.keysMu.Unlock()
			return token, nil
		}
		switch station.Type {
		case RelayStationTypeNewAPI:
			token, err := s.requestNewAPIProxyToken(ctx, station, session, sourceGroup)
			if err == nil {
				session.proxyTokens[sourceGroup] = token
			}
			lastErr = err
		case RelayStationTypeSub2API:
			token, err := s.requestSub2APIProxyToken(ctx, station, session.token, sourceGroup)
			if err == nil {
				session.proxyTokens[sourceGroup] = token
			}
			lastErr = err
		default:
			lastErr = errors.New("relay station type does not use platform API keys")
		}
		token := session.proxyTokens[sourceGroup]
		session.keysMu.Unlock()
		if lastErr == nil {
			return token, nil
		}
		var statusErr *relayHTTPStatusError
		if !errors.As(lastErr, &statusErr) || (statusErr.status != http.StatusUnauthorized && statusErr.status != http.StatusForbidden) {
			break
		}
		s.clearRelaySession(station.ID)
	}
	return "", lastErr
}

type relayNewAPIToken struct {
	ID     int    `json:"id"`
	Key    string `json:"key"`
	Group  string `json:"group"`
	Status int    `json:"status"`
}

func (s *RelayStationService) requestNewAPIProxyToken(ctx context.Context, station relayStation, session *relayStationSession, sourceGroup string) (string, error) {
	endpoint, err := relayEndpoint(station.ControlURL, "/api/token/")
	if err != nil {
		return "", err
	}
	if err := s.validateRelayURL(endpoint); err != nil {
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
	if session.token != "" {
		req.Header.Set("Authorization", "Bearer "+session.token)
	}
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
	if payload.Success != nil && !*payload.Success {
		return "", errors.New("newapi token list was rejected")
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
		if group != sourceGroup || token.Status != 1 {
			continue
		}
		if key := strings.TrimSpace(token.Key); key != "" && !strings.Contains(key, "*") {
			return "sk-" + strings.TrimPrefix(key, "sk-"), nil
		}
		keyEndpoint, err := relayEndpoint(station.ControlURL, fmt.Sprintf("/api/token/%d/key", token.ID))
		if err != nil {
			return "", err
		}
		if err := s.validateRelayURL(keyEndpoint); err != nil {
			return "", err
		}
		keyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, keyEndpoint, nil)
		if err != nil {
			return "", err
		}
		if session.token != "" {
			keyReq.Header.Set("Authorization", "Bearer "+session.token)
		}
		keyResp, err := session.client.Do(keyReq)
		if err != nil {
			return "", fmt.Errorf("get newapi token key: %w", err)
		}
		var keyPayload struct {
			Success *bool `json:"success"`
			Data    struct {
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
		if keyPayload.Success != nil && !*keyPayload.Success {
			return "", errors.New("newapi token key request was rejected")
		}
		if key := strings.TrimSpace(keyPayload.Data.Key); key != "" {
			return "sk-" + strings.TrimPrefix(key, "sk-"), nil
		}
	}
	return "", errors.New("newapi has no active API token for the source group")
}

func (s *RelayStationService) requestSub2APIProxyToken(ctx context.Context, station relayStation, token, sourceGroup string) (string, error) {
	groupsEndpoint, err := relayEndpoint(station.ControlURL, "/api/v1/groups/available")
	if err != nil {
		return "", err
	}
	if err := s.validateRelayURL(groupsEndpoint); err != nil {
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
	if groupsPayload.Code != 0 {
		return "", errors.New("sub2api group list was rejected")
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

	keysEndpoint, err := relayEndpoint(station.ControlURL, "/api/v1/keys")
	if err != nil {
		return "", err
	}
	if err := s.validateRelayURL(keysEndpoint); err != nil {
		return "", err
	}
	parsed, _ := url.Parse(keysEndpoint)
	query := parsed.Query()
	query.Set("page", "1")
	query.Set("page_size", "100")
	query.Set("group_id", fmt.Sprint(groupID))
	query.Set("status", "active")
	parsed.RawQuery = query.Encode()
	keysReq, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	keysReq.Header.Set("Authorization", "Bearer "+token)
	keysResp, err := newRelayProxyClient().Do(keysReq)
	if err != nil {
		return "", fmt.Errorf("list sub2api API keys: %w", err)
	}
	defer func() { _ = keysResp.Body.Close() }()
	if keysResp.StatusCode < http.StatusOK || keysResp.StatusCode >= http.StatusMultipleChoices {
		return "", &relayHTTPStatusError{status: keysResp.StatusCode}
	}
	var keysPayload struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				Key    string `json:"key"`
				Status string `json:"status"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := decodeRelayJSON(keysResp.Body, &keysPayload); err != nil {
		return "", err
	}
	if keysPayload.Code != 0 {
		return "", errors.New("sub2api API key list was rejected")
	}
	for _, key := range keysPayload.Data.Items {
		if key.Status == "active" && strings.TrimSpace(key.Key) != "" {
			return strings.TrimSpace(key.Key), nil
		}
	}
	return "", errors.New("sub2api has no active API key for the source group")
}

// Forward constructs an OpenAI-compatible upstream request with only
// server-side station credentials. It intentionally never forwards the buyer's
// Authorization or cookie headers to an upstream relay.
func (s *RelayStationService) Forward(ctx context.Context, route *RelayRoute, inbound *http.Request) (*http.Response, error) {
	if route == nil || inbound == nil || inbound.URL == nil {
		return nil, errors.New("relay request is invalid")
	}
	station := route.station
	if station.Type == RelayStationTypeAIHub {
		s.applyAIHubConnectionDefaults(&station)
	}
	target, err := relayProxyTarget(station.BaseURL, inbound.URL.Path, inbound.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	if err := s.validateRelayURL(target); err != nil {
		return nil, err
	}
	outbound, err := http.NewRequestWithContext(ctx, inbound.Method, target, inbound.Body)
	if err != nil {
		return nil, err
	}
	outbound.ContentLength = inbound.ContentLength
	copyRelayHeaders(outbound.Header, inbound.Header)

	if station.Type == RelayStationTypeAIHub {
		s.aihubMu.Lock()
		defer s.aihubMu.Unlock()
		switched, err := s.activateAIHubStationLocked(ctx, station)
		if err != nil {
			return nil, err
		}
		if switched && station.Username != "" {
			if err := s.postAIHubConfigRequest(ctx, station, s.aiHubGroupsForStation(station.ID)); err != nil {
				s.activeAIHubStationID = ""
				return nil, err
			}
		}
		if token := strings.TrimSpace(station.ProxyToken); token != "" {
			outbound.Header.Set("Authorization", "Bearer "+token)
		}
		outbound.Header.Set("X-Sub2api-Group", route.source.SourceGroup)
		return newRelayProxyClient().Do(outbound)
	}

	proxyToken := strings.TrimSpace(station.ProxyToken)
	if proxyToken == "" {
		proxyToken, err = s.relayProxyToken(ctx, station, route.source.SourceGroup)
		if err != nil {
			return nil, infraerrors.New(http.StatusBadGateway, "RELAY_PROXY_TOKEN_UNAVAILABLE", err.Error())
		}
	}
	if proxyToken == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "RELAY_PROXY_TOKEN_REQUIRED", "relay station has no usable API key")
	}
	outbound.Header.Set("Authorization", "Bearer "+proxyToken)
	return newRelayProxyClient().Do(outbound)
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
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(requestPath, "/v1/") {
		requestPath = strings.TrimPrefix(requestPath, "/v1")
	} else if basePath == "" && !strings.HasPrefix(requestPath, "/v1/") {
		requestPath = "/v1" + requestPath
	}
	parsed.Path = basePath + requestPath
	parsed.RawPath = ""
	parsed.RawQuery = rawQuery
	return parsed.String(), nil
}

func copyRelayHeaders(destination, source http.Header) {
	allowed := map[string]struct{}{
		"accept":              {},
		"accept-language":     {},
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
