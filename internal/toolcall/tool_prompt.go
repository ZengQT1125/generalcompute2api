package toolcall

import "strings"

// BuildToolCallInstructions generates the unified tool-calling instruction block
// used by all adapters (OpenAI, Claude, Gemini). It uses attention-optimized
// structure: rules → negative examples → positive examples → anchor.
//
// The toolNames slice should contain the actual tool names available in the
// current request; the function picks real names for examples.
func BuildToolCallInstructions(toolNames []string) string {
	tcO := MarkupPipeOpenTag(MarkupTagToolCalls)
	tcC := MarkupPipeCloseTag(MarkupTagToolCalls)
	ivC := MarkupPipeCloseTag(MarkupTagInvoke)
	paramPH := wrapParameter("PARAMETER_NAME", "<![CDATA[PARAMETER_VALUE]]>")
	invokePH := MarkupPipeInvokeOpen("TOOL_NAME_HERE")
	gemmaExample := buildGemmaToolExample(toolNames)

	return `工具调用格式：

【格式 A — QNML（推荐）】：
` + tcO + `
  ` + invokePH + `
    ` + paramPH + `
  ` + ivC + `
` + tcC + `

【格式 B — 标准 XML（备选，DeepSeek 等模型更易生成）】：
<tool_calls>
  <invoke name="TOOL_NAME_HERE">
    <parameter name="PARAMETER_NAME"><![CDATA[PARAMETER_VALUE]]></parameter>
  </invoke>
</tool_calls>
` + gemmaExample + `

规则：
1）优先使用 QNML 格式（` + tcO + `），也可使用标准 XML 格式（<tool_calls>）或 Gemma 格式（<|tool_call|>），三种均能正常解析。
2）在根节点下放置一个或多个 ` + MarkupPipeOpenTag(MarkupTagInvoke) + `（QNML）或 <invoke>（标准 XML）。
3）工具名称写在 name 属性中：` + MarkupPipeInvokeOpen("TOOL_NAME") + ` 或 <invoke name="TOOL_NAME">。
4）所有字符串值必须使用 <![CDATA[...]]>，包括很短的值；代码、脚本、文件内容、提示词、路径、名称、查询等均适用。
5）每个顶层参数必须写成完整节点，例如：` + wrapParameter("ARG_NAME", "…") + `。
6）对象在参数体内使用嵌套 XML；数组可重复 <item> 子节点。
7）数字、布尔值与 null 使用纯文本，不用 CDATA。
8）只能使用工具 schema 中声明的参数名，禁止臆造字段。
9）不要用 Markdown 代码围栏包裹 XML；不要输出解释性说明、角色标记或内心独白。
10）若调用工具，该工具块的首个非空白字符必须恰好是工具调用标签（` + tcO + `、<tool_calls> 或 <|tool_call|>）。
11）即使随后会闭合关闭标签，也禁止省略开头的根标签。
12）禁止输出 minimaxtool_call 或任何非标准前缀。
13）如果你是 Gemma 系列模型并更容易输出原生工具调用，可使用格式 C，但不要同时输出侦察计划、说明文字或 <|channel>thought 等角色/通道标记。
14）禁止把 shell、powershell、bash、cmd 等命令写成 Markdown 代码围栏；代码围栏不会被视为工具调用。需要执行命令时必须输出工具调用标记块。

参数形态：
- 字符串 => ` + wrapParameter("x", "<![CDATA[value]]>") + `
- 对象 => ` + "<|" + MarkupPipeChannel + "|" + MarkupTagParameter + ` name="x"><field>...</field>` + MarkupPipeCloseTag(MarkupTagParameter) + `
- 数组 => ` + "<|" + MarkupPipeChannel + "|" + MarkupTagParameter + ` name="x"><item>...</item><item>...</item>` + MarkupPipeCloseTag(MarkupTagParameter) + `
- 数字/布尔/null => ` + "<|" + MarkupPipeChannel + "|" + MarkupTagParameter + ` name="x">纯文本` + MarkupPipeCloseTag(MarkupTagParameter) + `

【错误示例 — 禁止如下】：

错误 1 — XML 后夹杂说明文字：
  ` + tcO + `...` + tcC + ` 希望这能帮到你。
错误 2 — Markdown 代码围栏：
  ` + "```" + "xml" + `
  ` + tcO + `...` + tcC + `
  ` + "```" + `
错误 3 — 缺少开头包裹：
  <invoke name="TOOL_NAME">...` + ivC + `
  ` + tcC + `
错误 4 — 把待执行命令写成代码围栏：
  ` + "```" + "powershell" + `
  Get-ChildItem -Force
  ` + "```" + `

请记住：不要用文字描述你要执行的命令——直接输出工具调用标记块（QNML、标准 XML 或 Gemma 格式均可）。

【Thinking 模型注意】（DeepSeek 3.2 等）：
你的推理在思考块中完成，但最终输出必须在 content 中以工具调用格式呈现。
不要在 content 中用自然语言描述命令——直接输出 <tool_calls>、` + tcO + ` 或 <|tool_call|> 标记块。
即使你已在思考中想好了要用的命令，也必须在 content 中输出完整的工具调用块。

` + buildCorrectToolExamples(toolNames)
}

