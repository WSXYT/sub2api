package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const relayUsageCaptureLimit = 2 << 20

type relayOpenAIForwardInput struct {
	APIKey               *service.APIKey
	Subscription         *service.UserSubscription
	Body                 []byte
	OriginalModel        string
	UpstreamModel        string
	Stream               bool
	SessionHash          string
	PricingAt            time.Time
	ChannelUsageFields   service.ChannelUsageFields
	RequiredCapability   service.OpenAIEndpointCapability
	RequiredTransport    service.OpenAIUpstreamTransport
	RequestPlatform      string
	RequireCompact       bool
	UseUpstreamTokenCost bool
}

// tryRelayOpenAIForward lets the native scheduler choose a relay Account. A
// native account selection is left to the caller's normal loop; relay traffic
// only short-circuits after a relay identity wins the same candidate pool.
func (h *OpenAIGatewayHandler) tryRelayOpenAIForward(c *gin.Context, input relayOpenAIForwardInput, streamStarted *bool) bool {
	if h.relayService == nil || input.APIKey == nil || input.APIKey.GroupID == nil {
		return false
	}

	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		c.Request.Context(), input.APIKey.GroupID, "", input.SessionHash,
		input.OriginalModel, nil, input.RequiredTransport, input.RequiredCapability,
		input.RequireCompact, false, input.UseUpstreamTokenCost, input.RequestPlatform,
	)
	if err != nil || selection == nil || selection.Account == nil || !selection.Account.IsRelay() {
		return false
	}
	account := selection.Account
	route, err := h.relayService.ResolveRouteForAccount(c.Request.Context(), account, *input.APIKey.GroupID)
	if errors.Is(err, service.ErrRelayRouteNotFound) {
		return false
	}
	if err != nil {
		h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", "No relay source is currently available", valueOrFalse(streamStarted))
		return true
	}
	if mappedModel := account.GetMappedModel(input.OriginalModel); mappedModel != input.OriginalModel {
		input.Body = h.gatewayService.ReplaceModelInBody(input.Body, mappedModel)
		input.UpstreamModel = mappedModel
	}
	relayLogger := requestLogger(c, "handler.openai_gateway.relay")
	release, slotStatus := h.acquireResponsesAccountSlot(c, input.APIKey.GroupID, input.SessionHash, selection, input.Stream, streamStarted, relayLogger)
	if slotStatus != openAISlotAcquireOK {
		return true
	}
	if release != nil {
		defer release()
	}

	inbound := c.Request.Clone(c.Request.Context())
	inbound.Body = io.NopCloser(bytes.NewReader(input.Body))
	inbound.ContentLength = int64(len(input.Body))
	inbound.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(input.Body)), nil
	}

	startedAt := time.Now()
	response, err := h.relayService.ForwardAccount(c.Request.Context(), account, route, inbound)
	if err != nil {
		h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", "Relay request failed", valueOrFalse(streamStarted))
		return true
	}
	defer func() { _ = response.Body.Close() }()

	result, err := forwardRelayOpenAIResponse(c, response, input, startedAt, streamStarted)
	if err != nil {
		logger.L().With(
			zap.String("component", "handler.relay_station"),
			zap.String("station_id", route.StationID()),
			zap.Error(err),
		).Warn("relay response read failed")
		return true
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || result == nil {
		return true
	}

	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := c.Request.URL.Path
	quotaPlatform := service.QuotaPlatform(c.Request.Context(), input.APIKey)
	sessionID := service.ExtractClientSessionID(c)
	requestPayloadHash := service.HashUsageRequestPayload(input.Body)
	h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             input.APIKey,
			User:               input.APIKey.User,
			Account:            account,
			Subscription:       input.Subscription,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			RequestPayloadHash: requestPayloadHash,
			APIKeyService:      h.apiKeyService,
			QuotaPlatform:      quotaPlatform,
			SessionID:          sessionID,
			PricingAt:          input.PricingAt,
			ChannelUsageFields: input.ChannelUsageFields,
		}); err != nil {
			logger.L().With(
				zap.String("component", "handler.relay_station"),
				zap.String("station_id", route.StationID()),
				zap.Int64("api_key_id", input.APIKey.ID),
				zap.Error(err),
			).Error("relay usage record failed")
		}
	})
	return true
}

func valueOrFalse(value *bool) bool {
	return value != nil && *value
}

