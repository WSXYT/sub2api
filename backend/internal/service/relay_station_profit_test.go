package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

type relayProfitSettingRepo struct{ SettingRepository }

type relayProfitUsageRepo struct {
	UsageLogRepository
	filters usagestats.UsageLogFilters
}

func (r *relayProfitUsageRepo) GetGroupStatsWithUsageFilters(_ context.Context, _ time.Time, _ time.Time, filters usagestats.UsageLogFilters) ([]usagestats.GroupStat, error) {
	r.filters = filters
	return []usagestats.GroupStat{{GroupID: 1, Requests: 2, Cost: 4}}, nil
}

func TestEstimateProfitExcludesAdminUsage(t *testing.T) {
	rate := 0.25
	repo := &relayProfitUsageRepo{}
	service := &RelayStationService{}
	service.settingRepo = relayProfitSettingRepo{}
	service.groupRepo = &relayGroupMultiplierRepo{groups: map[int64]*Group{
		1: {ID: 1, Name: "relay", RateMultiplier: 0.5},
	}}
	service.usage = &UsageService{usageRepo: repo}
	service.loaded = true
	service.config = relayStationConfig{
		Stations: []relayStation{{ID: "station", Enabled: true}},
		Bindings: []RelayGroupBinding{{GroupID: 1, Sources: []RelayStationSource{{
			StationID: "station", SourceGroup: "default", Enabled: true,
		}}}},
	}
	service.rates = relayRateCache{Rates: map[string]map[string]RelayStationRate{
		"station": {"default": {Rate: &rate, Status: RelayRateStatusReady}},
	}}

	estimates, err := service.EstimateProfit(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("estimate profit: %v", err)
	}
	if !repo.filters.ExcludeAdmin {
		t.Fatal("profit estimate included administrator usage")
	}
	if len(estimates) != 1 || estimates[0].EstimatedProfit == nil || *estimates[0].EstimatedProfit != 1 {
		t.Fatalf("profit estimates = %#v", estimates)
	}
}
