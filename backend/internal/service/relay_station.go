package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/google/uuid"
)

const (
	SettingKeyRelayStationConfig    = "relay_station_config"
	SettingKeyRelayStationRateCache = "relay_station_rate_cache"

	relayStationRatePollInterval = time.Minute
	relayStationRouteTTL         = 5 * time.Minute
)

var (
	ErrRelayStationNotFound = infraerrors.NotFound("RELAY_STATION_NOT_FOUND", "relay station not found")
	ErrRelayRouteNotFound   = errors.New("relay route is not configured")
	ErrRelayRateUnavailable = infraerrors.New(http.StatusBadGateway, "RELAY_RATE_UNAVAILABLE", "no relay source has a current price")
)

// RelayStationType identifies an upstream relay implementation.
type RelayStationType string

const (
	managedAIHubRouterURL = "http://127.0.0.1:8787"
	managedAIHubUIPasswordEnv = "AIHUB_AUTO_UI_PASSWORD"
	managedAIHubProxyTokenEnv = "AIHUB_AUTO_PROXY_TOKEN"

	RelayStationTypeAIHub   RelayStationType = "aihub"
	RelayStationTypeNewAPI  RelayStationType = "newapi"
	RelayStationTypeSub2API RelayStationType = "sub2api"
)

func (t RelayStationType) valid() bool {
	switch t {
	case RelayStationTypeAIHub, RelayStationTypeNewAPI, RelayStationTypeSub2API:
		return true
	default:
		return false
	}
}

const (
	RelayRateStatusReady           = "ready"
	RelayRateStatusUnauthenticated = "unauthenticated"
	RelayRateStatusUnavailable     = "unavailable"
	RelayRateStatusStale           = "stale"
)