func forwardRelayOpenAIResponse(
	c *gin.Context,
	response *http.Response,
	input relayOpenAIForwardInput,
	startedAt time.Time,
	streamStarted *bool,
) (*service.OpenAIForwardResult, error) {
	result := &service.OpenAIForwardResult{
		RequestID:        response.Header.Get("x-request-id"),
		Model:            input.OriginalModel,
		UpstreamModel:    input.UpstreamModel,
		UpstreamEndpoint: c.Request.URL.Path,
		ResponseHeaders:  response.Header.Clone(),
	}
	copyRelayResponseHeaders(c.Writer.Header(), response.Header)
	c.Status(response.StatusCode)

	stream := input.Stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	result.Stream = stream
	capture, err := copyRelayResponseBody(c, response.Body, stream, startedAt, result, streamStarted)
	result.Duration = time.Since(startedAt)
	if err != nil {
		return result, err
	}
	if stream {
		applyRelaySSEResult(result, capture)
	}
	applyRelayJSONResult(result, capture)
	return result, nil
}

func copyRelayResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "set-cookie":
			continue
		}
		destination[key] = append([]string(nil), values...)
	}
}

func copyRelayResponseBody(
	c *gin.Context,
	body io.Reader,
	stream bool,
	startedAt time.Time,
	result *service.OpenAIForwardResult,
	streamStarted *bool,
) ([]byte, error) {
	capture := make([]byte, 0, relayUsageCaptureLimit)
	buffer := make([]byte, 32*1024)
	clientWriteFailed := false
	for {
		count, readErr := body.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			capture = appendRelayUsageCapture(capture, chunk)
			if result.FirstTokenMs == nil {
				firstToken := int(time.Since(startedAt).Milliseconds())
				result.FirstTokenMs = &firstToken
			}
			if !clientWriteFailed {
				if _, err := c.Writer.Write(buffer[:count]); err != nil {
					clientWriteFailed = true
				} else if stream {
					if streamStarted != nil {
						*streamStarted = true
					}
					c.Writer.Flush()
				}
			}
		}
		if readErr == io.EOF {
			return capture, nil
		}
		if readErr != nil {
			return capture, readErr
		}
	}
}

func appendRelayUsageCapture(capture, chunk []byte) []byte {
	if len(chunk) >= relayUsageCaptureLimit {
		return append(capture[:0], chunk[len(chunk)-relayUsageCaptureLimit:]...)
	}
	if overflow := len(capture) + len(chunk) - relayUsageCaptureLimit; overflow > 0 {
		copy(capture, capture[overflow:])
		capture = capture[:len(capture)-overflow]
	}
	return append(capture, chunk...)
}

func applyRelaySSEResult(result *service.OpenAIForwardResult, payload []byte) {
	for _, line := range bytes.Split(payload, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		applyRelayJSONResult(result, data)
	}
}

func applyRelayJSONResult(result *service.OpenAIForwardResult, payload []byte) {
	if result == nil || !gjson.ValidBytes(payload) {
		return
	}
	parsed := gjson.ParseBytes(payload)
	if id := strings.TrimSpace(parsed.Get("id").String()); id != "" {
		result.ResponseID = id
	} else if id := strings.TrimSpace(parsed.Get("response.id").String()); id != "" {
		result.ResponseID = id
	}
	if model := strings.TrimSpace(parsed.Get("model").String()); model != "" {
		result.UpstreamModel = model
	} else if model := strings.TrimSpace(parsed.Get("response.model").String()); model != "" {
		result.UpstreamModel = model
	}
	if usage, ok := relayOpenAIUsage(parsed.Get("usage")); ok {
		result.Usage = usage
		return
	}
	if usage, ok := relayOpenAIUsage(parsed.Get("response.usage")); ok {
		result.Usage = usage
	}
}

func relayOpenAIUsage(value gjson.Result) (service.OpenAIUsage, bool) {
	if !value.Exists() {
		return service.OpenAIUsage{}, false
	}
	input, inputOK := relayUsageInt(value, "input_tokens", "prompt_tokens")
	output, outputOK := relayUsageInt(value, "output_tokens", "completion_tokens")
	cacheRead, cacheReadOK := relayUsageInt(value, "cache_read_input_tokens", "input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens")
	cacheCreation, cacheCreationOK := relayUsageInt(value, "cache_creation_input_tokens", "input_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_tokens")
	if !inputOK && !outputOK && !cacheReadOK && !cacheCreationOK {
		return service.OpenAIUsage{}, false
	}
	return service.OpenAIUsage{
		InputTokens:              input,
		OutputTokens:             output,
		CacheReadInputTokens:     cacheRead,
		CacheCreationInputTokens: cacheCreation,
	}, true
}

func relayUsageInt(value gjson.Result, paths ...string) (int, bool) {
	for _, path := range paths {
		field := value.Get(path)
		if field.Exists() && field.Type == gjson.Number {
			return int(field.Int()), true
		}
	}
	return 0, false
}
