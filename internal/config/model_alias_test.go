package config

import "testing"

func TestResolveModelDirectGLModel(t *testing.T) {
	got, ok := ResolveModel("deepseek-v3.2")
	if !ok || got != "deepseek-v3.2" {
		t.Fatalf("expected deepseek-v3.2, got ok=%v model=%q", ok, got)
	}
}

func TestResolveModelMinimaxWebAliases(t *testing.T) {
	for _, requested := range []string{"minimax2.7", "minimax-2.7", "minimax-m2-7"} {
		got, ok := ResolveModel(requested)
		if !ok || got != "minimax-m2.7" {
			t.Fatalf("expected %q to resolve to minimax-m2.7, got ok=%v model=%q", requested, ok, got)
		}
	}
}

func TestResolveModelRejectsPro(t *testing.T) {
	if got, ok := ResolveModel("deepseek-v5-pro"); ok {
		t.Fatalf("expected deepseek-v5-pro to be rejected, got %q", got)
	}
}

func TestResolveModelRejectsNoThinkingSuffix(t *testing.T) {
	for _, model := range []string{"deepseek-v5-flash-nothinking", "deepseek-v5-pro-nothinking"} {
		if got, ok := ResolveModel(model); ok {
			t.Fatalf("expected %q to be rejected, got %q", model, got)
		}
	}
}

func TestOpenAIModelByIDPreservesGLModelID(t *testing.T) {
	info, ok := OpenAIModelByID("deepseek-v3.2")
	if !ok || info.ID != "deepseek-v3.2" {
		t.Fatalf("expected advertised deepseek-v3.2, got ok=%v id=%q", ok, info.ID)
	}
}

func TestResolveModelRejectsUnknownAliases(t *testing.T) {
	for _, model := range []string{
		"gpt-4.1",
		"gpt-5",
		"claude-sonnet-4-6",
		"gemini-2.5-pro",
		"deepseek-chat",
		"deepseek-v5-pro",
	} {
		if got, ok := ResolveModel(model); ok {
			t.Fatalf("expected %q to be rejected, got %q", model, got)
		}
	}
}

func TestUpstreamDeepSeekSKU(t *testing.T) {
	if got := UpstreamDeepSeekSKU("deepseek-v3.2"); got != "deepseek-v3.2" {
		t.Fatalf("unexpected sku: %q", got)
	}
}

func TestUpstreamSafeModelType(t *testing.T) {
	if got := UpstreamSafeModelType("vision"); got != "default" {
		t.Fatalf("expected default, got %q", got)
	}
	if got := UpstreamSafeModelType(""); got != "default" {
		t.Fatalf("expected default, got %q", got)
	}
}
