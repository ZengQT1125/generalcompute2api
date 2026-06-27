package toolcall

import (
	"encoding/json"
	"regexp"
	"strings"
)

// parseMultiFormatToolCalls 依次通过 Named XML、JSON 标签块、Markdown JSON 块、TextKV 等多种方式尝试解析工具调用
func parseMultiFormatToolCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, false
	}

	// 1. 尝试 Minimax 原生文本块，例如: minimaxtool_call ... <parameter name="command">...</parameter>
	if calls, ok := parseMinimaxToolCalls(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	// 2. 尝试把模型误写出的 shell 代码围栏恢复为客户端 shell 工具调用
	if calls, ok := parseShellFenceToolCalls(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	// 3. 尝试 Named XML 格式 (例如: <tool_call name="xxx">...</tool_call>)
	if calls, ok := parseNamedXMLToolCalls(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	// 4. 尝试从 <tool_calls> 或 <tool_call> 标签中提取 JSON 块解析
	if calls, ok := parseJSONBlocksFromTags(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	// 5. 尝试解析 Markdown 代码围栏内的 JSON 块（或者整体是个 JSON）
	if calls, ok := parseJSONBlockMarkdown(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	// 6. 尝试解析 TextKV 键值对格式
	if calls, ok := parseTextKVToolCalls(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	return nil, false
}

func parseShellFenceToolCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	name := chooseShellFenceToolName(availableToolNames)
	if name == "" {
		return nil, false
	}
	argName := shellFenceCommandArgumentName(name)
	lines := strings.SplitAfter(text, "\n")
	out := make([]ParsedToolCall, 0, 2)
	for i := 0; i < len(lines); i++ {
		marker, ok := parseShellFenceOpenLine(lines[i])
		if !ok {
			continue
		}
		var body strings.Builder
		closed := false
		for j := i + 1; j < len(lines); j++ {
			if isFenceClose(strings.TrimLeft(strings.TrimRight(lines[j], "\r\n"), " \t"), marker) {
				i = j
				closed = true
				break
			}
			body.WriteString(lines[j])
		}
		if !closed {
			continue
		}
		command := strings.TrimSpace(body.String())
		if command == "" {
			continue
		}
		out = append(out, ParsedToolCall{
			Name: name,
			Input: map[string]any{
				argName: command,
			},
		})
	}
	return out, len(out) > 0
}

func chooseShellFenceToolName(availableToolNames []string) string {
	for _, want := range []string{"shell", "Bash", "shell_run", "execute_command", "exec_command", "PowerShell", "terminal"} {
		if name := allowedToolName(want, availableToolNames); name != "" {
			return name
		}
	}
	return ""
}

func shellFenceCommandArgumentName(name string) string {
	if toolNameKey(FromQwenToolName(name)) == "execcommand" {
		return "cmd"
	}
	return "command"
}

func parseShellFenceOpenLine(line string) (string, bool) {
	trimmed := strings.TrimLeft(strings.TrimRight(line, "\r\n"), " \t")
	marker, ok := parseFenceOpen(trimmed)
	if !ok {
		return "", false
	}
	rest := strings.TrimSpace(trimmed[len(marker):])
	if rest == "" {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	if !isShellFenceLanguage(fields[0]) {
		return "", false
	}
	return marker, true
}

func isShellFenceLanguage(lang string) bool {
	lang = strings.ToLower(strings.Trim(strings.TrimSpace(lang), "{}"))
	switch lang {
	case "powershell", "pwsh", "ps1", "shell", "bash", "sh", "zsh", "cmd", "bat", "terminal":
		return true
	default:
		return false
	}
}

var minimaxToolCallBlockRe = regexp.MustCompile(`(?is)(?:^|[\r\n])\s*<?minimaxtool_call>?\s*(.*?)</minimaxtool_call\s*>`)

func parseMinimaxToolCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	if !strings.Contains(strings.ToLower(text), "minimaxtool_call") {
		return nil, false
	}
	matches := minimaxToolCallBlockRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, false
	}
	name := chooseMinimaxToolName(availableToolNames)
	if name == "" {
		return nil, false
	}
	out := make([]ParsedToolCall, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		input := parseMinimaxParameters(match[1])
		if len(input) == 0 {
			continue
		}
		out = append(out, ParsedToolCall{Name: name, Input: input})
	}
	return out, len(out) > 0
}

func chooseMinimaxToolName(availableToolNames []string) string {
	preferred := []string{"shell", "Bash", "shell_run", "execute_command", "exec_command", "PowerShell"}
	for _, want := range preferred {
		if name := allowedToolName(want, availableToolNames); name != "" {
			return name
		}
	}
	if len(availableToolNames) == 1 {
		return allowedToolName(availableToolNames[0], availableToolNames)
	}
	return ""
}

func parseMinimaxParameters(body string) map[string]any {
	input := map[string]any{}
	for _, paramMatch := range findXMLElementBlocks(body, "parameter") {
		paramAttrs := parseXMLTagAttributes(paramMatch.Attrs)
		paramName := strings.TrimSpace(paramAttrs["name"])
		if paramName == "" {
			continue
		}
		input[paramName] = parseInvokeParameterValue(paramName, paramMatch.Body)
	}
	return input
}

// allowedToolName 根据允许调用的工具名列表校验工具名
func allowedToolName(name string, availableToolNames []string) string {
	name = canonicalToolName(name, availableToolNames)
	if name == "" {
		return ""
	}
	return name
}

var cdataRe = regexp.MustCompile(`(?i)^<!\[CDATA\[([\s\S]*?)\]\]>$`)

// decodeToolText 解除 CDATA 的包裹，提取文本
func decodeToolText(value string) string {
	trimmed := strings.TrimSpace(value)
	if match := cdataRe.FindStringSubmatch(trimmed); len(match) > 1 {
		return match[1]
	}
	return trimmed
}

// parseJSONArguments 尝试将参数解析为键值对 Map，如果解析失败或不是 Object，则将其作为 content 字段的值
func parseJSONArguments(body string) map[string]any {
	body = strings.TrimSpace(body)
	if body == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err == nil {
		return m
	}
	// 尝试利用已有的 RepairLooseJSON 辅助方法进行容错性解析
	repaired := RepairLooseJSON(body)
	if err := json.Unmarshal([]byte(repaired), &m); err == nil {
		return m
	}
	// 如果不是合法的 JSON Object，将整个字符串存入 map["content"] 返回
	return map[string]any{"content": body}
}

var namedXMLRe = regexp.MustCompile(`(?i)<tool_call\s+name=["']([^"']+)["'][^>]*>([\s\S]*?)<\/tool_call>`)

// parseNamedXMLToolCalls 解析 <tool_call name="xxx">...</tool_call> 风格的 XML 节点
func parseNamedXMLToolCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	matches := namedXMLRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, false
	}
	var out []ParsedToolCall
	for _, match := range matches {
		name := allowedToolName(match[1], availableToolNames)
		if name == "" {
			continue
		}
		body := decodeToolText(match[2])
		args := parseJSONArguments(body)
		out = append(out, ParsedToolCall{
			Name:  name,
			Input: args,
		})
	}
	return out, len(out) > 0
}

// extractJsonBlock 截取最内层指定的 XML 块内容
func extractJsonBlock(text, tag string) string {
	open := "<" + tag
	closeTag := "</" + tag + ">"
	lowerText := strings.ToLower(text)
	lastClose := strings.LastIndex(lowerText, closeTag)
	if lastClose == -1 {
		return ""
	}
	lastOpen := strings.LastIndex(lowerText[:lastClose], open)
	if lastOpen == -1 {
		return ""
	}
	inner := text[lastOpen+len(open) : lastClose]
	gt := strings.Index(inner, ">")
	var body string
	if gt == -1 {
		body = inner
	} else {
		body = inner[gt+1:]
	}
	return strings.TrimSpace(body)
}

// parseJSONBlocksFromTags 支持从 <tool_calls> 或 <tool_call> 标签内提取 JSON 并做多层转换
func parseJSONBlocksFromTags(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	blocks := []string{
		extractJsonBlock(text, "tool_calls"),
		extractJsonBlock(text, "tool_call"),
	}
	var out []ParsedToolCall
	for _, block := range blocks {
		if block == "" {
			continue
		}
		calls, ok := parseJSONToolCalls([]byte(block), availableToolNames)
		if ok && len(calls) > 0 {
			out = append(out, calls...)
		}
	}
	return out, len(out) > 0
}

// parseJSONToolCalls 反序列化 JSON 块，并递归提取其中的工具调用节点
func parseJSONToolCalls(data []byte, availableToolNames []string) ([]ParsedToolCall, bool) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		repaired := RepairLooseJSON(string(data))
		if err := json.Unmarshal([]byte(repaired), &raw); err != nil {
			return nil, false
		}
	}
	return parseJSONToolCallsValue(raw, availableToolNames)
}

