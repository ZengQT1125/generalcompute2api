package promptcompat

import (
	"strings"
)

// UpstreamMessages builds the message list actually sent to the GL playground
// upstream. The playground endpoint has no native tool registry, so tool
// support is realized purely by prompt injection:
//
//   - tool schemas + tool-call protocol instructions are injected into the
//     system message (reusing injectToolPrompt, the same renderer used for
//     the flattened DeepSeek-style prompt);
//   - assistant messages carrying tool_calls are rendered as text with the
//     QNML markup history block appended;
//   - tool/function role messages are rewritten as user messages with a
//     textual tool-result header, because the playground only accepts
//     system/user/assistant roles;
//   - user content is preserved as-is so multimodal blocks (e.g. image_url)
//     keep working;
//   - consecutive same-role text messages are merged to keep the transcript
//     strictly role-alternating for playground-side validation.
func (r StandardRequest) UpstreamMessages() []any {
	msgs := normalizeUpstreamPlaygroundMessages(r.Messages)
	if tools, ok := r.ToolsRaw.([]any); ok && len(tools) > 0 {
		msgs, _ = injectToolPrompt(msgs, tools, r.ToolChoice)
	}
	msgs = mergeConsecutiveUpstreamMessages(msgs)
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m)
	}
	return out
}

func normalizeUpstreamPlaygroundMessages(raw []any) []map[string]any {
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(asString(msg["role"])))
		switch role {
		case "assistant":
			content := buildAssistantContentForPrompt(msg)
			if content == "" {
				continue
			}
			out = append(out, map[string]any{"role": "assistant", "content": content})
		case "tool", "function":
			content := buildToolContentForPrompt(msg)
			if header := upstreamToolResultHeader(msg); header != "" {
				content = header + "\n" + content
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		case "system", "developer":
			// System content is flattened to text so tool-prompt injection can
			// append to it; empty system messages carry no information.
			content := NormalizeOpenAIContentForPrompt(msg["content"])
			if strings.TrimSpace(content) == "" {
				continue
			}
			out = append(out, map[string]any{"role": "system", "content": content})
		case "user":
			// Preserve original content (string or multimodal array) unchanged.
			out = append(out, map[string]any{"role": "user", "content": msg["content"]})
		default:
			content := NormalizeOpenAIContentForPrompt(msg["content"])
			if content == "" {
				continue
			}
			if role == "" {
				role = "user"
			}
			out = append(out, map[string]any{"role": normalizeOpenAIRoleForPrompt(role), "content": content})
		}
	}
	return out
}

func upstreamToolResultHeader(msg map[string]any) string {
	parts := make([]string, 0, 2)
	if name := strings.TrimSpace(asString(msg["name"])); name != "" {
		parts = append(parts, "name="+name)
	}
	if callID := strings.TrimSpace(asString(msg["tool_call_id"])); callID != "" {
		parts = append(parts, "tool_call_id="+callID)
	}
	if len(parts) == 0 {
		return "[工具返回]"
	}
	return "[工具返回 " + strings.Join(parts, " ") + "]"
}

func mergeConsecutiveUpstreamMessages(messages []map[string]any) []map[string]any {
	merged := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		if len(merged) > 0 {
			prev := merged[len(merged)-1]
			prevText, prevOK := prev["content"].(string)
			curText, curOK := msg["content"].(string)
			if prev["role"] == msg["role"] && prevOK && curOK {
				merged[len(merged)-1] = map[string]any{
					"role":    prev["role"],
					"content": strings.TrimSpace(prevText + "\n\n" + curText),
				}
				continue
			}
		}
		merged = append(merged, msg)
	}
	return merged
}
