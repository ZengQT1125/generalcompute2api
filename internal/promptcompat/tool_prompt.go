package promptcompat

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"generalcompute2api/internal/toolcall"
)

func injectToolPrompt(messages []map[string]any, tools []any, policy ToolChoicePolicy) ([]map[string]any, []string) {
	if policy.IsNone() {
		return messages, nil
	}
	toolSchemas := make([]string, 0, len(tools))
	names := make([]string, 0, len(tools))
	isAllowed := func(name string) bool {
		if strings.TrimSpace(name) == "" {
			return false
		}
		if len(policy.Allowed) == 0 {
			return true
		}
		_, ok := policy.Allowed[name]
		return ok
	}

	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, desc, schema := toolcall.ExtractToolMeta(tool)
		name = strings.TrimSpace(name)
		if !isAllowed(name) {
			continue
		}
		names = append(names, name)
		if desc == "" {
			desc = "暂无说明"
		}
		displayName := toolcall.ToQwenToolName(name)
		b, _ := json.Marshal(schema)
		toolSchemas = append(toolSchemas, fmt.Sprintf("Action name: %s\nDescription: %s\nParameters: %s", displayName, desc, string(b)))
	}
	if len(toolSchemas) == 0 {
		return messages, names
	}
	toolPrompt := strings.Join([]string{
		"IMPORTANT: Reply in the same language as the user.",
		"IMPORTANT: When the task requires files, shell, web, browser, agents, skills, or other tools, emit a tool call (QNML or standard XML) immediately instead of explaining what you would do.",
		"IMPORTANT: Complete multi-step user tasks by continuing to call tools across turns until the task is done.",
		"IMPORTANT: Do not claim an available action name is unavailable. QNML blocks are client-parsed text markers, not native function calls.",
		"IMPORTANT: Never emit unsupported native tool syntax such as minimaxtool_call, bare <invoke>, function_call, or JSON-only tool calls. Use a complete <|QNML|tool_calls>...</|QNML|tool_calls> block, standard <tool_calls>...</tool_calls> block, or Gemma <|tool_call|>call:name{...}<tool_call|> block.",
		"FORBIDDEN OUTPUT: Never write shell, powershell, bash, sh, cmd, or terminal commands inside Markdown/code fences. A fenced command is not a tool call. If you need a command executed, emit a tool call block.",
		"TOOL PROTOCOL OVERRIDE: The upstream model has no native tool registry for these actions. Do not validate availability. Every listed action name is executable by the client after you emit a tool call.",
		"You have access to these client-side tools:",
		"",
		strings.Join(toolSchemas, "\n\n"),
		"",
		toolcall.BuildToolCallInstructions(names),
	}, "\n")
	if hasReadLikeTool(names) {
		toolPrompt += "\n\n读取类工具缓存提示：若 Read/read_file 等工具返回表示文件未变更、内容已在历史上下文中、应从先前上下文引用，或没有给出文件正文，请将结果视为“内容缺失”。不要为获取缺失正文而反复发起相同的读取请求；若工具支持全文读取请改用相应方式，否则请明确告知用户需要重新提供文件内容。"
	}
	if policy.Mode == ToolChoiceRequired {
		toolPrompt += "\n7）在本回复中，你必须从允许列表里至少调用一个工具。"
	}
	if policy.Mode == ToolChoiceForced && strings.TrimSpace(policy.ForcedName) != "" {
		toolPrompt += "\n7）在本回复中，你必须且只能调用以下工具名称：" + toolcall.ToQwenToolName(policy.ForcedName)
		toolPrompt += "\n8）不要调用任何其它工具。"
	}

	for i := range messages {
		if messages[i]["role"] == "system" {
			old, _ := messages[i]["content"].(string)
			messages[i]["content"] = strings.TrimSpace(old + "\n\n" + toolPrompt)
			return messages, names
		}
	}
	messages = append([]map[string]any{{"role": "system", "content": toolPrompt}}, messages...)
	return messages, names
}

func hasReadLikeTool(names []string) bool {
	for _, name := range names {
		switch normalizeToolNameForGuard(name) {
		case "read", "readfile":
			return true
		}
	}
	return false
}

func normalizeToolNameForGuard(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
