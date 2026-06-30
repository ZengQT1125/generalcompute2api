package toolcall

import (
	"regexp"
	"strings"
)

var qwenToolNameAliases = map[string]string{
	"Read":         "fs_open_file",
	"Write":        "fs_put_file",
	"Edit":         "fs_patch_file",
	"Bash":         "shell_run",
	"Grep":         "text_search",
	"Glob":         "path_find",
	"NotebookEdit": "notebook_patch",
	"WebFetch":     "http_get_url",
	"WebSearch":    "web_query",
}

var qwenReverseToolNameAliases = func() map[string]string {
	out := make(map[string]string, len(qwenToolNameAliases))
	for raw, alias := range qwenToolNameAliases {
		out[strings.ToLower(alias)] = raw
	}
	return out
}()

func ToQwenToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if alias, ok := qwenToolNameAliases[name]; ok {
		return alias
	}
	if _, ok := qwenReverseToolNameAliases[strings.ToLower(name)]; ok {
		return name
	}
	if strings.HasPrefix(name, "u_") {
		return name
	}
	return "u_" + name
}

func FromQwenToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if raw, ok := qwenReverseToolNameAliases[strings.ToLower(name)]; ok {
		return raw
	}
	if strings.HasPrefix(name, "u_") && len(name) > 2 {
		return strings.TrimPrefix(name, "u_")
	}
	return name
}

func canonicalizeParsedToolCalls(calls []ParsedToolCall, availableToolNames []string) []ParsedToolCall {
	if len(calls) == 0 {
		return calls
	}
	out := make([]ParsedToolCall, 0, len(calls))
	for _, call := range calls {
		call.Name = canonicalToolName(call.Name, availableToolNames)
		if call.Name == "" {
			continue
		}
		out = append(out, call)
	}
	return out
}

func canonicalToolName(name string, availableToolNames []string) string {
	name = FromQwenToolName(name)
	if strings.TrimSpace(name) == "" {
		return ""
	}
	if len(availableToolNames) == 0 {
		return name
	}
	for _, candidate := range availableToolNames {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "__any_tool__" {
			return name
		}
		if toolNameKey(candidate) == toolNameKey(name) {
			return name
		}
		if toolNameKey(ToQwenToolName(candidate)) == toolNameKey(name) {
			return candidate
		}
	}
	return ""
}

func toolNameKey(name string) string {
	return regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "")
}

// ToolNameKey returns a normalized key for tool name matching (exported version).
func ToolNameKey(name string) string {
	return toolNameKey(name)
}
