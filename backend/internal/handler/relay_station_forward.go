package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const relayUsageCaptureLimit = 2 << 20

type relayGatewayForwardInput struct {
	Body          []byte
	Path          string
	OriginalModel string
	UpstreamModel string
	Stream        bool
}

func (h *OpenAIGatewayHandler) forwardRelayOpenAIAccount(
	ctx context.Context,
	c *gin.Context,
	account *service.Account,
	groupID int64,
	input relayGatewayForwardInput,
	streamStarted *bool,
) (*service.OpenAIForwardResult, error) {
	if h.relayService == nil || account == nil || !account.IsRelay() {
		return nil, service.ErrRelayRouteNotFound
	}
	route, err := h.relayService.ResolveRouteForAccount(ctx, account, groupID)
	if err != nil {
		return nil, relayGatewayFailoverError(account, 0)
	}
	if h.gatewayService != nil {
		latest, vetoed, _ := h.gatewayService.OpenAIProfitControlVetoLatest(ctx, account)
		if vetoed {
			return nil, relayGatewayFailoverError(account, 0)
		}
		if latest != nil {
			account = latest
		}
	}
	inbound := relayGatewayInboundRequest(ctx, c, input)
	startedAt := time.Now()
	response, err := h.relayService.ForwardAccount(ctx, account, route, inbound)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		statusCode, _ := service.RelayUpstreamStatus(err)
		return nil, relayGatewayFailoverError(account, statusCode)
	}
	defer func() { _ = response.Body.Close() }()
	return forwardRelayOpenAIResponse(c, response, input, startedAt, streamStarted)
}

func (h *GatewayHandler) forwardRelayAccount(
	ctx context.Context,
	c *gin.Context,
	account *service.Account,
	groupID int64,
	input relayGatewayForwardInput,
	streamStarted *bool,
) (*service.ForwardResult, error) {
	return forwardRelayGatewayAccount(h.gatewayService, h.relayService, ctx, c, account, groupID, input, streamStarted)
}

func forwardRelayGatewayAccount(
	gatewayService *service.GatewayService,
	relayService *service.RelayStationService,
	ctx context.Context,
	c *gin.Context,
	account *service.Account,
	groupID int64,
	input relayGatewayForwardInput,
	streamStarted *bool,
) (*service.ForwardResult, error) {
	if relayService == nil || account == nil || !account.IsRelay() {
		return nil, service.ErrRelayRouteNotFound
	}
	route, err := relayService.ResolveRouteForAccount(ctx, account, groupID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, relayGatewayFailoverError(account, 0)
	}
	if gatewayService != nil {
		latest, vetoed, _ := gatewayService.GatewayProfitControlVetoLatest(ctx, account)
		if vetoed {
			return nil, relayGatewayFailoverError(account, 0)
		}
		if latest != nil {
			account = latest
		}
	}

	inbound := relayGatewayInboundRequest(ctx, c, input)
	startedAt := time.Now()
	response, err := relayService.ForwardAccount(ctx, account, route, inbound)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		statusCode, _ := service.RelayUpstreamStatus(err)
		return nil, relayGatewayFailoverError(account, statusCode)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, relayGatewayFailoverError(account, response.StatusCode)
	}

	result := &service.ForwardResult{
		RequestID:         response.Header.Get("x-request-id"),
		Model:             input.OriginalModel,
		SelectedRelayRate: service.RelaySelectedRate(response.Header),
		UpstreamModel:     input.UpstreamModel,
		Stream:            input.Stream,
	}
	copyRelayResponseHeaders(c.Writer.Header(), response.Header)
	c.Status(response.StatusCode)
	stream := input.Stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	var streamUsage *relaySSEChunkAccumulator
	if stream {
		streamUsage = newRelaySSEChunkAccumulator(func(event []byte) { applyRelayGatewaySSEResult(result, event) })
	}
	capture, firstTokenMs, err := copyRelayResponseBody(c, response.Body, stream, startedAt, streamStarted, relaySSEConsumer(streamUsage))
	if streamUsage != nil {
		streamUsage.finish()
	}
	result.Duration = time.Since(startedAt)
	result.FirstTokenMs = firstTokenMs
	if err != nil {
		if errors.Is(err, service.ErrRelayUpstreamFailed) {
			return result, relayGatewayFailoverError(account, 0)
		}
		return result, err
	}
	if !stream {
		applyRelayGatewayJSONResult(result, capture)
	}
	return result, nil
}

