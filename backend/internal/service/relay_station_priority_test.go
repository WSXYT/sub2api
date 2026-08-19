package service

import (
	"context"
	"testing"
	"time"
)

type relayGroupMultiplierRepo struct {
	GroupRepository
	groups  map[int64]*Group
	updates []int64
}

type relayNativeAccountRepo struct {
	AccountRepository
	accounts []Account
	updates  []Account
	bindings map[int64][]int64
}

func (r *relayNativeAccountRepo) FindByExtraField(_ context.Context, _ string, _ any) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

func (r *relayNativeAccountRepo) Update(_ context.Context, account *Account) error {
	copy := *account
	r.updates = append(r.updates, copy)
	for index := range r.accounts {
		if r.accounts[index].ID == account.ID {
			r.accounts[index] = copy
		}
	}
	return nil
}

func (r *relayNativeAccountRepo) BindGroups(_ context.Context, accountID int64, groupIDs []int64) error {
	if r.bindings == nil {
		r.bindings = make(map[int64][]int64)
	}
	r.bindings[accountID] = append([]int64(nil), groupIDs...)
	return nil
}

func (r *relayGroupMultiplierRepo) GetByID(_ context.Context, id int64) (*Group, error) {
	group, found := r.groups[id]
	if !found {
		return nil, ErrGroupNotFound
	}
	copy := *group
	return &copy, nil
}

func (r *relayGroupMultiplierRepo) Update(_ context.Context, group *Group) error {
	copy := *group
	r.groups[group.ID] = &copy
	r.updates = append(r.updates, group.ID)
	return nil
}

func TestRelayNativeAccountIdentityUsesPlatformAPIKeys(t *testing.T) {
	for _, platform := range []string{PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek} {
		gotPlatform, accountType := relayNativeAccountIdentity(&Group{Platform: platform})
		if gotPlatform != platform || accountType != AccountTypeAPIKey {
			t.Fatalf("%s relay identity = %s/%s, want %s/%s", platform, gotPlatform, accountType, platform, AccountTypeAPIKey)
		}
	}

	platform, accountType := relayNativeAccountIdentity(&Group{Platform: PlatformOpenAI})
	if platform != PlatformOpenAI || accountType != "relay" {
		t.Fatalf("openai relay identity = %s/%s, want %s/relay", platform, accountType, PlatformOpenAI)
	}
}

func TestSyncNativeRelayAccountsMigratesExistingRelayIdentity(t *testing.T) {
	for _, platform := range []string{PlatformGrok, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			const groupID int64 = 9
			const accountID int64 = 77
			key := relayAccountKey("station", groupID, "source")
			accountRepo := &relayNativeAccountRepo{accounts: []Account{{
				ID:       accountID,
				Name:     "old station / source",
				Platform: PlatformOpenAI,
				Type:     "relay",
				Extra: map[string]any{
					relayAccountMarkerKey: true,
					relayAccountKeyKey:    key,
					relayStationIDKey:     "station",
					relayGroupIDKey:       groupID,
					relaySourceGroupKey:   "source",
				},
			}}}
			groupRepo := &relayGroupMultiplierRepo{groups: map[int64]*Group{
				groupID: {ID: groupID, Platform: platform},
			}}
			service := &RelayStationService{
				accountRepo: accountRepo,
				groupRepo:   groupRepo,
				loaded:      true,
				config: relayStationConfig{
					Stations: []relayStation{{ID: "station", Name: platform + " station", Enabled: true}},
					Bindings: []RelayGroupBinding{{GroupID: groupID, Sources: []RelayStationSource{{
						StationID: "station", SourceGroup: "source", Enabled: true,
					}}}},
				},
			}

			if err := service.SyncNativeRelayAccounts(context.Background()); err != nil {
				t.Fatalf("sync relay accounts: %v", err)
			}
			if len(accountRepo.updates) != 1 {
				t.Fatalf("updates = %d, want 1", len(accountRepo.updates))
			}
			updated := accountRepo.updates[0]
			if updated.ID != accountID || updated.Platform != platform || updated.Type != AccountTypeAPIKey {
				t.Fatalf("updated relay identity = id:%d %s/%s, want id:%d %s/%s", updated.ID, updated.Platform, updated.Type, accountID, platform, AccountTypeAPIKey)
			}
			if !updated.IsRelay() || updated.GetExtraString(relayAccountKeyKey) != key {
				t.Fatalf("updated relay marker/key were not preserved: %#v", updated.Extra)
			}
			if groups := accountRepo.bindings[accountID]; len(groups) != 1 || groups[0] != groupID {
				t.Fatalf("group binding = %v, want [%d]", groups, groupID)
			}
		})
	}
}

