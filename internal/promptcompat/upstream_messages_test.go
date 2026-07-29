package promptcompat

import (
	"strings"
	"testing"
)

func weatherTools() []any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查询天气",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []any{"city"},
				},
			},
		},
	}
}

func upstreamMsg(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	m, ok := msgs[i].(map[string]any)
	if !ok {
		t.Fatalf("message %d is not a map: %T", i, msgs[i])
	}
	return m
}

// 带 tools 时，上游消息必须包含注入的工具 schema 与协议指令（修复前缺失）。
func TestUpstreamMessagesInjectsToolPrompt(t *testing.T) {
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "user", "content": "北京今天天气"},
		},
		ToolsRaw:   weatherTools(),
		ToolChoice: DefaultToolChoicePolicy(),
	}
	msgs := req.UpstreamMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system+user), got %d: %#v", len(msgs), msgs)
	}
	sys := upstreamMsg(t, msgs, 0)
	if sys["role"] != "system" {
		t.Fatalf("first message role = %v, want system", sys["role"])
	}
	content, _ := sys["content"].(string)
	for _, want := range []string{"get_weather", "Action name:", "Description:", "Parameters:", "<|QNML|tool_calls>", "<tool_calls>"} {
		if !strings.Contains(content, want) {
			t.Errorf("injected system prompt missing %q", want)
		}
	}
	user := upstreamMsg(t, msgs, 1)
	if user["role"] != "user" || user["content"] != "北京今天天气" {
		t.Errorf("user message changed unexpectedly: %#v", user)
	}
}

// 已有 system 消息时，工具提示词应追加到原 system 内容上而不是丢弃。
func TestUpstreamMessagesAppendsToExistingSystem(t *testing.T) {
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "hi"},
		},
		ToolsRaw:   weatherTools(),
		ToolChoice: DefaultToolChoicePolicy(),
	}
	msgs := req.UpstreamMessages()
	sys := upstreamMsg(t, msgs, 0)
	content, _ := sys["content"].(string)
	if !strings.Contains(content, "你是助手") {
		t.Errorf("original system content lost: %q", content)
	}
	if !strings.Contains(content, "get_weather") {
		t.Errorf("tool prompt not injected into existing system message")
	}
}

// tool_choice=none 时不注入工具提示词。
func TestUpstreamMessagesToolChoiceNoneSkipsInjection(t *testing.T) {
	policy := DefaultToolChoicePolicy()
	policy.Mode = ToolChoiceNone
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		ToolsRaw:   weatherTools(),
		ToolChoice: policy,
	}
	msgs := req.UpstreamMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := upstreamMsg(t, msgs, 0)
	if m["role"] != "user" {
		t.Errorf("unexpected system message injected under tool_choice=none")
	}
}

// 多轮工具调用历史：assistant 的 tool_calls 渲染为 QNML 文本，
// tool 角色消息改写为带 [工具返回] 头的 user 消息，连续同角色合并。
func TestUpstreamMessagesRendersToolHistory(t *testing.T) {
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "user", "content": "北京天气?"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": `{"city":"北京"}`,
						},
					},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "name": "get_weather", "content": `{"temp":30}`},
			map[string]any{"role": "assistant", "content": "北京今天30度"},
		},
		ToolsRaw:   weatherTools(),
		ToolChoice: DefaultToolChoicePolicy(),
	}
	msgs := req.UpstreamMessages()
	// system(注入) + user + assistant(QNML) + user(工具返回) + assistant
	if len(msgs) != 5 {
		t.Fatalf("expected 5 messages, got %d: %#v", len(msgs), msgs)
	}
	assistantCall := upstreamMsg(t, msgs, 2)
	if assistantCall["role"] != "assistant" {
		t.Fatalf("message 2 role = %v, want assistant", assistantCall["role"])
	}
	callText, _ := assistantCall["content"].(string)
	if !strings.Contains(callText, "<|QNML|tool_calls>") || !strings.Contains(callText, "u_get_weather") {
		t.Errorf("assistant tool_calls not rendered as QNML text: %q", callText)
	}
	toolMsg := upstreamMsg(t, msgs, 3)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool message role = %v, want user (playground has no tool role)", toolMsg["role"])
	}
	toolText, _ := toolMsg["content"].(string)
	if !strings.Contains(toolText, "[工具返回 name=get_weather tool_call_id=call_1]") {
		t.Errorf("tool result header missing: %q", toolText)
	}
	if !strings.Contains(toolText, `{"temp":30}`) {
		t.Errorf("tool result body missing: %q", toolText)
	}
	if m := upstreamMsg(t, msgs, 4); m["role"] != "assistant" || m["content"] != "北京今天30度" {
		t.Errorf("final assistant message wrong: %#v", m)
	}
}

