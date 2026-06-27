package toolcall

// Pipe-style QNML tool markup. Legacy ZJML/DSML and <tool_calls> remain
// accepted by the scanner, but Codex-facing prompts prefer qwen2API's shape.
const (
	MarkupPipeChannel = "QNML"

	MarkupTagToolCalls = "tool_calls"
	MarkupTagInvoke    = "invoke"
	MarkupTagParameter = "parameter"
)

func MarkupPipeOpenTag(localName string) string {
	return "<|" + MarkupPipeChannel + "|" + localName + ">"
}

func MarkupPipeCloseTag(localName string) string {
	return "</|" + MarkupPipeChannel + "|" + localName + ">"
}

// MarkupPipeInvokeOpen renders <|QNML|invoke name="toolName">.
func MarkupPipeInvokeOpen(toolName string) string {
	return "<|" + MarkupPipeChannel + "|" + MarkupTagInvoke + ` name="` + toolName + `">`
}
