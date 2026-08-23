package admin

import (
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RelayStationHandler exposes credential-safe administrator APIs for relay stations.
type RelayStationHandler struct {
	relayService *service.RelayStationService
}

func NewRelayStationHandler(relayService *service.RelayStationService) *RelayStationHandler {
	return &RelayStationHandler{relayService: relayService}
}

type createRelayStationRequest struct {
	Type       service.RelayStationType `json:"type" binding:"required,oneof=aihub newapi sub2api"`
	Name       string                   `json:"name" binding:"required,max=100"`
	APIKeyName string                   `json:"api_key_name" binding:"omitempty,max=100"`
	BaseURL    string                   `json:"base_url" binding:"omitempty,max=2048"`
	ControlURL string                   `json:"control_url" binding:"omitempty,max=2048"`
	UIPassword string                   `json:"ui_password" binding:"omitempty,max=2048"`
	ProxyToken string                   `json:"proxy_token" binding:"omitempty,max=4096"`
	Username   string                   `json:"username" binding:"omitempty,max=512"`
	Password   string                   `json:"password" binding:"omitempty,max=4096"`
	Enabled    *bool                    `json:"enabled"`
}

type updateRelayStationRequest struct {
	Name       *string `json:"name" binding:"omitempty,max=100"`
	APIKeyName *string `json:"api_key_name" binding:"omitempty,max=100"`
	BaseURL    *string `json:"base_url" binding:"omitempty,max=2048"`
	ControlURL *string `json:"control_url" binding:"omitempty,max=2048"`
	UIPassword *string `json:"ui_password" binding:"omitempty,max=2048"`
	ProxyToken *string `json:"proxy_token" binding:"omitempty,max=4096"`
	Username   *string `json:"username" binding:"omitempty,max=512"`
	Password   *string `json:"password" binding:"omitempty,max=4096"`
	Enabled    *bool   `json:"enabled"`
}

type updateRelayBindingsRequest struct {
	Bindings []service.RelayGroupBinding `json:"bindings" binding:"required"`
}

type updateRelayAccountRequest struct {
	GroupID     int64  `json:"group_id" binding:"required"`
	SourceGroup string `json:"source_group"`
	Enabled     *bool  `json:"enabled"`
	Priority    *int   `json:"priority"`
}

// List handles GET /api/v1/admin/relay-stations.
func (h *RelayStationHandler) List(c *gin.Context) {
	stations, err := h.relayService.ListStations(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, stations)
}

// ListAccounts handles GET /api/v1/admin/relay-stations/accounts.
func (h *RelayStationHandler) ListAccounts(c *gin.Context) {
	accounts, err := h.relayService.ListRelayAccounts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"accounts": accounts})
}

// UpdateAccount handles PATCH /api/v1/admin/relay-stations/accounts/:station_id.
func (h *RelayStationHandler) UpdateAccount(c *gin.Context) {
	var request updateRelayAccountRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("VALIDATION_ERROR", "invalid relay account update"))
		return
	}
	if err := h.relayService.UpdateRelayAccount(c.Request.Context(), strings.TrimSpace(c.Param("station_id")), request.GroupID, request.SourceGroup, service.RelayAccountUpdateInput{
		Enabled:  request.Enabled,
		Priority: request.Priority,
	}); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"updated": true})
}

// Get handles GET /api/v1/admin/relay-stations/:id.
func (h *RelayStationHandler) Get(c *gin.Context) {
	station, err := h.relayService.GetStation(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, station)
}

// Create handles POST /api/v1/admin/relay-stations.
func (h *RelayStationHandler) Create(c *gin.Context) {
	var request createRelayStationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("VALIDATION_ERROR", "invalid relay station request"))
		return
	}
	station, err := h.relayService.CreateStation(c.Request.Context(), service.RelayStationCreateInput{
		Type:       request.Type,
		Name:       request.Name,
		APIKeyName: request.APIKeyName,
		BaseURL:    request.BaseURL,
		ControlURL: request.ControlURL,
		UIPassword: request.UIPassword,
		ProxyToken: request.ProxyToken,
		Username:   request.Username,
		Password:   request.Password,
		Enabled:    request.Enabled,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, gin.H{
		"station":      station,
		"aihub_synced": h.relayService.SyncAIHubConfig(c.Request.Context()) == nil,
	})
}

// Update handles PUT /api/v1/admin/relay-stations/:id.
func (h *RelayStationHandler) Update(c *gin.Context) {
	var request updateRelayStationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("VALIDATION_ERROR", "invalid relay station request"))
		return
	}
	station, err := h.relayService.UpdateStation(c.Request.Context(), strings.TrimSpace(c.Param("id")), service.RelayStationUpdateInput{
		Name:        request.Name,
		APIKeyName:  request.APIKeyName,
		BaseURL:     request.BaseURL,
		ControlURL: request.ControlURL,
		UIPassword: request.UIPassword,
		ProxyToken: request.ProxyToken,
		Username:   request.Username,
		Password:   request.Password,
		Enabled:    request.Enabled,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"station":      station,
		"aihub_synced": h.relayService.SyncAIHubConfig(c.Request.Context()) == nil,
	})
}