// 连续同角色的文本消息应合并（多个工具返回合并为一条 user 消息）。
func TestUpstreamMessagesMergesConsecutiveToolResults(t *testing.T) {
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "user", "content": "查两个城市"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`}},
					map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"上海"}`}},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "name": "get_weather", "content": `{"temp":30}`},
			map[string]any{"role": "tool", "tool_call_id": "call_2", "name": "get_weather", "content": `{"temp":22}`},
		},
		ToolsRaw:   weatherTools(),
		ToolChoice: DefaultToolChoicePolicy(),
	}
	msgs := req.UpstreamMessages()
	// system + user + assistant(QNML 两个调用) + user(合并两个工具返回)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages after merge, got %d: %#v", len(msgs), msgs)
	}
	merged := upstreamMsg(t, msgs, 3)
	text, _ := merged["content"].(string)
	if !strings.Contains(text, "call_1") || !strings.Contains(text, "call_2") {
		t.Errorf("merged tool results missing entries: %q", text)
	}
}

// 多模态用户消息（image_url 数组）必须原样透传，不能被扁平化成纯文本。
func TestUpstreamMessagesPreservesMultimodalContent(t *testing.T) {
	imgBlock := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "看图"},
				imgBlock,
			}},
		},
	}
	msgs := req.UpstreamMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := upstreamMsg(t, msgs, 0)
	arr, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("multimodal content was flattened: %#v", m["content"])
	}
	if len(arr) != 2 {
		t.Fatalf("multimodal content blocks lost: %#v", arr)
	}
}

// 无 tools 的简单请求消息应保持不变（行为回归保护）。
func TestUpstreamMessagesNoToolsPassThrough(t *testing.T) {
	req := StandardRequest{
		Messages: []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "你好"},
			map[string]any{"role": "assistant", "content": "你好！有什么可以帮你？"},
			map[string]any{"role": "user", "content": "讲个笑话"},
		},
	}
	msgs := req.UpstreamMessages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(msgs), msgs)
	}
	wantRoles := []string{"system", "user", "assistant", "user"}
	wantTexts := []string{"你是助手", "你好", "你好！有什么可以帮你？", "讲个笑话"}
	for i := range wantRoles {
		m := upstreamMsg(t, msgs, i)
		if m["role"] != wantRoles[i] || m["content"] != wantTexts[i] {
			t.Errorf("message %d = %#v, want role=%s content=%s", i, m, wantRoles[i], wantTexts[i])
		}
	}
}

// CompletionPayload 集成：GL client 通过 payload["messages"].([]any) 取消息，
// 类型断言必须成立，且其中必须带有注入的工具提示词。
func TestCompletionPayloadCarriesInjectedMessages(t *testing.T) {
	req := map[string]any{
		"model": "deepseek-v3.2",
		"messages": []any{
			map[string]any{"role": "user", "content": "北京天气"},
		},
		"tools": weatherTools(),
	}
	stdReq, err := NormalizeOpenAIChatRequest(req, "trace-upstream")
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	payload := stdReq.CompletionPayload("sess-1")
	msgs, ok := payload["messages"].([]any)
	if !ok {
		t.Fatalf("payload messages type %T breaks GL client assertion", payload["messages"])
	}
	if len(msgs) == 0 {
		t.Fatal("payload messages empty")
	}
	sys := upstreamMsg(t, msgs, 0)
	if sys["role"] != "system" {
		t.Fatalf("first payload message role = %v, want system with tool prompt", sys["role"])
	}
	content, _ := sys["content"].(string)
	if !strings.Contains(content, "get_weather") || !strings.Contains(content, "<|QNML|tool_calls>") {
		t.Errorf("payload system message lacks tool injection: %.200q", content)
	}
}