// RelayPriceBand is forwarded to aihub-auto when a source group is configured.
type RelayPriceBand struct {
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// RelayStationSource binds one local group to an upstream station and source group.
type RelayStationSource struct {
	StationID   string          `json:"station_id"`
	Enabled     bool            `json:"enabled"`
	SourceGroup string          `json:"source_group,omitempty"`
	Priority    int             `json:"priority"`
	Delta       float64         `json:"delta"`
	MaxRate     *float64        `json:"max_rate,omitempty"`
	Mode         string          `json:"mode,omitempty"`
	AccountPools []string        `json:"account_pools,omitempty"`
	AdjustRate   *bool           `json:"adjust_rate,omitempty"`
}

// RelayGroupBinding is persisted as part of the relay configuration.
type RelayGroupBinding struct {
	GroupID int64                `json:"group_id"`
	Sources []RelayStationSource `json:"sources"`
}

// RelayStationCreateInput carries server-side credentials on creation. It must
// never be returned directly from an HTTP handler.
type RelayStationCreateInput struct {
	Type       RelayStationType
	Name       string
	BaseURL    string
	ControlURL string
	UIPassword string
	ProxyToken string
	Username   string
	Password   string
	Enabled    *bool
}

// RelayStationUpdateInput uses pointers so an omitted secret remains unchanged.
type RelayStationUpdateInput struct {
	Name       *string
	BaseURL    *string
	ControlURL *string
	UIPassword *string
	ProxyToken *string
	Username   *string
	Password   *string
	Enabled    *bool
}

// RelayStationView is deliberately credential-free.
type RelayStationView struct {
	ID          string           `json:"id"`
	Type        RelayStationType `json:"type"`
	Name        string           `json:"name"`
	BaseURL     string           `json:"base_url"`
	ControlURL  string           `json:"control_url"`
	Enabled     bool             `json:"enabled"`
	Credentials RelayCredentials `json:"credentials"`
	Balance     *float64         `json:"balance,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// RelayCredentials reports only whether a secret is configured.
type RelayCredentials struct {
	UIPassword bool `json:"ui_password"`
	ProxyToken bool `json:"proxy_token"`
	Username   bool `json:"username"`
	Password   bool `json:"password"`
}

type relayStation struct {
	ID         string           `json:"id"`
	Type       RelayStationType `json:"type"`
	Name       string           `json:"name"`
	BaseURL    string           `json:"base_url"`
	ControlURL string           `json:"control_url"`

	UIPassword string `json:"ui_password"`
	ProxyToken string `json:"proxy_token"`
	Username   string `json:"username"`
	Password   string `json:"password"`

	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s relayStation) view() RelayStationView {
	return RelayStationView{
		ID:         s.ID,
		Type:       s.Type,
		Name:       s.Name,
		BaseURL:    s.BaseURL,
		ControlURL: s.ControlURL,
		Enabled:    s.Enabled,
		Credentials: RelayCredentials{
			UIPassword: strings.TrimSpace(s.UIPassword) != "",
			ProxyToken: strings.TrimSpace(s.ProxyToken) != "",
			Username:   strings.TrimSpace(s.Username) != "",
			Password:   strings.TrimSpace(s.Password) != "",
		},
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
}

type relayStationConfig struct {
	Stations []relayStation      `json:"stations"`
	Bindings []RelayGroupBinding `json:"bindings"`
}

// RelayStationRate is the raw station rate before the binding delta is applied.
type RelayStationRate struct {
	Rate           *float64 `json:"rate"`
	Status         string   `json:"status"`
	SupportedModels []string `json:"supported_models,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type relayRateCache struct {
	UpdatedAt time.Time                              `json:"updated_at"`
	Rates     map[string]map[string]RelayStationRate `json:"rates"`
}

// RelayRateView is the administrator-facing rate cache view.
type RelayRateView struct {
	StationID     string    `json:"station_id"`
	StationName   string    `json:"station_name"`
	SourceGroup   string    `json:"source_group"`
	Status        string    `json:"status"`
	Rate          *float64  `json:"rate"`
	EffectiveRate *float64  `json:"effective_rate,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RelayAccountView exposes a relay binding in Account Management without
// pretending its credentials are a native sub2api account.
type RelayAccountView struct {
	StationID     string           `json:"station_id"`
	StationName   string           `json:"station_name"`
	StationType   RelayStationType `json:"station_type"`
	GroupID       int64            `json:"group_id"`
	GroupName     string           `json:"group_name"`
	SourceGroup   string           `json:"source_group"`
	Enabled       bool             `json:"enabled"`
	Priority      int              `json:"priority"`
	RateStatus    string           `json:"rate_status"`
	EffectiveRate *float64         `json:"effective_rate,omitempty"`
	Balance       *float64         `json:"balance,omitempty"`
}

type RelayAccountUpdateInput struct {
	Enabled  *bool
	Priority *int
}

// RelayStationGroup is an upstream group that can be selected in a binding.
type RelayStationGroup struct {
	Name string `json:"name"`
}

// RelayProfitEstimate is a current-price estimate, not historical source attribution.
type RelayProfitEstimate struct {
	GroupID          int64    `json:"group_id"`
	GroupName        string   `json:"group_name"`
	StationID        string   `json:"station_id"`
	StationName      string   `json:"station_name"`
	SourceGroup      string   `json:"source_group"`
	RateStatus       string   `json:"rate_status"`
	Requests         int64    `json:"requests"`
	TotalCost        float64  `json:"total_cost"`
	DownstreamRate   float64  `json:"downstream_rate"`
	UpstreamRate     *float64 `json:"upstream_rate,omitempty"`
	EstimatedRevenue *float64 `json:"estimated_revenue,omitempty"`
	EstimatedCost    *float64 `json:"estimated_cost,omitempty"`
	EstimatedProfit  *float64 `json:"estimated_profit,omitempty"`
}

type relayRouteCacheEntry struct {
	StationID   string
	SourceGroup string
	ExpiresAt   time.Time
	Revision    uint64
}

// RelayRoute is opaque to callers outside the relay package surface. The
// unexported station field prevents credentials from escaping as JSON.
type RelayRoute struct {
	station       relayStation
	source        RelayStationSource
	effectiveRate float64
}

func (r *RelayRoute) StationID() string {
	if r == nil {
		return ""
	}
	return r.station.ID
}

func (r *RelayRoute) SourceGroup() string {
	if r == nil {
		return ""
	}
	return r.source.SourceGroup
}

func (r *RelayRoute) EffectiveRate() float64 {
	if r == nil {
		return 0
	}
	return r.effectiveRate
}

// RelayStationService owns relay configuration, price snapshots, sticky route
// selection, and the upstream adapter calls. Configuration and rate snapshots
// are JSON settings; session affinity is intentionally process-local so a
// request does not write the database.
type RelayStationService struct {
	settingRepo SettingRepository
	groupRepo   GroupRepository
	accountRepo AccountRepository
	usage       *UsageService
	cfg         *config.Config

	loadMu sync.Mutex
	mu     sync.RWMutex
	loaded bool

	// Each AIHub station owns a distinct aihub-auto instance. The mutex only
	// serializes its initial login; proxy requests stay fully concurrent.
	aihubMu             sync.Mutex
	activeAIHubStations map[string]struct{}

	config relayStationConfig
	rates  relayRateCache

	revision   uint64
	routes     map[string]relayRouteCacheEntry
	sessions   map[string]*relayStationSession
	started    atomic.Bool
	stopOnce   sync.Once
	stopSignal chan struct{}
}

func NewRelayStationService(settingRepo SettingRepository, groupRepo GroupRepository, accountRepo AccountRepository, usage *UsageService, cfg *config.Config) *RelayStationService {
	return &RelayStationService{
		settingRepo: settingRepo,
		groupRepo:   groupRepo,
		accountRepo: accountRepo,
		usage:       usage,
		cfg:         cfg,
		routes:             make(map[string]relayRouteCacheEntry),
		sessions:           make(map[string]*relayStationSession),
		activeAIHubStations: make(map[string]struct{}),
		stopSignal:         make(chan struct{}),
	}
}

const (
	relayAccountMarkerKey = "relay_account"
	relayAccountKeyKey    = "relay_account_key"
	relayStationIDKey     = "relay_station_id"
	relayGroupIDKey       = "relay_group_id"
	relaySourceGroupKey   = "relay_source_group"
)

// SyncNativeRelayAccounts creates the native Account identities represented by
// relay bindings. The station settings remain the transport source of truth;
// account extra metadata gives native scheduling and admin APIs a stable key.
func (s *RelayStationService) SyncNativeRelayAccounts(ctx context.Context) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.mu.RLock()
	stations := append([]relayStation(nil), s.config.Stations...)
	bindings := cloneRelayBindings(s.config.Bindings)
	s.mu.RUnlock()
	byID := make(map[string]relayStation, len(stations))
	for _, station := range stations {
		byID[station.ID] = station
	}
	existing, err := s.accountRepo.FindByExtraField(ctx, relayAccountMarkerKey, true)
	if err != nil {
		return err
	}
	existingByKey := make(map[string][]Account, len(existing))
	desired := make(map[string]bool)
	for _, binding := range bindings {
		for _, source := range binding.Sources {
			station, ok := byID[source.StationID]
			if !ok {
				continue
			}
			key := relayAccountKey(station.ID, binding.GroupID, source.SourceGroup)
			desired[key] = source.Enabled && station.Enabled
		}
	}
	for _, account := range existing {
		key := account.GetExtraString(relayAccountKeyKey)
		existingByKey[key] = append(existingByKey[key], account)
	}
	for _, binding := range bindings {
		for _, source := range binding.Sources {
			station, ok := byID[source.StationID]
			if !ok {
				continue
			}
			key := relayAccountKey(station.ID, binding.GroupID, source.SourceGroup)
			for _, account := range existingByKey[key] {
				if !source.Enabled || !station.Enabled {
					if err := s.accountRepo.SetSchedulable(ctx, account.ID, false); err != nil {
						return err
					}
				}
			}
			if len(existingByKey[key]) > 0 {
				continue
			}
			account := &Account{
				Name:          fmt.Sprintf("%s / %s", station.Name, source.SourceGroup),
				Platform:      PlatformOpenAI,
				Type:          "relay",
				Credentials:   map[string]any{},
				Extra:         map[string]any{relayAccountMarkerKey: true, relayAccountKeyKey: key, relayStationIDKey: station.ID, relayGroupIDKey: binding.GroupID, relaySourceGroupKey: source.SourceGroup},
				Concurrency:   3,
				Priority:      -source.Priority,
				Status:        StatusActive,
				Schedulable:   source.Enabled && station.Enabled,
				RateMultiplier: func() *float64 { value := 1.0; return &value }(),
			}
			if err := s.accountRepo.Create(ctx, account); err != nil {
				return err
			}
			if err := s.accountRepo.BindGroups(ctx, account.ID, []int64{binding.GroupID}); err != nil {
				return err
			}
		}
	}
	for key, accounts := range existingByKey {
		if desired[key] {
			continue
		}
		for _, account := range accounts {
			if err := s.accountRepo.SetSchedulable(ctx, account.ID, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func relayAccountKey(stationID string, groupID int64, sourceGroup string) string {
	return fmt.Sprintf("%s:%d:%s", stationID, groupID, sourceGroup)
}

// Start begins best-effort periodic rate polling and aihub configuration sync.
func (s *RelayStationService) Start() {
	if s == nil || !s.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		s.refreshInBackground()
		ticker := time.NewTicker(relayStationRatePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.refreshInBackground()
			case <-s.stopSignal:
				return
			}
		}
	}()
}

func (s *RelayStationService) refreshInBackground() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.RefreshRates(ctx)
	_ = s.SyncAIHubConfig(ctx)
}

func (s *RelayStationService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopSignal) })
}

func (s *RelayStationService) ensureLoaded(ctx context.Context) error {
	if s == nil || s.settingRepo == nil {
		return infraerrors.New(http.StatusInternalServerError, "RELAY_NOT_CONFIGURED", "relay station service is unavailable")
	}
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}

	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.RLock()
	loaded = s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}

	configSnapshot := relayStationConfig{}
	if raw, err := s.settingRepo.GetValue(ctx, SettingKeyRelayStationConfig); err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			return fmt.Errorf("load relay station configuration: %w", err)
		}
	} else if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &configSnapshot); err != nil {
			return infraerrors.New(http.StatusInternalServerError, "RELAY_CONFIG_INVALID", "relay station configuration is invalid")
		}
	}
	if err := s.validateConfig(&configSnapshot); err != nil {
		return err
	}

	rateSnapshot := relayRateCache{Rates: make(map[string]map[string]RelayStationRate)}
	if raw, err := s.settingRepo.GetValue(ctx, SettingKeyRelayStationRateCache); err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			return fmt.Errorf("load relay rate cache: %w", err)
		}
	} else if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &rateSnapshot); err != nil {
			return infraerrors.New(http.StatusInternalServerError, "RELAY_RATE_CACHE_INVALID", "relay rate cache is invalid")
		}
	}
	if rateSnapshot.Rates == nil {
		rateSnapshot.Rates = make(map[string]map[string]RelayStationRate)
	}

	s.mu.Lock()
	s.config = configSnapshot
	s.rates = rateSnapshot
	s.loaded = true
	s.mu.Unlock()
	return nil
}

func (s *RelayStationService) ListStations(ctx context.Context) ([]RelayStationView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	stations := append([]relayStation(nil), s.config.Stations...)
	s.mu.RUnlock()

	result := make([]RelayStationView, 0, len(stations))
	for _, station := range stations {
		result = append(result, s.stationView(ctx, station))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *RelayStationService) GetStation(ctx context.Context, id string) (*RelayStationView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	station, ok := findRelayStation(s.config.Stations, id)
	s.mu.RUnlock()
	if !ok {
		return nil, ErrRelayStationNotFound
	}
	view := s.stationView(ctx, station)
	return &view, nil
}

func (s *RelayStationService) stationView(ctx context.Context, station relayStation) RelayStationView {
	view := station.view()
	if station.Type != RelayStationTypeAIHub || station.Username == "" || station.Password == "" {
		return view
	}
	if balance, err := s.fetchAIHubBalance(ctx, station); err == nil {
		view.Balance = balance
	}
	return view
}

func (s *RelayStationService) CreateStation(ctx context.Context, input RelayStationCreateInput) (*RelayStationView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	station := relayStation{
		ID:         uuid.NewString(),
		Type:       input.Type,
		Name:       strings.TrimSpace(input.Name),
		BaseURL:    strings.TrimSpace(input.BaseURL),
		ControlURL: strings.TrimSpace(input.ControlURL),
		UIPassword: strings.TrimSpace(input.UIPassword),
		ProxyToken: strings.TrimSpace(input.ProxyToken),
		Username:   strings.TrimSpace(input.Username),
		Password:   strings.TrimSpace(input.Password),
		Enabled:    enabled,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if station.Type == RelayStationTypeAIHub {
		if station.Username == "" || station.Password == "" {
			return nil, infraerrors.BadRequest("RELAY_AIHUB_ACCOUNT_REQUIRED", "aihub station requires email and password")
		}
		if station.BaseURL == "" {
			station.BaseURL = managedAIHubRouterURL
		}
	} else if station.Type == RelayStationTypeNewAPI || station.Type == RelayStationTypeSub2API {
		if station.ControlURL == "" {
			station.ControlURL = station.BaseURL
		}
		if station.BaseURL == "" || station.ControlURL == "" {
			return nil, infraerrors.BadRequest("RELAY_ENDPOINTS_REQUIRED", "newapi and sub2api stations require base_url and control_url")
		}
	}
	if station.ControlURL == "" {
		station.ControlURL = station.BaseURL
	}
	if err := s.validateStation(&station); err != nil {
		return nil, err
	}

	s.mu.Lock()
	candidate := cloneRelayConfig(s.config)
	candidate.Stations = append(candidate.Stations, station)
	if err := s.validateConfig(&candidate); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.persistConfigLocked(ctx, candidate); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.config = candidate
	s.clearRoutesLocked()
	s.mu.Unlock()
	if err := s.SyncNativeRelayAccounts(ctx); err != nil {
		return nil, err
	}

	view := station.view()
	return &view, nil
}

func (s *RelayStationService) UpdateStation(ctx context.Context, id string, input RelayStationUpdateInput) (*RelayStationView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	candidate := cloneRelayConfig(s.config)
	index := relayStationIndex(candidate.Stations, id)
	if index < 0 {
		s.mu.Unlock()
		return nil, ErrRelayStationNotFound
	}
	station := &candidate.Stations[index]
	if input.Name != nil {
		station.Name = strings.TrimSpace(*input.Name)
	}
	if input.BaseURL != nil {
		station.BaseURL = strings.TrimSpace(*input.BaseURL)
	}
	if input.ControlURL != nil {
		station.ControlURL = strings.TrimSpace(*input.ControlURL)
		if station.ControlURL == "" {
			station.ControlURL = station.BaseURL
		}
	}
	if input.UIPassword != nil {
		station.UIPassword = strings.TrimSpace(*input.UIPassword)
	}
	if input.ProxyToken != nil {
		station.ProxyToken = strings.TrimSpace(*input.ProxyToken)
	}
	if input.Username != nil {
		station.Username = strings.TrimSpace(*input.Username)
	}
	if input.Password != nil {
		station.Password = strings.TrimSpace(*input.Password)
	}
	if input.Enabled != nil {
		station.Enabled = *input.Enabled
	}
	station.UpdatedAt = time.Now().UTC()
	if err := s.validateConfig(&candidate); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.persistConfigLocked(ctx, candidate); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.config = candidate
	s.clearRoutesLocked()
	delete(s.sessions, id)
	stationType := station.Type
	view := station.view()
	s.mu.Unlock()
	if stationType == RelayStationTypeAIHub {
		s.clearActiveAIHubStation(id)
	}
	if err := s.SyncNativeRelayAccounts(ctx); err != nil {
		return nil, err
	}
	return &view, nil
}

func (s *RelayStationService) DeleteStation(ctx context.Context, id string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	candidate := cloneRelayConfig(s.config)
	index := relayStationIndex(candidate.Stations, id)
	if index < 0 {
		s.mu.Unlock()
		return ErrRelayStationNotFound
	}
	stationType := candidate.Stations[index].Type
	candidate.Stations = append(candidate.Stations[:index], candidate.Stations[index+1:]...)
	candidate.Bindings = removeRelayStationFromBindings(candidate.Bindings, id)
	if err := s.validateConfig(&candidate); err != nil {
		s.mu.Unlock()
		return err
	}
	candidateRates := cloneRelayRates(s.rates)
	delete(candidateRates.Rates, id)
	candidateRates.UpdatedAt = time.Now().UTC()
	if err := s.persistAllLocked(ctx, candidate, candidateRates); err != nil {
		s.mu.Unlock()
		return err
	}
	s.config = candidate
	s.rates = candidateRates
	delete(s.sessions, id)
	s.clearRoutesLocked()
	s.mu.Unlock()
	if stationType == RelayStationTypeAIHub {
		s.clearActiveAIHubStation(id)
	}
	return nil
}

func (s *RelayStationService) ListRelayAccounts(ctx context.Context) ([]RelayAccountView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	configSnapshot := s.snapshotConfig()
	rateSnapshot := s.snapshotRates()
	stations := relayStationMap(configSnapshot.Stations)
	result := make([]RelayAccountView, 0)
	for _, binding := range configSnapshot.Bindings {
		groupName := fmt.Sprintf("#%d", binding.GroupID)
		if s.groupRepo != nil {
			if group, err := s.groupRepo.GetByID(ctx, binding.GroupID); err == nil && group != nil {
				groupName = group.Name
			}
		}
		for _, source := range binding.Sources {
			station, ok := stations[source.StationID]
			if !ok {
				continue
			}
			rate := rateSnapshot.Rates[source.StationID][source.SourceGroup]
			account := RelayAccountView{
				StationID:   station.ID,
				StationName: station.Name,
				StationType: station.Type,
				GroupID:     binding.GroupID,
				GroupName:   groupName,
				SourceGroup: source.SourceGroup,
				Enabled:     station.Enabled && source.Enabled,
				Priority:    source.Priority,
				RateStatus:  rate.Status,
			}
			if effective, ok := relayEffectiveRate(rate, source); ok {
				account.EffectiveRate = &effective
			} else if rateReady(rate) {
				account.RateStatus = RelayRateStatusUnavailable
			}
			if station.Type == RelayStationTypeAIHub {
				if balance, err := s.fetchAIHubBalance(ctx, station); err == nil {
					account.Balance = balance
				}
			}
			result = append(result, account)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].GroupID == result[j].GroupID {
			if result[i].Priority == result[j].Priority {
				return result[i].StationName < result[j].StationName
			}
			return result[i].Priority > result[j].Priority
		}
		return result[i].GroupName < result[j].GroupName
	})
	return result, nil
}

func (s *RelayStationService) UpdateRelayAccount(ctx context.Context, stationID string, groupID int64, sourceGroup string, input RelayAccountUpdateInput) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	stationID = strings.TrimSpace(stationID)
	if groupID <= 0 || stationID == "" {
		return infraerrors.BadRequest("RELAY_ACCOUNT_INVALID", "relay account station_id and group_id are required")
	}
	if input.Enabled == nil && input.Priority == nil {
		return infraerrors.BadRequest("RELAY_ACCOUNT_UPDATE_EMPTY", "relay account update is empty")
	}

	s.mu.Lock()
	candidate := cloneRelayConfig(s.config)
	bindingIndex := -1
	for index := range candidate.Bindings {
		if candidate.Bindings[index].GroupID == groupID {
			bindingIndex = index
			break
		}
	}
	if bindingIndex < 0 {
		s.mu.Unlock()
		return infraerrors.New(http.StatusNotFound, "RELAY_ACCOUNT_NOT_FOUND", "relay account was not found")
	}
	sourceGroup = normalizeRelaySourceGroup(sourceGroup)
	for index := range candidate.Bindings[bindingIndex].Sources {
		source := &candidate.Bindings[bindingIndex].Sources[index]
		if source.StationID != stationID || source.SourceGroup != sourceGroup {
			continue
		}
		if input.Enabled != nil {
			source.Enabled = *input.Enabled
		}
		if input.Priority != nil {
			source.Priority = *input.Priority
		}
		if err := s.validateConfig(&candidate); err != nil {
			s.mu.Unlock()
			return err
		}
		if err := s.persistConfigLocked(ctx, candidate); err != nil {
			s.mu.Unlock()
			return err
		}
		s.config = candidate
		s.clearRoutesLocked()
		s.mu.Unlock()
		s.clearActiveAIHubStation(stationID)
		return nil
	}
	s.mu.Unlock()
	return infraerrors.New(http.StatusNotFound, "RELAY_ACCOUNT_NOT_FOUND", "relay account was not found")
}

func (s *RelayStationService) ListBindings(ctx context.Context) ([]RelayGroupBinding, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	bindings := cloneRelayBindings(s.config.Bindings)
	s.mu.RUnlock()
	return bindings, nil
}

func (s *RelayStationService) UpdateBindings(ctx context.Context, bindings []RelayGroupBinding) ([]RelayGroupBinding, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	candidate := cloneRelayConfig(s.config)
	candidate.Bindings = cloneRelayBindings(bindings)
	if err := s.validateConfig(&candidate); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.persistConfigLocked(ctx, candidate); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.config = candidate
	s.clearRoutesLocked()
	result := cloneRelayBindings(candidate.Bindings)
	s.mu.Unlock()
	s.clearActiveAIHubStation("")
	if err := s.SyncNativeRelayAccounts(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// RefreshRates polls all enabled stations and persists only credential-free rate snapshots.
func (s *RelayStationService) RefreshRates(ctx context.Context) error {
	return s.refreshRates(ctx, "")
}

// TestStation forces an upstream request and returns the current snapshots.
func (s *RelayStationService) TestStation(ctx context.Context, id string) ([]RelayRateView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	snapshot := s.snapshotConfig()
	station, found := findRelayStation(snapshot.Stations, id)
	if !found {
		return nil, ErrRelayStationNotFound
	}
	if len(relayRequiredSourceGroups(snapshot, id)[id]) == 0 {
		if _, err := s.fetchStationRates(ctx, station, map[string]struct{}{"default": {}}); err != nil {
			return nil, infraerrors.New(http.StatusBadGateway, "RELAY_TEST_FAILED", err.Error())
		}
	}
	if err := s.refreshRates(ctx, id); err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "RELAY_TEST_FAILED", err.Error())
	}
	return s.ListRatesForStation(ctx, id)
}

func (s *RelayStationService) refreshRates(ctx context.Context, onlyStationID string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	snapshot := s.snapshotConfig()
	requiredGroups := relayRequiredSourceGroups(snapshot, onlyStationID)
	if onlyStationID != "" {
		if _, ok := findRelayStation(snapshot.Stations, onlyStationID); !ok {
			return ErrRelayStationNotFound
		}
	}
	if len(requiredGroups) == 0 {
		return nil
	}

	now := time.Now().UTC()
	updates := make(map[string]map[string]RelayStationRate, len(requiredGroups))
	var pollErrors []error
	for stationID, sourceGroups := range requiredGroups {
		station, ok := findRelayStation(snapshot.Stations, stationID)
		if !ok || !station.Enabled {
			continue
		}
		rates, err := s.fetchStationRates(ctx, station, sourceGroups)
		if err != nil {
			pollErrors = append(pollErrors, fmt.Errorf("%s: %w", station.Name, err))
			rates = make(map[string]RelayStationRate, len(sourceGroups))
		}
		for sourceGroup := range sourceGroups {
			rate, exists := rates[sourceGroup]
			if !exists {
				rate = RelayStationRate{Status: RelayRateStatusUnavailable}
			}
			if rate.Status == "" {
				rate.Status = RelayRateStatusUnavailable
			}
			rate.UpdatedAt = now
			rates[sourceGroup] = rate
		}
		updates[stationID] = rates
	}

	s.mu.Lock()
	candidateRates := cloneRelayRates(s.rates)
	if candidateRates.Rates == nil {
		candidateRates.Rates = make(map[string]map[string]RelayStationRate)
	}
	for stationID, rates := range updates {
		if candidateRates.Rates[stationID] == nil {
			candidateRates.Rates[stationID] = make(map[string]RelayStationRate)
		}
		for sourceGroup, rate := range rates {
			candidateRates.Rates[stationID][sourceGroup] = rate
		}
	}
	candidateRates.UpdatedAt = now
	persistErr := s.persistRatesLocked(ctx, candidateRates)
	if persistErr == nil {
		s.rates = candidateRates
	}
	s.mu.Unlock()
	if persistErr != nil {
		return persistErr
	}
	if err := s.syncNativeRelayRates(ctx, snapshot, candidateRates); err != nil {
		return err
	}
	return errors.Join(pollErrors...)
}

func (s *RelayStationService) syncNativeRelayRates(ctx context.Context, snapshot relayStationConfig, rates relayRateCache) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	stations := relayStationMap(snapshot.Stations)
	for _, binding := range snapshot.Bindings {
		for _, source := range binding.Sources {
			station, stationFound := stations[source.StationID]
			if !stationFound {
				continue
			}
			key := relayAccountKey(source.StationID, binding.GroupID, source.SourceGroup)
			accounts, err := s.accountRepo.FindByExtraField(ctx, relayAccountKeyKey, key)
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				continue
			}
			rate := rates.Rates[source.StationID][source.SourceGroup]
			updates := map[string]any{
				"relay_rate_updated_at":          rate.UpdatedAt.Format(time.RFC3339Nano),
				"relay_effective_rate":            nil,
				"relay_station_type":             string(station.Type),
				"relay_model_capability_known":   station.Type == RelayStationTypeAIHub && rate.SupportedModels != nil,
				"relay_supported_models":         rate.SupportedModels,
			}
			if effective, ok := relayEffectiveRate(rate, source); ok {
				updates["relay_effective_rate"] = effective
			}
			for _, account := range accounts {
				if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *RelayStationService) ListRates(ctx context.Context) ([]RelayRateView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	return s.listRates(""), nil
}

func (s *RelayStationService) ListRatesForStation(ctx context.Context, stationID string) ([]RelayRateView, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	_, found := findRelayStation(s.config.Stations, stationID)
	s.mu.RUnlock()
	if !found {
		return nil, ErrRelayStationNotFound
	}
	return s.listRates(stationID), nil
}

// ListGroups returns the currently available upstream groups for a station.
// AIHub uses policy keys rather than a user-selectable upstream group.
func (s *RelayStationService) ListGroups(ctx context.Context, stationID string) ([]RelayStationGroup, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	station, found := findRelayStation(s.snapshotConfig().Stations, stationID)
	if !found {
		return nil, ErrRelayStationNotFound
	}
	if station.Type == RelayStationTypeAIHub {
		return nil, infraerrors.BadRequest("RELAY_GROUP_LIST_UNSUPPORTED", "aihub stations select groups automatically")
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		session, err := s.relaySession(ctx, station)
		if err != nil {
			return nil, err
		}
		groups := make(map[string]float64)
		var status int
		switch station.Type {
		case RelayStationTypeNewAPI:
			groups, status, err = s.requestNewAPIRates(ctx, station, session)
		case RelayStationTypeSub2API:
			groups, status, err = s.requestSub2APIRates(ctx, station, session.token)
		default:
			return nil, errors.New("unsupported relay station type")
		}
		if err == nil {
			result := make([]RelayStationGroup, 0, len(groups))
			for name := range groups {
				result = append(result, RelayStationGroup{Name: name})
			}
			sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
			return result, nil
		}
		lastErr = err
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			break
		}
		s.clearRelaySession(station.ID)
	}
	return nil, lastErr
}

func (s *RelayStationService) listRates(onlyStationID string) []RelayRateView {
	s.mu.RLock()
	configSnapshot := cloneRelayConfig(s.config)
	rateSnapshot := cloneRelayRates(s.rates)
	s.mu.RUnlock()

	stationByID := relayStationMap(configSnapshot.Stations)
	seen := make(map[string]struct{})
	result := make([]RelayRateView, 0)
	for _, binding := range configSnapshot.Bindings {
		for _, source := range binding.Sources {
			if onlyStationID != "" && source.StationID != onlyStationID {
				continue
			}
			key := source.StationID + "\x00" + source.SourceGroup
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			station, ok := stationByID[source.StationID]
			if !ok {
				continue
			}
			rate := rateSnapshot.Rates[source.StationID][source.SourceGroup]
			view := RelayRateView{
				StationID:   station.ID,
				StationName: station.Name,
				SourceGroup: source.SourceGroup,
				Status:      rate.Status,
				Rate:        cloneFloat64(rate.Rate),
				UpdatedAt:   rate.UpdatedAt,
			}
			if effective, ok := relayEffectiveRate(rate, source); ok {
				view.EffectiveRate = &effective
			}
			result = append(result, view)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].StationName == result[j].StationName {
			return result[i].SourceGroup < result[j].SourceGroup
		}
		return result[i].StationName < result[j].StationName
	})
	return result
}

// ResolveRouteForAccount resolves the exact relay source represented by a native
// relay account after the native scheduler has selected it.
func (s *RelayStationService) ResolveRouteForAccount(ctx context.Context, account *Account, groupID int64) (*RelayRoute, error) {
	if account == nil || !account.IsRelay() || groupID <= 0 {
		return nil, ErrRelayRouteNotFound
	}
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	configSnapshot := s.snapshotConfig()
	station, found := findRelayStation(configSnapshot.Stations, account.RelayStationID())
	if !found {
		return nil, ErrRelayRouteNotFound
	}
	binding, found := findRelayBinding(configSnapshot.Bindings, groupID)
	if !found {
		return nil, ErrRelayRouteNotFound
	}
	for _, source := range binding.Sources {
		if source.StationID != station.ID || source.SourceGroup != account.RelaySourceGroup() || !source.Enabled || !station.Enabled {
			continue
		}
		rate := s.snapshotRates().Rates[station.ID][source.SourceGroup]
		if !rateReadyForRoute(rate, time.Now()) {
			_ = s.RefreshRates(ctx)
			rate = s.snapshotRates().Rates[station.ID][source.SourceGroup]
		}
		if !rateReadyForRoute(rate, time.Now()) {
			return nil, ErrRelayRateUnavailable
		}
		effectiveRate, ok := relayEffectiveRate(rate, source)
		if !ok {
			return nil, ErrRelayRateUnavailable
		}
		return &RelayRoute{station: station, source: source, effectiveRate: effectiveRate}, nil
	}
	return nil, ErrRelayRouteNotFound
}

// ResolveRoute returns the cheapest currently-ready source for a new affinity
// key. Existing affinity keys keep their source until relayStationRouteTTL.
func (s *RelayStationService) ResolveRoute(ctx context.Context, groupID int64, affinityKey string) (*RelayRoute, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	if groupID <= 0 {
		return nil, ErrRelayRouteNotFound
	}

	configSnapshot := s.snapshotConfig()
	binding, found := findRelayBinding(configSnapshot.Bindings, groupID)
	if !found || len(binding.Sources) == 0 {
		return nil, ErrRelayRouteNotFound
	}

	cacheKey := ""
	if affinityKey != "" {
		cacheKey = fmt.Sprintf("%d:%s", groupID, affinityKey)
		if route := s.cachedRoute(configSnapshot, cacheKey); route != nil {
			return route, nil
		}
	}

	now := time.Now()
	if !s.hasReadyRate(binding, now) {
		// A new request should not choose from a stale startup cache. One bounded
		// refresh is enough; background polling handles the steady state.
		_ = s.RefreshRates(ctx)
	}

	rateSnapshot := s.snapshotRates()
	now = time.Now()
	stationByID := relayStationMap(configSnapshot.Stations)
	var selected *RelayRoute
	for _, source := range binding.Sources {
		if !source.Enabled {
			continue
		}
		station, ok := stationByID[source.StationID]
		if !ok || !station.Enabled || (station.Type == RelayStationTypeAIHub && strings.TrimSpace(station.BaseURL) == "") {
			continue
		}
		rate := rateSnapshot.Rates[source.StationID][source.SourceGroup]
		if !rateReadyForRoute(rate, now) {
			continue
		}
		effectiveRate, ok := relayEffectiveRate(rate, source)
		if !ok {
			continue
		}
		candidate := &RelayRoute{station: station, source: source, effectiveRate: effectiveRate}
		if selected == nil || candidate.source.Priority > selected.source.Priority ||
			(candidate.source.Priority == selected.source.Priority && (candidate.effectiveRate < selected.effectiveRate ||
				(candidate.effectiveRate == selected.effectiveRate && candidate.station.ID < selected.station.ID))) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, ErrRelayRateUnavailable
	}

	if cacheKey != "" {
		s.mu.Lock()
		s.routes[cacheKey] = relayRouteCacheEntry{
			StationID:   selected.station.ID,
			SourceGroup: selected.source.SourceGroup,
			ExpiresAt:   time.Now().Add(relayStationRouteTTL),
			Revision:    s.revision,
		}
		s.mu.Unlock()
	}
	return selected, nil
}

// EstimateProfit calculates current-price scenarios from UsageLog group totals.
func (s *RelayStationService) EstimateProfit(ctx context.Context, start, end time.Time) ([]RelayProfitEstimate, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	if s.usage == nil || s.groupRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "RELAY_PROFIT_UNAVAILABLE", "relay profit estimation is unavailable")
	}
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return nil, infraerrors.BadRequest("RELAY_PROFIT_RANGE_INVALID", "start and end must form a valid time range")
	}

	stats, err := s.usage.GetGroupStatsWithFilters(ctx, start, end, usagestats.UsageLogFilters{})
	if err != nil {
		return nil, err
	}
	statsByGroup := make(map[int64]usagestats.GroupStat, len(stats))
	for _, stat := range stats {
		statsByGroup[stat.GroupID] = stat
	}

	configSnapshot := s.snapshotConfig()
	rateSnapshot := s.snapshotRates()
	stationByID := relayStationMap(configSnapshot.Stations)
	result := make([]RelayProfitEstimate, 0)
	for _, binding := range configSnapshot.Bindings {
		group, groupErr := s.groupRepo.GetByID(ctx, binding.GroupID)
		if groupErr != nil || group == nil {
			continue
		}
		stat := statsByGroup[binding.GroupID]
		for _, source := range binding.Sources {
			if !source.Enabled {
				continue
			}
			station, ok := stationByID[source.StationID]
			if !ok || !station.Enabled {
				continue
			}
			rate := rateSnapshot.Rates[source.StationID][source.SourceGroup]
			estimate := RelayProfitEstimate{
				GroupID:        group.ID,
				GroupName:      group.Name,
				StationID:      station.ID,
				StationName:    station.Name,
				SourceGroup:    source.SourceGroup,
				RateStatus:     rate.Status,
				Requests:       stat.Requests,
				TotalCost:      stat.Cost,
				DownstreamRate: group.RateMultiplier,
			}
			if upstreamRate, ok := relayEffectiveRate(rate, source); ok {
					revenue := stat.Cost * group.RateMultiplier
					cost := stat.Cost * upstreamRate
					profit := revenue - cost
					estimate.UpstreamRate = &upstreamRate
					estimate.EstimatedRevenue = &revenue
					estimate.EstimatedCost = &cost
					estimate.EstimatedProfit = &profit
				}
			result = append(result, estimate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].GroupID == result[j].GroupID {
			return result[i].StationName < result[j].StationName
		}
		return result[i].GroupID < result[j].GroupID
	})
	return result, nil
}

func (s *RelayStationService) snapshotConfig() relayStationConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRelayConfig(s.config)
}

func (s *RelayStationService) snapshotRates() relayRateCache {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneRelayRates(s.rates)
}

func (s *RelayStationService) cachedRoute(configSnapshot relayStationConfig, key string) *RelayRoute {
	s.mu.RLock()
	entry, ok := s.routes[key]
	revision := s.revision
	s.mu.RUnlock()
	if !ok || entry.Revision != revision || !entry.ExpiresAt.After(time.Now()) {
		return nil
	}
	binding, ok := findRelayBinding(configSnapshot.Bindings, parseRelayGroupID(key))
	if !ok {
		return nil
	}
	station, stationOK := findRelayStation(configSnapshot.Stations, entry.StationID)
	source, sourceOK := findRelaySource(binding.Sources, entry.StationID, entry.SourceGroup)
	if !stationOK || !sourceOK || !station.Enabled || !source.Enabled {
		return nil
	}
	return &RelayRoute{station: station, source: source}
}

func (s *RelayStationService) hasReadyRate(binding RelayGroupBinding, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, source := range binding.Sources {
		if source.Enabled && rateReadyForRoute(s.rates.Rates[source.StationID][source.SourceGroup], now) {
			return true
		}
	}
	return false
}

func (s *RelayStationService) clearRoutesLocked() {
	s.revision++
	s.routes = make(map[string]relayRouteCacheEntry)
}

func (s *RelayStationService) persistConfigLocked(ctx context.Context, config relayStationConfig) error {
	encoded, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode relay station configuration: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyRelayStationConfig, string(encoded)); err != nil {
		return fmt.Errorf("save relay station configuration: %w", err)
	}
	return nil
}

func (s *RelayStationService) persistRatesLocked(ctx context.Context, rates relayRateCache) error {
	encoded, err := json.Marshal(rates)
	if err != nil {
		return fmt.Errorf("encode relay rate cache: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyRelayStationRateCache, string(encoded)); err != nil {
		return fmt.Errorf("save relay rate cache: %w", err)
	}
	return nil
}

func (s *RelayStationService) persistAllLocked(ctx context.Context, config relayStationConfig, rates relayRateCache) error {
	configData, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode relay station configuration: %w", err)
	}
	rateData, err := json.Marshal(rates)
	if err != nil {
		return fmt.Errorf("encode relay rate cache: %w", err)
	}
	if err := s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingKeyRelayStationConfig:    string(configData),
		SettingKeyRelayStationRateCache: string(rateData),
	}); err != nil {
		return fmt.Errorf("save relay station settings: %w", err)
	}
	return nil
}

func (s *RelayStationService) validateConfig(candidate *relayStationConfig) error {
	if candidate == nil {
		return infraerrors.BadRequest("RELAY_CONFIG_INVALID", "relay station configuration is required")
	}
	stationIDs := make(map[string]relayStation, len(candidate.Stations))
	aihubRouters := make(map[string]string, len(candidate.Stations))
	for index := range candidate.Stations {
		station := &candidate.Stations[index]
		if err := s.validateStation(station); err != nil {
			return err
		}
		if _, exists := stationIDs[station.ID]; exists {
			return infraerrors.BadRequest("RELAY_STATION_DUPLICATE", "relay station id must be unique")
		}
		if station.Type == RelayStationTypeAIHub && station.ControlURL != "" {
			router := strings.TrimRight(station.ControlURL, "/")
			if existingID, exists := aihubRouters[router]; exists {
				return infraerrors.BadRequest("RELAY_AIHUB_ROUTER_DUPLICATE", "each aihub account requires its own aihub-auto router instance: "+existingID)
			}
			aihubRouters[router] = station.ID
		}
		stationIDs[station.ID] = *station
	}

	bindingIDs := make(map[int64]struct{}, len(candidate.Bindings))
	aihubPolicies := make(map[string]relayAIHubConfig)
	filteredBindings := make([]RelayGroupBinding, 0, len(candidate.Bindings))
	for index := range candidate.Bindings {
		binding := candidate.Bindings[index]
		if binding.GroupID <= 0 {
			return infraerrors.BadRequest("RELAY_GROUP_INVALID", "relay binding group_id must be positive")
		}
		if _, exists := bindingIDs[binding.GroupID]; exists {
			return infraerrors.BadRequest("RELAY_GROUP_DUPLICATE", "relay binding group_id must be unique")
		}
		bindingIDs[binding.GroupID] = struct{}{}
		if len(binding.Sources) == 0 {
			continue
		}

		sourceIDs := make(map[string]struct{}, len(binding.Sources))
		for sourceIndex := range binding.Sources {
			source := &binding.Sources[sourceIndex]
			source.StationID = strings.TrimSpace(source.StationID)
			source.Mode = strings.TrimSpace(source.Mode)
			var poolErr error
			source.AccountPools, poolErr = normalizeRelayAccountPools(source.AccountPools)
			if poolErr != nil {
				return poolErr
			}
			if source.StationID == "" {
				return infraerrors.BadRequest("RELAY_SOURCE_INVALID", "relay source station_id is required")
			}
			station, exists := stationIDs[source.StationID]
			if !exists {
				return infraerrors.BadRequest("RELAY_STATION_UNKNOWN", "relay source references an unknown station")
			}
			if station.Type == RelayStationTypeAIHub {
				// aihub-auto selects AIHub groups internally; the relay has one source.
				source.SourceGroup = "default"
			} else {
				source.SourceGroup = normalizeRelaySourceGroup(source.SourceGroup)
			}
			if !validRelaySourceGroup(source.SourceGroup) {
				return infraerrors.BadRequest("RELAY_SOURCE_GROUP_INVALID", "relay source_group is invalid")
			}
			if source.Priority < -1_000_000 || source.Priority > 1_000_000 {
				return infraerrors.BadRequest("RELAY_PRIORITY_INVALID", "relay source priority must be between -1000000 and 1000000")
			}
			if math.IsNaN(source.Delta) || math.IsInf(source.Delta, 0) {
				return infraerrors.BadRequest("RELAY_DELTA_INVALID", "relay source delta must be finite")
			}
			if source.MaxRate != nil && (*source.MaxRate < 0 || math.IsNaN(*source.MaxRate) || math.IsInf(*source.MaxRate, 0)) {
				return infraerrors.BadRequest("RELAY_MAX_RATE_INVALID", "relay source max_rate must be a finite non-negative number")
			}
			if station.Type == RelayStationTypeAIHub && source.Mode != "" {
				switch source.Mode {
				case "economy", "balanced", "speed":
				default:
					return infraerrors.BadRequest("RELAY_MODE_INVALID", "aihub relay mode must be economy, balanced, or speed")
				}
			}
			if station.Type == RelayStationTypeAIHub && source.Enabled {
				policy := relayAIHubPolicyForSource(*source)
				if existing, found := aihubPolicies[source.StationID]; found && !sameRelayAIHubConfig(existing, policy) {
					return infraerrors.BadRequest("RELAY_AIHUB_POLICY_CONFLICT", "all bindings for an aihub account must use the same aihub-auto policy")
				}
				aihubPolicies[source.StationID] = policy
			}
			sourceKey := source.StationID + "\x00" + source.SourceGroup
			if _, exists := sourceIDs[sourceKey]; exists {
				return infraerrors.BadRequest("RELAY_SOURCE_DUPLICATE", "relay source may be bound once per station and source group")
			}
			sourceIDs[sourceKey] = struct{}{}
		}
		filteredBindings = append(filteredBindings, binding)
	}
	candidate.Bindings = filteredBindings
	return nil
}

func (s *RelayStationService) validateStation(station *relayStation) error {
	if station == nil || !station.Type.valid() {
		return infraerrors.BadRequest("RELAY_STATION_TYPE_INVALID", "relay station type must be aihub, newapi, or sub2api")
	}
	station.ID = strings.TrimSpace(station.ID)
	station.Name = strings.TrimSpace(station.Name)
	station.BaseURL = strings.TrimSpace(station.BaseURL)
	station.ControlURL = strings.TrimSpace(station.ControlURL)
	if station.Type == RelayStationTypeAIHub && station.BaseURL == "" {
		station.BaseURL = managedAIHubRouterURL
	}
	station.UIPassword = strings.TrimSpace(station.UIPassword)
	station.ProxyToken = strings.TrimSpace(station.ProxyToken)
	station.Username = strings.TrimSpace(station.Username)
	station.Password = strings.TrimSpace(station.Password)
	if station.ID == "" || station.Name == "" || len(station.Name) > 100 {
		return infraerrors.BadRequest("RELAY_STATION_INVALID", "relay station id and name are required")
	}
	if station.ControlURL == "" {
		station.ControlURL = station.BaseURL
	}
	if station.Type != RelayStationTypeAIHub || station.BaseURL != "" {
		if err := s.validateRelayURL(station.BaseURL); err != nil {
			return infraerrors.BadRequest("RELAY_BASE_URL_INVALID", "relay station base_url is invalid")
		}
		if err := s.validateRelayURL(station.ControlURL); err != nil {
			return infraerrors.BadRequest("RELAY_CONTROL_URL_INVALID", "relay station control_url is invalid")
		}
	}
	switch station.Type {
	case RelayStationTypeAIHub:
		// Legacy stations may rely on a pre-authenticated sidecar. They remain
		// editable and ResolveRoute excludes entries without a router URL.
		if station.Username == "" {
			return infraerrors.BadRequest("RELAY_AIHUB_ACCOUNT_REQUIRED", "aihub station requires an email")
		}
	case RelayStationTypeNewAPI, RelayStationTypeSub2API:
		if station.Username == "" || station.Password == "" {
			return infraerrors.BadRequest("RELAY_LOGIN_CREDENTIALS_REQUIRED", "newapi and sub2api stations require username and password")
		}
	}
	return nil
}

func (s *RelayStationService) applyAIHubConnectionDefaults(station *relayStation) {
	if station == nil || station.Type != RelayStationTypeAIHub {
		return
	}
	if station.BaseURL == "" {
		station.BaseURL = managedAIHubRouterURL
	}
	if station.ControlURL == "" {
		station.ControlURL = station.BaseURL
	}
	if station.UIPassword == "" {
		station.UIPassword = strings.TrimSpace(os.Getenv(managedAIHubUIPasswordEnv))
	}
	if station.ProxyToken == "" {
		station.ProxyToken = strings.TrimSpace(os.Getenv(managedAIHubProxyTokenEnv))
	}
}

func relayAIHubPolicyForSource(source RelayStationSource) relayAIHubConfig {
	policy := relayAIHubConfig{
		Mode:             source.Mode,
		AccountPoolPlans: append([]string(nil), source.AccountPools...),
	}
	if source.MaxRate != nil {
		min := 0.0
		policy.PriceBand = &RelayPriceBand{Min: &min, Max: cloneFloat64(source.MaxRate)}
	}
	return policy
}

func sameRelayAIHubConfig(left, right relayAIHubConfig) bool {
	if left.Mode != right.Mode || !sameRelayStringSlice(left.AccountPoolPlans, right.AccountPoolPlans) {
		return false
	}
	return sameRelayFloat64(left.PriceBand, right.PriceBand, func(band *RelayPriceBand) *float64 { return band.Min }) &&
		sameRelayFloat64(left.PriceBand, right.PriceBand, func(band *RelayPriceBand) *float64 { return band.Max })
}

func normalizeRelayAccountPools(values []string) ([]string, error) {
	allowed := map[string]struct{}{"plus": {}, "pro": {}, "team": {}}
	selected := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := allowed[value]; !ok {
			return nil, infraerrors.BadRequest("RELAY_ACCOUNT_POOL_INVALID", "aihub account_pools may contain only plus, pro, or team")
		}
		selected[value] = struct{}{}
	}
	result := make([]string, 0, len(selected))
	for _, value := range []string{"plus", "pro", "team"} {
		if _, ok := selected[value]; ok {
			result = append(result, value)
		}
	}
	return result, nil
}

func sameRelayStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameRelayFloat64(left, right *RelayPriceBand, value func(*RelayPriceBand) *float64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, rightValue := value(left), value(right)
	if leftValue == nil || rightValue == nil {
		return leftValue == nil && rightValue == nil
	}
	return *leftValue == *rightValue
}

func relayRequiredSourceGroups(config relayStationConfig, onlyStationID string) map[string]map[string]struct{} {
	result := make(map[string]map[string]struct{})
	stationByID := relayStationMap(config.Stations)
	for _, binding := range config.Bindings {
		for _, source := range binding.Sources {
			if !source.Enabled || (onlyStationID != "" && source.StationID != onlyStationID) {
				continue
			}
			station, ok := stationByID[source.StationID]
			if !ok || !station.Enabled {
				continue
			}
			if result[source.StationID] == nil {
				result[source.StationID] = make(map[string]struct{})
			}
			result[source.StationID][source.SourceGroup] = struct{}{}
		}
	}
	return result
}

func relayStationMap(stations []relayStation) map[string]relayStation {
	result := make(map[string]relayStation, len(stations))
	for _, station := range stations {
		result[station.ID] = station
	}
	return result
}

func findRelayStation(stations []relayStation, id string) (relayStation, bool) {
	for _, station := range stations {
		if station.ID == id {
			return station, true
		}
	}
	return relayStation{}, false
}

func relayStationIndex(stations []relayStation, id string) int {
	for index := range stations {
		if stations[index].ID == id {
			return index
		}
	}
	return -1
}

func findRelayBinding(bindings []RelayGroupBinding, groupID int64) (RelayGroupBinding, bool) {
	for _, binding := range bindings {
		if binding.GroupID == groupID {
			return binding, true
		}
	}
	return RelayGroupBinding{}, false
}

func findRelaySource(sources []RelayStationSource, stationID, sourceGroup string) (RelayStationSource, bool) {
	for _, source := range sources {
		if source.StationID == stationID && source.SourceGroup == sourceGroup {
			return source, true
		}
	}
	return RelayStationSource{}, false
}

func removeRelayStationFromBindings(bindings []RelayGroupBinding, stationID string) []RelayGroupBinding {
	result := make([]RelayGroupBinding, 0, len(bindings))
	for _, binding := range bindings {
		sources := make([]RelayStationSource, 0, len(binding.Sources))
		for _, source := range binding.Sources {
			if source.StationID != stationID {
				sources = append(sources, source)
			}
		}
		if len(sources) > 0 {
			binding.Sources = sources
			result = append(result, binding)
		}
	}
	return result
}

func normalizeRelaySourceGroup(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	return value
}

func validRelaySourceGroup(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func relayEffectiveRate(rate RelayStationRate, source RelayStationSource) (float64, bool) {
	if !rateReady(rate) {
		return 0, false
	}
	upstream := *rate.Rate
	if source.MaxRate != nil && upstream > *source.MaxRate {
		return 0, false
	}
	effective := upstream
	if source.AdjustRate == nil || *source.AdjustRate {
		effective += source.Delta
	}
	if source.MaxRate != nil && effective > *source.MaxRate {
		effective = *source.MaxRate
	}
	if effective < 0 || math.IsNaN(effective) || math.IsInf(effective, 0) {
		return 0, false
	}
	return effective, true
}

func rateReady(rate RelayStationRate) bool {
	return rate.Status == RelayRateStatusReady && rate.Rate != nil && *rate.Rate >= 0 &&
		!math.IsNaN(*rate.Rate) && !math.IsInf(*rate.Rate, 0)
}

func rateReadyForRoute(rate RelayStationRate, now time.Time) bool {
	return rateReady(rate) && !rate.UpdatedAt.IsZero() && rate.UpdatedAt.After(now.Add(-2*relayStationRatePollInterval))
}

func parseRelayGroupID(cacheKey string) int64 {
	var groupID int64
	_, _ = fmt.Sscanf(cacheKey, "%d:", &groupID)
	return groupID
}

func cloneRelayConfig(source relayStationConfig) relayStationConfig {
	result := relayStationConfig{
		Stations: append([]relayStation(nil), source.Stations...),
		Bindings: cloneRelayBindings(source.Bindings),
	}
	if result.Stations == nil {
		result.Stations = []relayStation{}
	}
	if result.Bindings == nil {
		result.Bindings = []RelayGroupBinding{}
	}
	return result
}

func cloneRelayBindings(source []RelayGroupBinding) []RelayGroupBinding {
	result := make([]RelayGroupBinding, 0, len(source))
	for _, binding := range source {
		clone := RelayGroupBinding{GroupID: binding.GroupID, Sources: make([]RelayStationSource, 0, len(binding.Sources))}
		for _, item := range binding.Sources {
			item.AccountPools = append([]string(nil), item.AccountPools...)
			item.AdjustRate = cloneBool(item.AdjustRate)
			clone.Sources = append(clone.Sources, item)
		}
		result = append(result, clone)
	}
	return result
}

func cloneRelayPriceBand(source *RelayPriceBand) *RelayPriceBand {
	if source == nil {
		return nil
	}
	return &RelayPriceBand{Min: cloneFloat64(source.Min), Max: cloneFloat64(source.Max)}
}

func cloneRelayRates(source relayRateCache) relayRateCache {
	result := relayRateCache{UpdatedAt: source.UpdatedAt, Rates: make(map[string]map[string]RelayStationRate, len(source.Rates))}
	for stationID, byGroup := range source.Rates {
		copyByGroup := make(map[string]RelayStationRate, len(byGroup))
		for sourceGroup, rate := range byGroup {
			rate.Rate = cloneFloat64(rate.Rate)
			copyByGroup[sourceGroup] = rate
		}
		result.Rates[stationID] = copyByGroup
	}
	return result
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