// Delete handles DELETE /api/v1/admin/relay-stations/:id.
func (h *RelayStationHandler) Delete(c *gin.Context) {
	if err := h.relayService.DeleteStation(c.Request.Context(), strings.TrimSpace(c.Param("id"))); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"aihub_synced": h.relayService.SyncAIHubConfig(c.Request.Context()) == nil})
}

// Test handles POST /api/v1/admin/relay-stations/:id/test.
func (h *RelayStationHandler) Test(c *gin.Context) {
	rates, err := h.relayService.TestStation(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"rates": rates})
}

// ListBindings handles GET /api/v1/admin/relay-stations/bindings.
func (h *RelayStationHandler) ListBindings(c *gin.Context) {
	bindings, err := h.relayService.ListBindings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"bindings": bindings})
}

// UpdateBindings handles PUT /api/v1/admin/relay-stations/bindings.
func (h *RelayStationHandler) UpdateBindings(c *gin.Context) {
	var request updateRelayBindingsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("VALIDATION_ERROR", "invalid relay bindings request"))
		return
	}
	bindings, err := h.relayService.UpdateBindings(c.Request.Context(), request.Bindings)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	syncErr := h.relayService.SyncAIHubConfig(c.Request.Context())
	payload := gin.H{"bindings": bindings, "aihub_synced": syncErr == nil}
	if syncErr != nil {
		logger.L().Warn("relay aihub synchronization failed", zap.String("error_type", fmt.Sprintf("%T", syncErr)))
		payload["aihub_sync_error"] = "relay synchronization failed"
	}
	response.Success(c, payload)
}

// ListGroups handles GET /api/v1/admin/relay-stations/:id/groups.
func (h *RelayStationHandler) ListGroups(c *gin.Context) {
	groups, err := h.relayService.ListGroups(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"groups": groups})
}

// ListRates handles GET /api/v1/admin/relay-stations/rates.
func (h *RelayStationHandler) ListRates(c *gin.Context) {
	stationID := strings.TrimSpace(c.Query("station_id"))
	var (
		rates []service.RelayRateView
		err   error
	)
	if stationID == "" {
		rates, err = h.relayService.ListRates(c.Request.Context())
	} else {
		rates, err = h.relayService.ListRatesForStation(c.Request.Context(), stationID)
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"rates": rates})
}

// RefreshRates handles POST /api/v1/admin/relay-stations/rates/refresh.
func (h *RelayStationHandler) RefreshRates(c *gin.Context) {
	refreshErr := h.relayService.RefreshRates(c.Request.Context())
	rates, err := h.relayService.ListRates(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	payload := gin.H{"rates": rates, "refreshed": refreshErr == nil}
	if refreshErr != nil {
		logger.L().Warn("relay rate refresh failed", zap.String("error_type", fmt.Sprintf("%T", refreshErr)))
		payload["error"] = "relay rate refresh failed"
	}
	response.Success(c, payload)
}

// SyncAIHub handles POST /api/v1/admin/relay-stations/sync.
func (h *RelayStationHandler) SyncAIHub(c *gin.Context) {
	if err := h.relayService.SyncAIHubConfig(c.Request.Context()); err != nil {
		response.ErrorFrom(c, infraerrors.New(502, "AIHUB_SYNC_FAILED", "failed to synchronize aihub configuration"))
		return
	}
	response.Success(c, gin.H{"synced": true})
}

// Profit handles GET /api/v1/admin/relay-stations/profit.
func (h *RelayStationHandler) Profit(c *gin.Context) {
	start, end, err := relayProfitRange(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	estimates, err := h.relayService.EstimateProfit(c.Request.Context(), start, end)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"start_at": start, "end_at": end, "estimates": estimates})
}

func relayProfitRange(c *gin.Context) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -30)
	if raw := strings.TrimSpace(c.Query("start_at")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, infraerrors.BadRequest("RELAY_PROFIT_RANGE_INVALID", "start_at must use RFC3339")
		}
		start = parsed
	}
	if raw := strings.TrimSpace(c.Query("end_at")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, infraerrors.BadRequest("RELAY_PROFIT_RANGE_INVALID", "end_at must use RFC3339")
		}
		end = parsed
	}
	if !end.After(start) || end.Sub(start) > 366*24*time.Hour {
		return time.Time{}, time.Time{}, infraerrors.BadRequest("RELAY_PROFIT_RANGE_INVALID", "profit range must be positive and no longer than 366 days")
	}
	return start, end, nil
}