func TestRelayNativePriorityStaysPositiveAndReversesSourceOrder(t *testing.T) {
	if got := relayNativePriority(relaySourcePriorityLimit); got != 1 {
		t.Fatalf("highest source priority mapped to %d, want 1", got)
	}
	if got := relayNativePriority(0); got != relayNativePriorityBase {
		t.Fatalf("zero source priority mapped to %d, want %d", got, relayNativePriorityBase)
	}
	if got, err := relaySourcePriorityForNative(relayNativePriority(0)); err != nil || got != 0 {
		t.Fatalf("native priority did not round-trip to source priority: %d/%v", got, err)
	}
	if _, err := relaySourcePriorityForNative(relayNativePriorityBase + relaySourcePriorityLimit + 1); err == nil {
		t.Fatal("out-of-range native relay priority was accepted")
	}
	if high, low := relayNativePriority(100), relayNativePriority(10); high >= low {
		t.Fatalf("higher source priority must map to a lower native priority: %d >= %d", high, low)
	}
}

func TestSyncRelayGroupRateMultipliersUsesHighestEnabledCorrection(t *testing.T) {
	first, second, capped, disabled, stale, endpointless, unadjusted := 0.2, 0.25, 0.6, 0.9, 0.8, 0.95, 0.9
	maxRate := 0.5
	adjustRate := false
	now := time.Now()
	repo := &relayGroupMultiplierRepo{groups: map[int64]*Group{
		1: {ID: 1, RateMultiplier: 1},
		2: {ID: 2, RateMultiplier: 0.8},
	}}
	service := &RelayStationService{groupRepo: repo}
	snapshot := relayStationConfig{
		Stations: []relayStation{
			{ID: "one", Enabled: true},
			{ID: "two", Enabled: true},
			{ID: "aihub", Type: RelayStationTypeAIHub, Enabled: true},
		},
		Bindings: []RelayGroupBinding{
			{GroupID: 1, Sources: []RelayStationSource{
				{StationID: "one", SourceGroup: "first", Enabled: true, Delta: 0.1},
				{StationID: "one", SourceGroup: "second", Enabled: true, Delta: 0.2, MaxRate: &maxRate},
				{StationID: "two", SourceGroup: "capped", Enabled: true, MaxRate: &maxRate},
				{StationID: "two", SourceGroup: "stale", Enabled: true},
				{StationID: "two", SourceGroup: "unadjusted", Enabled: true, Delta: 0.2, AdjustRate: &adjustRate},
				{StationID: "aihub", SourceGroup: "default", Enabled: true},
			}},
			{GroupID: 2, Sources: []RelayStationSource{
				{StationID: "one", SourceGroup: "disabled", Enabled: false},
			}},
		},
	}
	rates := relayRateCache{Rates: map[string]map[string]RelayStationRate{
		"one": {
			"first":    {Rate: &first, Status: RelayRateStatusReady, UpdatedAt: now},
			"second":   {Rate: &second, Status: RelayRateStatusReady, UpdatedAt: now},
			"disabled": {Rate: &disabled, Status: RelayRateStatusReady, UpdatedAt: now},
		},
		"two": {
			"capped":     {Rate: &capped, Status: RelayRateStatusReady, UpdatedAt: now},
			"stale":      {Rate: &stale, Status: RelayRateStatusReady, UpdatedAt: now.Add(-2 * relayStationRatePollInterval)},
			"unadjusted": {Rate: &unadjusted, Status: RelayRateStatusReady, UpdatedAt: now},
		},
		"aihub": {
			"default": {Rate: &endpointless, Status: RelayRateStatusReady, UpdatedAt: now},
		},
	}}

	if err := service.syncNativeRelayRates(context.Background(), snapshot, rates); err != nil {
		t.Fatalf("sync group multipliers: %v", err)
	}
	if got := repo.groups[1].RateMultiplier; got != 0.45 {
		t.Fatalf("group multiplier = %v, want highest enabled correction 0.45", got)
	}
	if got := repo.groups[2].RateMultiplier; got != 0.8 {
		t.Fatalf("group without a routeable source changed to %v, want 0.8", got)
	}
	if len(repo.updates) != 1 || repo.updates[0] != 1 {
		t.Fatalf("updated groups = %v, want only group 1", repo.updates)
	}
}
