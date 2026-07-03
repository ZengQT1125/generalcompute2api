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

	// 5.5 尝试裸工具名 XML 格式
		if calls, ok := parseBareToolNameXMLCalls(trimmed, availableToolNames); ok && len(calls) > 0 {
		return calls, true
	}

	// 5.6 尝试 Gemma 4 的 <|tool_call|>call:name{...}<tool_call|> 格式
	if calls, ok := parseGemmaToolCalls(trimmed, availableToolNames); ok && len(calls) > 0 {
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


// parseBareToolNameXMLCalls 处理模型直接以工具名为 XML 标签名输出的格式：
//
//	<read_file>
//	  <path>G:\xxx</path>
//	</read_file>
//
// 每个顶层工具标签的直系子标签作为参数名/值处理。
func parseBareToolNameXMLCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	if len(availableToolNames) == 0 {
		return nil, false
	}
	// 对每个可用工具名，生成可能被模型用作 XML 标签的变体
	tagCandidates := make([]string, 0, len(availableToolNames)*3)
	for _, name := range availableToolNames {
		if name == "" || name == "__any_tool__" {
			continue
		}
		tagCandidates = append(tagCandidates, name)
		// 也尝试 Qwen 别名
		if alias := ToQwenToolName(name); alias != name {
			tagCandidates = append(tagCandidates, alias)
		}
		// 也尝试反向解析后的规范名
		if canonical := FromQwenToolName(name); canonical != name && canonical != "" {
			tagCandidates = append(tagCandidates, canonical)
		}
	}
	tagCandidates = uniqueStrings(tagCandidates)

	type foundBlock struct {
		tag  string
		body string
	}
	var blocks []foundBlock

	for _, tag := range tagCandidates {
		for _, block := range findXMLElementBlocks(text, tag) {
			blocks = append(blocks, foundBlock{tag: tag, body: block.Body})
		}
	}
	if len(blocks) == 0 {
		return nil, false
	}

	out := make([]ParsedToolCall, 0, len(blocks))
	for _, block := range blocks {
		name := block.tag
		// 校验工具名是否在允许列表中
		name = allowedToolName(name, availableToolNames)
		if name == "" {
			continue
		}

		input := parseBareXMLChildrenAsParams(block.body)
		if input == nil {
			input = map[string]any{}
		}
		// 如果 body 本身就是合法的 JSON，尝试直接解析
		if strings.TrimSpace(block.body) != "" && len(input) == 0 {
			if parsed := parseJSONArguments(block.body); len(parsed) > 0 {
				if content, ok := parsed["content"]; ok && len(parsed) == 1 {
					// 单个 content 字段说明不是标准 JSON 对象，保留为空
					_ = content
				} else {
					input = parsed
				}
			}
		}
		out = append(out, ParsedToolCall{Name: name, Input: input})
	}
	return out, len(out) > 0
}

// parseBareXMLChildrenAsParams 提取 <toolName><param1>val1</param1><param2>val2</param2></toolName> 中的子节点作为参数
func parseBareXMLChildrenAsParams(body string) map[string]any {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return map[string]any{}
	}

	out := map[string]any{}
	// 使用已有的 findXMLElementBlocks 提取直系子标签
	// 注意：XML 解码器可能把嵌套结构展平，我们先处理一级子标签
	for {
		tag, ok := findFirstXMLTag(trimmed)
		if !ok {
			break
		}
		block := findXMLBlockByTag(trimmed, tag)
		if block == "" {
			break
		}
		// 提取标签内的文本内容
		inner := extractXMLBlockBody(block)
		trimmed = removeFirstXMLBlock(trimmed)
		if inner == "" {
			continue
		}
		// 处理 CDATA
		if cdataValue, ok := extractStandaloneCDATA(inner); ok {
			out[tag] = cdataValue
		} else {
			out[tag] = inner
		}
	}
	return out
}

// findFirstXMLTag 找到文本中第一个 XML 开始标签名
func findFirstXMLTag(text string) (string, bool) {
	lower := strings.ToLower(text)
	for i := 0; i < len(text); i++ {
		if text[i] != '<' {
			continue
		}
		if i+1 < len(text) && text[i+1] == '/' {
			continue // 跳过闭合标签
		}
		start := i + 1
		end := start
		for end < len(text) && text[end] != '>' && text[end] != ' ' && text[end] != '/' && text[end] != '\t' && text[end] != '\n' && text[end] != '\r' {
			end++
		}
		if end > start && end < len(text) && text[end] == '>' {
			_ = lower
			return text[start:end], true
		}
		// 有属性的标签
		if end > start {
			return text[start:end], true
		}
	}
	return "", false
}

// findXMLBlockByTag 提取第一个匹配指定标签名的完整 XML 块（包含子标签）
func findXMLBlockByTag(text, tag string) string {
	lower := strings.ToLower(text)
	targetStart := "<" + strings.ToLower(tag)
	closeTag := "</" + strings.ToLower(tag) + ">"

	startIdx := strings.Index(lower, targetStart)
	if startIdx < 0 {
		return ""
	}
	// 找到结束 >
	gtIdx := strings.Index(text[startIdx:], ">")
	if gtIdx < 0 {
		return ""
	}
	bodyStart := startIdx + gtIdx + 1

	// 查找匹配的闭合标签，考虑嵌套
	depth := 1
	searchFrom := bodyStart
	for depth > 0 && searchFrom < len(text) {
		nextOpen := strings.Index(strings.ToLower(text[searchFrom:]), targetStart)
		nextClose := strings.Index(strings.ToLower(text[searchFrom:]), closeTag)

		if nextClose < 0 {
			return "" // 没有闭合标签
		}
		if nextOpen >= 0 && nextOpen < nextClose {
			depth++
			searchFrom += nextOpen + len(targetStart)
		} else {
			depth--
			if depth == 0 {
				blockEnd := searchFrom + nextClose + len(closeTag)
				return text[startIdx:blockEnd]
			}
			searchFrom += nextClose + len(closeTag)
		}
	}
	return ""
}