func relayGatewayInboundRequest(ctx context.Context, c *gin.Context, input relayGatewayForwardInput) *http.Request {
	inbound := c.Request.Clone(ctx)
	if input.Path != "" {
		inbound.URL.Path = input.Path
		inbound.URL.RawPath = ""
	}
	if input.Stream {
		inbound.Header.Set("X-Sub2API-Relay-Expected-Stream", "1")
	}
	inbound.Body = io.NopCloser(bytes.NewReader(input.Body))
	inbound.ContentLength = int64(len(input.Body))
	inbound.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(input.Body)), nil
	}
	return inbound
}

func relayGatewayFailoverError(account *service.Account, upstreamStatus int) error {
	retryableOnSameAccount := account != nil && account.IsPoolMode() && account.IsPoolModeRetryableStatus(upstreamStatus)
	return &service.UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":{"message":"Upstream request failed"}}`),
		RetryableOnSameAccount: retryableOnSameAccount,
		Stage:                  service.GatewayFailureStageInference,
		Scope:                  service.GatewayFailureScopeAccount,
		ClientStatusCode:       http.StatusBadGateway,
		ClientMessage:          "Upstream request failed",
	}
}

func applyRelayGatewayJSONResult(result *service.ForwardResult, payload []byte) {
	if result == nil || !gjson.ValidBytes(payload) {
		return
	}
	parsed := gjson.ParseBytes(payload)
	for _, path := range []string{"model", "modelVersion", "response.model", "response.modelVersion"} {
		if model := strings.TrimSpace(parsed.Get(path).String()); model != "" {
			result.UpstreamResponseModel = model
			break
		}
	}
	for _, path := range []string{"usage", "message.usage", "response.usage"} {
		mergeRelayClaudeUsage(&result.Usage, relayClaudeUsage(parsed.Get(path)))
	}
	for _, path := range []string{"usageMetadata", "response.usageMetadata"} {
		mergeRelayClaudeUsage(&result.Usage, relayGeminiUsage(parsed.Get(path)))
	}
}

func relayClaudeUsage(value gjson.Result) service.ClaudeUsage {
	input, _ := relayUsageInt(value, "input_tokens", "prompt_tokens")
	output, _ := relayUsageInt(value, "output_tokens", "completion_tokens")
	cacheRead, _ := relayUsageInt(value, "cache_read_input_tokens", "input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens")
	cacheCreation, _ := relayUsageInt(value, "cache_creation_input_tokens", "input_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_tokens")
	return service.ClaudeUsage{InputTokens: input, OutputTokens: output, CacheReadInputTokens: cacheRead, CacheCreationInputTokens: cacheCreation}
}

func relayGeminiUsage(value gjson.Result) service.ClaudeUsage {
	input, _ := relayUsageInt(value, "promptTokenCount")
	output, _ := relayUsageInt(value, "candidatesTokenCount")
	cacheRead, _ := relayUsageInt(value, "cachedContentTokenCount")
	return service.ClaudeUsage{InputTokens: input, OutputTokens: output, CacheReadInputTokens: cacheRead}
}

func mergeRelayClaudeUsage(destination *service.ClaudeUsage, candidate service.ClaudeUsage) {
	if candidate.InputTokens > destination.InputTokens {
		destination.InputTokens = candidate.InputTokens
	}
	if candidate.OutputTokens > destination.OutputTokens {
		destination.OutputTokens = candidate.OutputTokens
	}
	if candidate.CacheReadInputTokens > destination.CacheReadInputTokens {
		destination.CacheReadInputTokens = candidate.CacheReadInputTokens
	}
	if candidate.CacheCreationInputTokens > destination.CacheCreationInputTokens {
		destination.CacheCreationInputTokens = candidate.CacheCreationInputTokens
	}
}

