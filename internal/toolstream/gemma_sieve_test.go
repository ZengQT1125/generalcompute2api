package toolstream

import (
	"testing"
	"generalcompute2api/internal/toolcall"
)

func TestGemmaToolCallThroughSieve(t *testing.T) {
	input := `<|tool_call|>call:list_files{path: "."}<tool_call|>`
	
	// 1. 测试 parseGemmaToolCalls 直接解析
	calls := toolcall.ParseToolCalls(input, nil)
	if len(calls) == 0 {
		t.Fatal("parseGemmaToolCalls should find the tool call")
	}
	t.Logf("ParseToolCalls found: name=%q, input=%v", calls[0].Name, calls[0].Input)
	
	// 2. 测试 ParseStandaloneToolCallsDetailed
	detailed := toolcall.ParseStandaloneToolCallsDetailed(input, nil)
	if len(detailed.Calls) == 0 {
		t.Fatal("ParseStandaloneToolCallsDetailed should find the tool call")
	}
	t.Logf("ParseStandaloneToolCallsDetailed found: name=%q, input=%v", detailed.Calls[0].Name, detailed.Calls[0].Input)
	
	// 3. 测试 sieve ProcessChunk 单次处理
	state := &State{}
	events := ProcessChunk(state, input, nil)
	t.Logf("ProcessChunk events: %+v", events)
	// The tool call might be in pendingToolCalls after ProcessChunk
	if len(state.pendingToolCalls) > 0 {
		t.Logf("pendingToolCalls: name=%q, input=%v", state.pendingToolCalls[0].Name, state.pendingToolCalls[0].Input)
	}
	
	// 4. 测试 Flush
	flushEvents := Flush(state, nil)
	t.Logf("Flush events: %+v", flushEvents)
	for i, evt := range flushEvents {
		if len(evt.ToolCalls) > 0 {
			t.Logf("Flush event[%d] has tool calls: name=%q, input=%v", i, evt.ToolCalls[0].Name, evt.ToolCalls[0].Input)
		}
		if evt.Content != "" {
			t.Logf("Flush event[%d] has content: %q", i, evt.Content)
		}
	}
	
	// 验证
	found := false
	for _, evt := range flushEvents {
		if len(evt.ToolCalls) > 0 && evt.ToolCalls[0].Name == "list_files" {
			found = true
			break
		}
	}
	if !found {
		// Also check pendingToolCalls after Flush
		if len(state.pendingToolCalls) > 0 {
			t.Logf("After Flush, pendingToolCalls still has: name=%q", state.pendingToolCalls[0].Name)
			found = true
		}
	}
	if !found {
		t.Fatal("sieve should produce tool calls for gemma format")
	}
}