// parseJSONToolCallsValue 递归解析 JSON 结构中的单节点或数组，以兼容 nested 的 tools/tool_calls 等多种结构
func parseJSONToolCallsValue(val any, availableToolNames []string) ([]ParsedToolCall, bool) {
	if val == nil {
		return nil, false
	}
	switch x := val.(type) {
	case []any:
		var out []ParsedToolCall
		for _, item := range x {
			if calls, ok := parseJSONToolCallsValue(item, availableToolNames); ok {
				out = append(out, calls...)
			}
		}
		return out, len(out) > 0
	case map[string]any:
		// 递归处理嵌套字段
		for _, key := range []string{"tool_calls", "tools", "calls", "function_call"} {
			if nested, ok := x[key]; ok {
				if calls, ok := parseJSONToolCallsValue(nested, availableToolNames); ok {
					return calls, true
				}
			}
		}

		// 提取当前的 function 节点
		fn := x
		if f, ok := x["function"].(map[string]any); ok {
			fn = f
		}

		var name string
		for _, key := range []string{"name", "tool", "tool_name", "function_name"} {
			if n, ok := fn[key].(string); ok && strings.TrimSpace(n) != "" {
				name = strings.TrimSpace(n)
				break
			}
		}

		name = allowedToolName(name, availableToolNames)
		if name == "" {
			return nil, false
		}

		var args map[string]any
		rawArgs := fn["arguments"]
		if rawArgs == nil {
			rawArgs = fn["args"]
		}
		if rawArgs == nil {
			rawArgs = fn["input"]
		}
		if rawArgs == nil {
			rawArgs = fn["parameters"]
		}
		if rawArgs == nil {
			rawArgs = x["arguments"]
		}

		if rawArgs != nil {
			switch a := rawArgs.(type) {
			case map[string]any:
				args = a
			case string:
				args = parseJSONArguments(a)
			}
		}

		if args == nil {
			args = map[string]any{}
		}

		return []ParsedToolCall{{
			Name:  name,
			Input: args,
		}}, true
	}
	return nil, false
}

