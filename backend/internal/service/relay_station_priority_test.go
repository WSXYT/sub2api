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

func TestSyncRelayGroupRateMultipliersUsesHighestEffectiveRate(t *testing.T) {
	first, second, capped, disabled, stale, endpointless := 0.2, 0.25, 0.6, 0.9, 0.8, 0.95
	maxRate := 0.5
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
			"capped": {Rate: &capped, Status: RelayRateStatusReady, UpdatedAt: now},
			"stale":  {Rate: &stale, Status: RelayRateStatusReady, UpdatedAt: now.Add(-2 * relayStationRatePollInterval)},
		},
		"aihub": {
			"default": {Rate: &endpointless, Status: RelayRateStatusReady, UpdatedAt: now},
		},
	}}

	if err := service.syncNativeRelayRates(context.Background(), snapshot, rates); err != nil {
		t.Fatalf("sync group multipliers: %v", err)
	}
	if got := repo.groups[1].RateMultiplier; got != 0.45 {
		t.Fatalf("group multiplier = %v, want highest effective rate 0.45", got)
	}
	if got := repo.groups[2].RateMultiplier; got != 0.8 {
		t.Fatalf("group without a routeable source changed to %v, want 0.8", got)
	}
	if len(repo.updates) != 1 || repo.updates[0] != 1 {
		t.Fatalf("updated groups = %v, want only group 1", repo.updates)
	}
}
