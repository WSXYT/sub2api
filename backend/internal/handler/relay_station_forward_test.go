package handler

import (
	"net/http"
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

func TestCopyRelayResponseHeadersSkipsCookies(t *testing.T) {
	destination := make(http.Header)
	source := http.Header{
		"Content-Type": []string{"application/json"},
		"Set-Cookie":   []string{"secret=value"},
		"Connection":   []string{"keep-alive"},
	}
	copyRelayResponseHeaders(destination, source)

	if destination.Get("Content-Type") != "application/json" {
		t.Fatalf("content type was not copied: %#v", destination)
	}
	if destination.Get("Set-Cookie") != "" || destination.Get("Connection") != "" {
		t.Fatalf("unsafe response headers were copied: %#v", destination)
	}
}