// parseJSONBlockMarkdown 支持从 Markdown 围栏 ```json 内提取或将整个文本视作 JSON 解析
func parseJSONBlockMarkdown(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 2 {
			if strings.HasPrefix(lines[0], "```") {
				lines = lines[1:]
			}
			if len(lines) > 0 && strings.HasSuffix(strings.TrimSpace(lines[len(lines)-1]), "```") {
				lines = lines[:len(lines)-1]
			}
			trimmed = strings.Join(lines, "\n")
		}
	}
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return nil, false
	}

	// 安全校验：对于没有 XML 标签修饰的 Markdown JSON 块，
	// 如果顶层是 "tool_calls" / "tools" / "calls" 这种数组结构的 JSON，
	// 我们认为它是普通对话在展示工具调用示例，而不是真正的工具调用，从而忽略它。
	var raw any
	if err := json.Unmarshal([]byte(trimmed), &raw); err == nil {
		if m, ok := raw.(map[string]any); ok {
			for _, key := range []string{"tool_calls", "tools", "calls"} {
				if _, exists := m[key]; exists {
					return nil, false
				}
			}
		}
	} else {
		repaired := RepairLooseJSON(trimmed)
		if err2 := json.Unmarshal([]byte(repaired), &raw); err2 == nil {
			if m, ok := raw.(map[string]any); ok {
				for _, key := range []string{"tool_calls", "tools", "calls"} {
					if _, exists := m[key]; exists {
						return nil, false
					}
				}
			}
		} else {
			return nil, false
		}
	}

	return parseJSONToolCallsValue(raw, availableToolNames)
}

var fnNameRe = regexp.MustCompile(`(?i)function\.name\s*:\s*([^\n\r]+)`)
var fnArgsRe = regexp.MustCompile(`(?i)function\.arguments\s*:\s*([\s\S]+)`)

// parseTextKVToolCalls 支持解析 "function.name: xxx \n function.arguments: yyy" 风格的键值对形式
func parseTextKVToolCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	nameMatch := fnNameRe.FindStringSubmatch(text)
	if len(nameMatch) < 2 {
		return nil, false
	}
	name := allowedToolName(nameMatch[1], availableToolNames)
	if name == "" {
		return nil, false
	}

	var args map[string]any
	argsMatch := fnArgsRe.FindStringSubmatch(text)
	if len(argsMatch) >= 2 {
		args = parseJSONArguments(argsMatch[1])
	} else {
		args = map[string]any{}
	}

	return []ParsedToolCall{{
		Name:  name,
		Input: args,
	}}, true
}
