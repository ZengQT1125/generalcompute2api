package prompt

import "testing"

func TestStringifyToolCallArgumentsPreservesConcatenatedJSON(t *testing.T) {
	got := StringifyToolCallArguments(`{}{"query":"测试工具调用"}`)
	if got != `{}{"query":"测试工具调用"}` {
		t.Fatalf("expected raw concatenated JSON to be preserved, got %q", got)
	}
}

func TestFormatToolCallsForPromptQNML(t *testing.T) {
	got := FormatToolCallsForPrompt([]any{
		map[string]any{
			"id": "call_1",
			"function": map[string]any{
				"name":      "search_web",
				"arguments": map[string]any{"query": "latest"},
			},
		},
	})
	if got == "" {
		t.Fatal("expected non-empty formatted tool calls")
	}
	want := "<|QNML|tool_calls>\n  <|QNML|invoke name=\"u_search_web\">\n    <|QNML|parameter name=\"query\"><![CDATA[latest]]></|QNML|parameter>\n  </|QNML|invoke>\n</|QNML|tool_calls>"
	if got != want {
		t.Fatalf("unexpected formatted tool call markup: %q", got)
	}
}

func TestFormatToolCallsForPromptEscapesXMLEntities(t *testing.T) {
	got := FormatToolCallsForPrompt([]any{
		map[string]any{
			"name":      "search<&>",
			"arguments": `{"q":"a < b && c > d"}`,
		},
	})
	want := "<|QNML|tool_calls>\n  <|QNML|invoke name=\"u_search&lt;&amp;&gt;\">\n    <|QNML|parameter name=\"q\"><![CDATA[a < b && c > d]]></|QNML|parameter>\n  </|QNML|invoke>\n</|QNML|tool_calls>"
	if got != want {
		t.Fatalf("unexpected escaped tool call XML: %q", got)
	}
}

func TestFormatToolCallsForPromptUsesCDATAForMultilineContent(t *testing.T) {
	got := FormatToolCallsForPrompt([]any{
		map[string]any{
			"name": "write_file",
			"arguments": map[string]any{
				"path":    "script.sh",
				"content": "#!/bin/bash\nprintf \"hello\"\n",
			},
		},
	})
	want := "<|QNML|tool_calls>\n  <|QNML|invoke name=\"u_write_file\">\n    <|QNML|parameter name=\"content\"><![CDATA[#!/bin/bash\nprintf \"hello\"\n]]></|QNML|parameter>\n    <|QNML|parameter name=\"path\"><![CDATA[script.sh]]></|QNML|parameter>\n  </|QNML|invoke>\n</|QNML|tool_calls>"
	if got != want {
		t.Fatalf("unexpected multiline cdata tool call XML: %q", got)
	}
}
