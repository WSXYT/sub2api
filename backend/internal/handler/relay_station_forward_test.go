package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestApplyRelayOpenAIResult(t *testing.T) {
	result := &service.OpenAIForwardResult{Model: "gpt-test"}
	applyRelayJSONResult(result, []byte(`{
		"id":"chatcmpl_1",
		"model":"gpt-upstream",
		"usage":{"prompt_tokens":20,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":5}}
	}`))

	if result.ResponseID != "chatcmpl_1" || result.UpstreamModel != "gpt-upstream" {
		t.Fatalf("unexpected response metadata: %#v", result)
	}
	if result.Usage.InputTokens != 20 || result.Usage.OutputTokens != 8 || result.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("unexpected usage: %#v", result.Usage)
	}
}

func TestApplyRelaySSEResult(t *testing.T) {
	result := &service.OpenAIForwardResult{Model: "gpt-test"}
	applyRelaySSEResult(result, []byte("event: response.completed\n"+
		"data: {\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\",\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}}\n\n"+
		"data: [DONE]\n"))

	if result.ResponseID != "resp_1" || result.UpstreamModel != "gpt-upstream" {
		t.Fatalf("unexpected response metadata: %#v", result)
	}
	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected usage: %#v", result.Usage)
	}
}

func TestApplyRelayGatewayResultMergesAnthropicStreamUsage(t *testing.T) {
	result := &service.ForwardResult{Model: "claude-test"}
	applyRelayGatewayJSONResult(result, []byte(`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":3}}}`))
	applyRelayGatewayJSONResult(result, []byte(`{"type":"message_delta","usage":{"output_tokens":5}}`))

	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 5 || result.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("unexpected merged Anthropic usage: %#v", result.Usage)
	}
}

func TestRelaySSEChunkAccumulatorPreservesLongStreamUsage(t *testing.T) {
	result := &service.ForwardResult{Model: "claude-test"}
	accumulator := newRelaySSEChunkAccumulator(func(event []byte) { applyRelayGatewaySSEResult(result, event) })
	accumulator.consume([]byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":3}}}\n\n"))
	accumulator.consume([]byte("data: {\"delta\":\"" + strings.Repeat("x", relayUsageCaptureLimit+1) + "\"}\n\n"))
	accumulator.consume([]byte("event: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n\n"))
	accumulator.finish()

	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 5 || result.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("long stream lost usage: %#v", result.Usage)
	}
}

func TestApplyRelayGatewayResultReadsGeminiUsage(t *testing.T) {
	result := &service.ForwardResult{Model: "gemini-test"}
	applyRelayGatewayJSONResult(result, []byte(`{
		"modelVersion":"gemini-upstream",
		"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"cachedContentTokenCount":4}
	}`))

	if result.UpstreamResponseModel != "gemini-upstream" {
		t.Fatalf("unexpected upstream model: %q", result.UpstreamResponseModel)
	}
	if result.Usage.InputTokens != 20 || result.Usage.OutputTokens != 8 || result.Usage.CacheReadInputTokens != 4 {
		t.Fatalf("unexpected Gemini usage: %#v", result.Usage)
	}
}

func TestRelayGatewayFailoverErrorPreservesPoolRetryWithoutClientDetails(t *testing.T) {
	account := &service.Account{
		Platform:    service.PlatformOpenAI,
		Type:        "relay",
		Credentials: map[string]any{"pool_mode": true},
		Extra:       map[string]any{"relay_account": true},
	}
	failoverErr, ok := relayGatewayFailoverError(account, http.StatusTooManyRequests).(*service.UpstreamFailoverError)
	if !ok {
		t.Fatalf("unexpected relay failover error type")
	}
	if !failoverErr.RetryableOnSameAccount {
		t.Fatalf("relay 429 did not preserve pool-mode same-account retry")
	}
	if failoverErr.StatusCode != http.StatusBadGateway || failoverErr.ClientStatusCode != http.StatusBadGateway || string(failoverErr.ResponseBody) != `{"error":{"message":"Upstream request failed"}}` {
		t.Fatalf("relay failover error exposed upstream status: %#v", failoverErr)
	}
}

func TestCopyRelayResponseHeadersHidesUpstreamIdentity(t *testing.T) {
	destination := make(http.Header)
	source := http.Header{
		"Content-Type":        []string{"application/json"},
		"Content-Disposition": []string{`attachment; filename="result.json"`},
		"Set-Cookie":          []string{"secret=value"},
		"Server":              []string{"secret-provider"},
		"Via":                 []string{"secret-relay"},
		"X-Upstream-Provider": []string{"secret-provider"},
		"X-Aihub-Auto-Rate":   []string{"0.03"},
	}
	copyRelayResponseHeaders(destination, source)

	if destination.Get("Content-Type") != "application/json" || destination.Get("Content-Disposition") != "" {
		t.Fatalf("response headers were not canonicalized: %#v", destination)
	}
	for _, key := range []string{"Set-Cookie", "Server", "Via", "X-Upstream-Provider", "Content-Encoding", "X-Aihub-Auto-Rate"} {
		if destination.Get(key) != "" {
			t.Fatalf("upstream identity header %s was copied: %#v", key, destination)
		}
	}
}