type promptToolExample struct {
	name   string
	params string
}

func buildCorrectToolExamples(toolNames []string) string {
	names := uniqueToolNames(toolNames)
	examples := make([]string, 0, 4)

	if single, ok := firstBasicExample(names); ok {
		examples = append(examples, "示例 A — 单个工具：\n"+renderToolExampleBlock([]promptToolExample{single}))
	}

	if parallel := firstNBasicExamples(names, 2); len(parallel) >= 2 {
		examples = append(examples, "示例 B — 并行两个工具：\n"+renderToolExampleBlock(parallel))
	}

	if nested, ok := firstNestedExample(names); ok {
		examples = append(examples, "示例 C — 含嵌套 XML 参数的工具：\n"+renderToolExampleBlock([]promptToolExample{nested}))
	}

	if script, ok := firstScriptExample(names); ok {
		examples = append(examples, "示例 D — 使用 CDATA 的长脚本（适合代码/脚本）：\n"+renderToolExampleBlock([]promptToolExample{script}))
	}

	if len(examples) == 0 {
		return ""
	}
	return "【正确示例】：\n\n" + strings.Join(examples, "\n\n") + "\n\n"
}

func buildGemmaToolExample(toolNames []string) string {
	example, ok := firstBasicExample(uniqueToolNames(toolNames))
	if !ok {
		return ""
	}
	params := gemmaExampleParams(example.params)
	if params == "" {
		params = "{}"
	}
	return `

【格式 C — Gemma 兼容（Gemma 模型可选）】：
<|tool_call|>call:` + example.name + params + `<tool_call|>
`
}

