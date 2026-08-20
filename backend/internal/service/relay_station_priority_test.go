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
	accounts     []Account
	updates      []Account
	bindings     map[int64][]int64
	unscheduled  []int64
	extraUpdates map[int64]map[string]any
}

type relaySettingRepo struct{ SettingRepository }

func (r *relayNativeAccountRepo) FindByExtraField(_ context.Context, field string, value any) ([]Account, error) {
	result := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Extra != nil && account.Extra[field] == value {
			result = append(result, account)
		}
	}
	return result, nil
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

func (r *relayNativeAccountRepo) SetSchedulable(_ context.Context, accountID int64, schedulable bool) error {
	if !schedulable {
		r.unscheduled = append(r.unscheduled, accountID)
	}
	return nil
}

func (r *relayNativeAccountRepo) UpdateExtra(_ context.Context, accountID int64, updates map[string]any) error {
	if r.extraUpdates == nil {
		r.extraUpdates = make(map[int64]map[string]any)
	}
	r.extraUpdates[accountID] = updates
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

func TestRelayNativeAccountIdentityPreservesGroupPlatform(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek} {
		gotPlatform, accountType := relayNativeAccountIdentity(&Group{Platform: platform})
		if gotPlatform != platform || accountType != AccountTypeAPIKey {
			t.Fatalf("%s relay identity = %s/%s, want %s/%s", platform, gotPlatform, accountType, platform, AccountTypeAPIKey)
		}
	}

	platform, accountType := relayNativeAccountIdentity(&Group{Platform: PlatformAntigravity})
	if platform != PlatformAntigravity || accountType != AccountTypeOAuth {
		t.Fatalf("antigravity relay identity = %s/%s, want %s/%s", platform, accountType, PlatformAntigravity, AccountTypeOAuth)
	}

	platform, accountType = relayNativeAccountIdentity(&Group{Platform: PlatformOpenAI})
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
				settingRepo: &relaySettingRepo{},
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

func TestSyncNativeRelayAccountsDisablesDuplicateIdentity(t *testing.T) {
	const groupID int64 = 9
	key := relayAccountKey("station", groupID, "source")
	accountRepo := &relayNativeAccountRepo{accounts: []Account{
		{ID: 10, Extra: map[string]any{relayAccountMarkerKey: true, relayAccountKeyKey: key}},
		{ID: 11, Extra: map[string]any{relayAccountMarkerKey: true, relayAccountKeyKey: key}},
	}}
	service := &RelayStationService{
		settingRepo: &relaySettingRepo{},
		accountRepo: accountRepo,
		groupRepo:   &relayGroupMultiplierRepo{groups: map[int64]*Group{groupID: {ID: groupID, Platform: PlatformOpenAI}}},
		loaded:      true,
		config: relayStationConfig{
			Stations: []relayStation{{ID: "station", Name: "station", Enabled: true}},
			Bindings: []RelayGroupBinding{{GroupID: groupID, Sources: []RelayStationSource{{StationID: "station", SourceGroup: "source", Enabled: true}}}},
		},
	}

	if err := service.SyncNativeRelayAccounts(context.Background()); err != nil {
		t.Fatalf("sync relay accounts: %v", err)
	}
	if len(accountRepo.updates) != 1 || accountRepo.updates[0].ID != 10 {
		t.Fatalf("canonical updates = %#v, want only account 10", accountRepo.updates)
	}
	if len(accountRepo.unscheduled) != 1 || accountRepo.unscheduled[0] != 11 {
		t.Fatalf("disabled duplicates = %v, want [11]", accountRepo.unscheduled)
	}
}

func TestUpdateRelayBindingsRejectsMissingGroupBeforePersistence(t *testing.T) {
	service := &RelayStationService{
		settingRepo: &relaySettingRepo{},
		groupRepo:   &relayGroupMultiplierRepo{groups: map[int64]*Group{}},
		loaded:      true,
		config:      relayStationConfig{Bindings: []RelayGroupBinding{{GroupID: 1}}},
	}
	_, err := service.UpdateBindings(context.Background(), []RelayGroupBinding{{GroupID: 404}})
	if err == nil {
		t.Fatal("missing relay group was accepted")
	}
	if len(service.config.Bindings) != 1 || service.config.Bindings[0].GroupID != 1 {
		t.Fatalf("invalid binding mutated persisted config: %#v", service.config.Bindings)
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