// extractXMLBlockBody 从 <tag>body</tag> 中提取 body 部分
func extractXMLBlockBody(block string) string {
	gtIdx := strings.Index(block, ">")
	if gtIdx < 0 {
		return ""
	}
	closeTag := "</"
	closeIdx := strings.LastIndex(block, closeTag)
	if closeIdx < 0 || closeIdx <= gtIdx+1 {
		return ""
	}
	return block[gtIdx+1 : closeIdx]
}

// removeFirstXMLBlock 移除文本中第一个 XML 块
func removeFirstXMLBlock(text string) string {
	// 找第一个 <tag> 
	start := strings.Index(text, "<")
	if start < 0 {
		return ""
	}
	// 如果是闭合标签跳过
	if start+1 < len(text) && text[start+1] == '/' {
		nextStart := strings.Index(text[start+1:], "<")
		if nextStart >= 0 {
			return removeFirstXMLBlock(text[start+1+nextStart:])
		}
		return ""
	}
	// 找闭合标签
	gtIdx := strings.Index(text[start:], ">")
	if gtIdx < 0 {
		return ""
	}
	// 检查是否有属性
	tagEnd := start + gtIdx
	tagNameStart := start + 1
	tagNameEnd := tagNameStart
	for tagNameEnd < tagEnd && text[tagNameEnd] != ' ' && text[tagNameEnd] != '/' && text[tagNameEnd] != '\t' && text[tagNameEnd] != '\n' && text[tagNameEnd] != '\r' {
		tagNameEnd++
	}
	tagName := text[tagNameStart:tagNameEnd]
	closeTag := "</" + tagName + ">"
	lower := strings.ToLower(text)
	lowerClose := strings.ToLower(closeTag)
	closeIdx := strings.Index(lower[start:], lowerClose)
	if closeIdx < 0 {
		return ""
	}
	blockEnd := start + closeIdx + len(closeTag)
	if blockEnd >= len(text) {
		return ""
	}
	return text[blockEnd:]
}

// uniqueStrings 去重字符串切片
func uniqueStrings(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}



// parseGemmaToolCalls 处理 Gemma 4 的 <|tool_call|>call:name{...}<tool_call|> 格式。
// 这种格式的闭标签 <tool_call|> 没有 "/" 前缀，标准 XML/DSML 解析器无法识别。
var gemmaToolCallBlockPattern = regexp.MustCompile(`(?is)<\|tool_call\|?>\s*call\s*:\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(\{(?:[^{}]|\{[^{}]*\})*\})\s*<tool_call\|>`)

func parseGemmaToolCalls(text string, availableToolNames []string) ([]ParsedToolCall, bool) {
	if !strings.Contains(text, "<|tool_call") {
		return nil, false
	}
	matches := gemmaToolCallBlockPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, false
	}
	out := make([]ParsedToolCall, 0, len(matches))
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		argsRaw := strings.TrimSpace(m[2])
		input := parseGemmaArgs(argsRaw)
		out = append(out, ParsedToolCall{Name: name, Input: input})
	}
	return out, len(out) > 0
}

func parseGemmaArgs(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return map[string]any{}
	}
	// 1. 尝试标准 JSON 解析
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed != nil {
		return parsed
	}
	// 2. JSON 解析失败（常见原因是 key 没有引号），用简单键值对提取
	inner := strings.TrimPrefix(strings.TrimSuffix(raw, "}"), "{")
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return map[string]any{}
	}
	out := map[string]any{}
	// 用正则解析 key: value 对
	kvPattern := regexp.MustCompile("(?s)([a-zA-Z_][a-zA-Z0-9_]*)\\s*:\\s*(\"(?:[^\"\\\\]|\\\\.)*\"|'[^']*'|\\d+(?:\\.\\d+)?|true|false|null|\\{(?:[^{}]|\\{[^{}]*\\})*\\}|\\[(?:[^\\[\\]]|\\[[^\\[\\]]*\\])*\\])")
	for _, kv := range kvPattern.FindAllStringSubmatch(inner, -1) {
		if len(kv) < 3 {
			continue
		}
		key := strings.TrimSpace(kv[1])
		valStr := strings.TrimSpace(kv[2])
		if key == "" {
			continue
		}
		out[key] = parseGemmaValue(valStr)
	}
	return out
}

func parseGemmaValue(valStr string) any {
	// 字符串（带引号）
	if (strings.HasPrefix(valStr, "\"") && strings.HasSuffix(valStr, "\"")) ||
		(strings.HasPrefix(valStr, "'") && strings.HasSuffix(valStr, "'")) {
		s := valStr[1 : len(valStr)-1]
		s = strings.ReplaceAll(s, "\\\"", "\"")
		s = strings.ReplaceAll(s, "\\'", "'")
		s = strings.ReplaceAll(s, "\\\\", "\\")
		return s
	}
	// null
	if valStr == "null" {
		return nil
	}
	// 布尔
	if valStr == "true" {
		return true
	}
	if valStr == "false" {
		return false
	}
	// 数字
	var num float64
	if err := json.Unmarshal([]byte(valStr), &num); err == nil {
		return num
	}
	// 对象/数组保持原始字符串
	return valStr
}

