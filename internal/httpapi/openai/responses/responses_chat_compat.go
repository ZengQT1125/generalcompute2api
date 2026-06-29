package responses

import (
	"fmt"
	"strings"

	"generalcompute2api/internal/promptcompat"
)

// convertResponsesReqToChat 将 OpenAI Responses API 请求转为 Chat Completions 格式，
// 使得上游 DeepSeek 只看到标准的 chat 请求格式，不走 responses 路径。
func convertResponsesReqToChat(req map[string]any) map[string]any {
	chat := make(map[string]any, len(req)+2)

	// 透传基础字段
	for _, k := range []string{"model", "stream", "temperature", "top_p",
		"max_tokens", "max_completion_tokens",
		"presence_penalty", "frequency_penalty", "stop",
		"tools", "tool_choice"} {
		if v, ok := req[k]; ok {
			chat[k] = v
		}
	}

	// 获取 messages（responses 同时支持 input 和 messages 字段）
	messages := promptcompat.ResponsesMessagesFromRequest(req)
	if len(messages) == 0 {
		// 留空让 NormalizeOpenAIChatRequest 报错
		return chat
	}
	chat["messages"] = messages
	return chat
}

// parseResponsesToolChoice 解析 responses 风格的 tool_choice，
// 返回 chat 兼容的 ToolChoicePolicy。
func parseResponsesToolChoice(toolChoiceRaw any, toolsRaw any) (promptcompat.ToolChoicePolicy, error) {
	return toolChoicePolicyFromResponses(toolChoiceRaw, toolsRaw)
}

// toolChoicePolicyFromResponses 是 parseToolChoicePolicy 的简化版，
// 仅处理 responses 常用的 tool_choice 格式。
func toolChoicePolicyFromResponses(toolChoiceRaw any, toolsRaw any) (promptcompat.ToolChoicePolicy, error) {
	policy := promptcompat.DefaultToolChoicePolicy()

	if toolChoiceRaw == nil {
		return policy, nil
	}

	switch v := toolChoiceRaw.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "auto":
			policy.Mode = promptcompat.ToolChoiceAuto
		case "none":
			policy.Mode = promptcompat.ToolChoiceNone
		case "required":
			policy.Mode = promptcompat.ToolChoiceRequired
		default:
			// 可能传了具体的 tool 名称
			policy.Mode = promptcompat.ToolChoiceForced
			policy.ForcedName = v
			policy.Allowed = map[string]struct{}{v: {}}
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "auto":
			policy.Mode = promptcompat.ToolChoiceAuto
		case "none":
			policy.Mode = promptcompat.ToolChoiceNone
		case "required":
			policy.Mode = promptcompat.ToolChoiceRequired
		case "function":
			name, _ := v["name"].(string)
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return policy, fmt.Errorf("tool_choice function requires name")
			}
			policy.Mode = promptcompat.ToolChoiceForced
			policy.ForcedName = name
			policy.Allowed = map[string]struct{}{name: {}}
		default:
			// 如果传了 name 字段，视为 forced
			if name, _ := v["name"].(string); strings.TrimSpace(name) != "" {
				policy.Mode = promptcompat.ToolChoiceForced
				policy.ForcedName = strings.TrimSpace(name)
				policy.Allowed = map[string]struct{}{policy.ForcedName: {}}
			}
		}
	}

	return policy, nil
}

// ensureAllowedToolNames 按 allowed 集合过滤 toolNames。
func ensureAllowedToolNames(toolNames []string, allowed map[string]struct{}) []string {
	if len(allowed) == 0 || len(toolNames) == 0 {
		return toolNames
	}
	out := make([]string, 0, len(toolNames))
	for _, name := range toolNames {
		if _, ok := allowed[name]; ok {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return toolNames // fallback
	}
	return out
}