func gemmaExampleParams(params string) string {
	pairs := []string{}
	for _, line := range strings.Split(params, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nameStart := strings.Index(line, `name="`)
		if nameStart < 0 {
			continue
		}
		nameStart += len(`name="`)
		nameEnd := strings.Index(line[nameStart:], `"`)
		if nameEnd < 0 {
			continue
		}
		name := line[nameStart : nameStart+nameEnd]
		value := ""
		cdataStart := strings.Index(line, "<![CDATA[")
		cdataEnd := strings.LastIndex(line, "]]>")
		if cdataStart >= 0 && cdataEnd > cdataStart {
			value = line[cdataStart+len("<![CDATA[") : cdataEnd]
		} else {
			close := strings.Index(line, ">")
			endTag := strings.LastIndex(line, "</")
			if close >= 0 && endTag > close {
				value = strings.TrimSpace(line[close+1 : endTag])
			}
		}
		if name == "" {
			continue
		}
		pairs = append(pairs, name+": "+quoteGemmaExampleValue(value))
	}
	if len(pairs) == 0 {
		return "{}"
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

func quoteGemmaExampleValue(value string) string {
	if value == "" {
		return `""`
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func uniqueToolNames(toolNames []string) []string {
	names := make([]string, 0, len(toolNames))
	seen := map[string]bool{}
	for _, name := range toolNames {
		name = ToQwenToolName(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func firstBasicExample(names []string) (promptToolExample, bool) {
	for _, name := range names {
		if params, ok := exampleBasicParams(name); ok {
			return promptToolExample{name: name, params: params}, true
		}
	}
	return promptToolExample{}, false
}

func firstNBasicExamples(names []string, count int) []promptToolExample {
	out := make([]promptToolExample, 0, count)
	for _, name := range names {
		if params, ok := exampleBasicParams(name); ok {
			out = append(out, promptToolExample{name: name, params: params})
			if len(out) == count {
				return out
			}
		}
	}
	return out
}

func firstNestedExample(names []string) (promptToolExample, bool) {
	for _, name := range names {
		if params, ok := exampleNestedParams(name); ok {
			return promptToolExample{name: name, params: params}, true
		}
	}
	return promptToolExample{}, false
}

func firstScriptExample(names []string) (promptToolExample, bool) {
	for _, name := range names {
		if params, ok := exampleScriptParams(name); ok {
			return promptToolExample{name: name, params: params}, true
		}
	}
	return promptToolExample{}, false
}

func renderToolExampleBlock(calls []promptToolExample) string {
	var b strings.Builder
	b.WriteString(MarkupPipeOpenTag(MarkupTagToolCalls) + "\n")
	for _, call := range calls {
		b.WriteString("  " + MarkupPipeInvokeOpen(call.name) + "\n")
		b.WriteString(indentPromptParameters(call.params, "    "))
		b.WriteString("\n  " + MarkupPipeCloseTag(MarkupTagInvoke) + "\n")
	}
	b.WriteString(MarkupPipeCloseTag(MarkupTagToolCalls))
	return b.String()
}

func indentPromptParameters(body, indent string) string {
	if strings.TrimSpace(body) == "" {
		return indent + "<|" + MarkupPipeChannel + "|" + MarkupTagParameter + ` name="content">` + MarkupPipeCloseTag(MarkupTagParameter)
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = line
			continue
		}
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

func wrapParameter(name, inner string) string {
	return "<|" + MarkupPipeChannel + "|" + MarkupTagParameter + ` name="` + name + `">` + inner + MarkupPipeCloseTag(MarkupTagParameter)
}

func exampleBasicParams(name string) (string, bool) {
	switch FromQwenToolName(name) {
	case "Read":
		return wrapParameter("file_path", promptCDATA("README.md")), true
	case "Glob":
		return wrapParameter("pattern", promptCDATA("**/*.go")) + "\n" + wrapParameter("path", promptCDATA(".")), true
	case "read_file":
		return wrapParameter("path", promptCDATA("src/main.go")), true
	case "list_files":
		return wrapParameter("path", promptCDATA(".")), true
	case "search_files":
		return wrapParameter("query", promptCDATA("工具调用解析器")), true
	case "Bash", "execute_command":
		return wrapParameter("command", promptCDATA("pwd")), true
	case "exec_command":
		return wrapParameter("cmd", promptCDATA("pwd")), true
	case "Write":
		return wrapParameter("file_path", promptCDATA("notes.txt")) + "\n" + wrapParameter("content", promptCDATA("Hello world")), true
	case "write_to_file":
		return wrapParameter("path", promptCDATA("notes.txt")) + "\n" + wrapParameter("content", promptCDATA("Hello world")), true
	case "Edit":
		return wrapParameter("file_path", promptCDATA("README.md")) + "\n" + wrapParameter("old_string", promptCDATA("foo")) + "\n" + wrapParameter("new_string", promptCDATA("bar")), true
	case "MultiEdit":
		return wrapParameter("file_path", promptCDATA("README.md")) + "\n" + wrapParameter("edits", `<item><old_string>`+promptCDATA("foo")+`</old_string><new_string>`+promptCDATA("bar")+`</new_string></item>`), true
	}
	return "", false
}

func exampleNestedParams(name string) (string, bool) {
	switch FromQwenToolName(name) {
	case "MultiEdit":
		return wrapParameter("file_path", promptCDATA("README.md")) + "\n" + wrapParameter("edits", `<item><old_string>`+promptCDATA("foo")+`</old_string><new_string>`+promptCDATA("bar")+`</new_string></item>`), true
	case "Task":
		return wrapParameter("description", promptCDATA("排查不稳定测试")) + "\n" + wrapParameter("prompt", promptCDATA("运行定向测试并汇总失败原因")), true
	case "ask_followup_question":
		return wrapParameter("question", promptCDATA("你更倾向哪种方案？")) + "\n" + wrapParameter("follow_up", `<item><text>`+promptCDATA("方案 A")+`</text></item><item><text>`+promptCDATA("方案 B")+`</text></item>`), true
	}
	return "", false
}

func exampleScriptParams(name string) (string, bool) {
	scriptCommand := `cat > /tmp/test_escape.sh <<'EOF'
#!/bin/bash
echo 'single "double"'
echo "literal dollar: \$HOME"
EOF
bash /tmp/test_escape.sh`
	scriptContent := `#!/bin/bash
echo 'single "double"'
echo "literal dollar: $HOME"`

	switch FromQwenToolName(name) {
	case "Bash":
		return wrapParameter("command", promptCDATA(scriptCommand)) + "\n" + wrapParameter("description", promptCDATA("测试 Shell 转义")), true
	case "execute_command":
		return wrapParameter("command", promptCDATA(scriptCommand)), true
	case "exec_command":
		return wrapParameter("cmd", promptCDATA(scriptCommand)), true
	case "Write":
		return wrapParameter("file_path", promptCDATA("test_escape.sh")) + "\n" + wrapParameter("content", promptCDATA(scriptContent)), true
	case "write_to_file":
		return wrapParameter("path", promptCDATA("test_escape.sh")) + "\n" + wrapParameter("content", promptCDATA(scriptContent)), true
	}
	return "", false
}

func promptCDATA(text string) string {
	if text == "" {
		return ""
	}
	if strings.Contains(text, "]]>") {
		return "<![CDATA[" + strings.ReplaceAll(text, "]]>", "]]]]><![CDATA[>") + "]]>"
	}
	return "<![CDATA[" + text + "]]>"
}