func forwardRelayOpenAIResponse(
	c *gin.Context,
	response *http.Response,
	input relayGatewayForwardInput,
	startedAt time.Time,
	streamStarted *bool,
) (*service.OpenAIForwardResult, error) {
	result := &service.OpenAIForwardResult{
		RequestID:         response.Header.Get("x-request-id"),
		Model:             input.OriginalModel,
		SelectedRelayRate: service.RelaySelectedRate(response.Header),
		UpstreamModel:     input.UpstreamModel,
		UpstreamEndpoint:  c.Request.URL.Path,
		ResponseHeaders:   response.Header.Clone(),
	}
	copyRelayResponseHeaders(c.Writer.Header(), response.Header)
	c.Status(response.StatusCode)

	stream := input.Stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	result.Stream = stream
	var streamUsage *relaySSEChunkAccumulator
	if stream {
		streamUsage = newRelaySSEChunkAccumulator(func(event []byte) { applyRelaySSEResult(result, event) })
	}
	capture, firstTokenMs, err := copyRelayResponseBody(c, response.Body, stream, startedAt, streamStarted, relaySSEConsumer(streamUsage))
	if streamUsage != nil {
		streamUsage.finish()
	}
	result.Duration = time.Since(startedAt)
	result.FirstTokenMs = firstTokenMs
	if err != nil {
		if errors.Is(err, service.ErrRelayUpstreamFailed) {
			return result, relayGatewayFailoverError(nil, 0)
		}
		return result, err
	}
	if !stream {
		applyRelayJSONResult(result, capture)
	}
	return result, nil
}

func copyRelayResponseHeaders(destination, source http.Header) {
	if strings.Contains(strings.ToLower(source.Get("Content-Type")), "text/event-stream") {
		destination.Set("Content-Type", "text/event-stream")
		return
	}
	destination.Set("Content-Type", "application/json")
}

func copyRelayResponseBody(
	c *gin.Context,
	body io.Reader,
	stream bool,
	startedAt time.Time,
	streamStarted *bool,
	onChunk func([]byte),
) ([]byte, *int, error) {
	capture := make([]byte, 0, relayUsageCaptureLimit)
	buffer := make([]byte, 32*1024)
	clientWriteFailed := false
	var firstTokenMs *int
	for {
		count, readErr := body.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			capture = appendRelayUsageCapture(capture, chunk)
			if onChunk != nil {
				onChunk(chunk)
			}
			if firstTokenMs == nil {
				firstToken := int(time.Since(startedAt).Milliseconds())
				firstTokenMs = &firstToken
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
			return capture, firstTokenMs, nil
		}
		if readErr != nil {
			return capture, firstTokenMs, readErr
		}
	}
}

type relaySSEChunkAccumulator struct {
	pending []byte
	apply   func([]byte)
}

func newRelaySSEChunkAccumulator(apply func([]byte)) *relaySSEChunkAccumulator {
	return &relaySSEChunkAccumulator{apply: apply}
}

func relaySSEConsumer(accumulator *relaySSEChunkAccumulator) func([]byte) {
	if accumulator == nil {
		return nil
	}
	return accumulator.consume
}

func (a *relaySSEChunkAccumulator) consume(chunk []byte) {
	if a == nil || a.apply == nil || len(chunk) == 0 {
		return
	}
	a.pending = append(a.pending, chunk...)
	for {
		index, delimiterLength := relaySSEDelimiter(a.pending)
		if index < 0 {
			return
		}
		end := index + delimiterLength
		a.apply(a.pending[:end])
		a.pending = append(a.pending[:0], a.pending[end:]...)
	}
}

func (a *relaySSEChunkAccumulator) finish() {
	if a == nil || a.apply == nil || len(bytes.TrimSpace(a.pending)) == 0 {
		return
	}
	a.apply(a.pending)
	a.pending = nil
}

func relaySSEDelimiter(payload []byte) (int, int) {
	lf := bytes.Index(payload, []byte("\n\n"))
	crlf := bytes.Index(payload, []byte("\r\n\r\n"))
	if lf >= 0 && (crlf < 0 || lf < crlf) {
		return lf, 2
	}
	if crlf >= 0 {
		return crlf, 4
	}
	return -1, 0
}

func applyRelayGatewaySSEResult(result *service.ForwardResult, payload []byte) {
	for _, line := range bytes.Split(payload, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			applyRelayGatewayJSONResult(result, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
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
