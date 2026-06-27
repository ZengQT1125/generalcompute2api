package toolstream

import (
	"strings"

	"generalcompute2api/internal/toolcall"
)

func ProcessChunk(state *State, chunk string, toolNames []string) []Event {
	if state == nil {
		return nil
	}
	if chunk != "" {
		state.pending.WriteString(chunk)
	}
	events := make([]Event, 0, 2)
	if len(state.pendingToolCalls) > 0 {
		events = append(events, Event{ToolCalls: state.pendingToolCalls})
		state.pendingToolRaw = ""
		state.pendingToolCalls = nil
	}

	for {
		if state.capturing {
			if state.pending.Len() > 0 {
				state.capture.WriteString(state.pending.String())
				state.pending.Reset()
			}
			prefix, calls, suffix, ready := consumeToolCapture(state, toolNames)
			if !ready {
				break
			}
			captured := state.capture.String()
			state.capture.Reset()
			state.capturing = false
			state.resetIncrementalToolState()
			if len(calls) > 0 {
				if prefix != "" {
					state.noteText(prefix)
					events = append(events, Event{Content: prefix})
				}
				if suffix != "" {
					state.pending.WriteString(suffix)
				}
				_ = captured
				state.pendingToolCalls = calls
				continue
			}
			if prefix != "" {
				state.noteText(prefix)
				events = append(events, Event{Content: prefix})
			}
			if suffix != "" {
				state.pending.WriteString(suffix)
			}
			continue
		}

		pending := state.pending.String()
		if pending == "" {
			break
		}
		start := findToolSegmentStart(state, pending)
		if start >= 0 {
			prefix := pending[:start]
			if prefix != "" {
				state.noteText(prefix)
				events = append(events, Event{Content: prefix})
			}
			state.pending.Reset()
			state.capture.WriteString(pending[start:])
			state.capturing = true
			state.resetIncrementalToolState()
			continue
		}

		safe, hold := splitSafeContentForToolDetection(state, pending)
		if safe == "" {
			break
		}
		state.pending.Reset()
		state.pending.WriteString(hold)
		state.noteText(safe)
		events = append(events, Event{Content: safe})
	}

	return events
}

func Flush(state *State, toolNames []string) []Event {
	if state == nil {
		return nil
	}
	events := ProcessChunk(state, "", toolNames)
	if len(state.pendingToolCalls) > 0 {
		events = append(events, Event{ToolCalls: state.pendingToolCalls})
		state.pendingToolRaw = ""
		state.pendingToolCalls = nil
	}
	if state.capturing {
		consumedPrefix, consumedCalls, consumedSuffix, ready := consumeToolCapture(state, toolNames)
		if ready {
			if consumedPrefix != "" {
				state.noteText(consumedPrefix)
				events = append(events, Event{Content: consumedPrefix})
			}
			if len(consumedCalls) > 0 {
				events = append(events, Event{ToolCalls: consumedCalls})
			}
			if consumedSuffix != "" {
				state.noteText(consumedSuffix)
				events = append(events, Event{Content: consumedSuffix})
			}
		} else {
			content := state.capture.String()
			if content != "" {
				recovered := toolcall.SanitizeLooseCDATA(content)
				if recovered != content {
					if prefix, calls, suffix, recoveredReady := consumeXMLToolCapture(recovered, toolNames); recoveredReady && len(calls) > 0 {
						if prefix != "" {
							state.noteText(prefix)
							events = append(events, Event{Content: prefix})
						}
						events = append(events, Event{ToolCalls: calls})
						if suffix != "" {
							state.noteText(suffix)
							events = append(events, Event{Content: suffix})
						}
					} else if !hasMalformedExecutableToolMarker(content) {
						// If capture never resolved into a real tool call, release
						// the buffered text instead of swallowing it.
						state.noteText(content)
						events = append(events, Event{Content: content})
					}
				} else if !hasMalformedExecutableToolMarker(content) {
					// If capture never resolved into a real tool call, release the
					// buffered text instead of swallowing it.
					state.noteText(content)
					events = append(events, Event{Content: content})
				}
			}
		}
		state.capture.Reset()
		state.capturing = false
		state.resetIncrementalToolState()
	}
	if state.pending.Len() > 0 {
		content := state.pending.String()
		// If pending never resolved into a real tool call, release it as text.
		if !hasMalformedExecutableToolMarker(content) {
			state.noteText(content)
			events = append(events, Event{Content: content})
		}
		state.pending.Reset()
	}
	return events
}

func splitSafeContentForToolDetection(state *State, s string) (safe, hold string) {
	if s == "" {
		return "", ""
	}
	if xmlIdx := findPartialXMLToolTagStart(s); xmlIdx >= 0 {
		if insideCodeFenceWithState(state, s[:xmlIdx]) {
			return s, ""
		}
		if xmlIdx > 0 {
			return s[:xmlIdx], s[xmlIdx:]
		}
		return "", s
	}
	if holdStart := findPartialMinimaxToolCallStart(s); holdStart >= 0 {
		if insideCodeFenceWithState(state, s[:holdStart]) {
			return s, ""
		}
		if holdStart > 0 {
			return s[:holdStart], s[holdStart:]
		}
		return "", s
	}
	return s, ""
}

func findToolSegmentStart(state *State, s string) int {
	if s == "" {
		return -1
	}
	if start := findMinimaxToolCallStart(state, s); start >= 0 {
		return start
	}
	offset := 0
	for {
		tag, ok := toolcall.FindToolMarkupTagOutsideIgnored(s, offset)
		if !ok {
			return -1
		}
		start := includeDuplicateLeadingLessThan(s, tag.Start)
		if !insideCodeFenceWithState(state, s[:start]) {
			return start
		}
		offset = tag.End + 1
	}
}

func findMinimaxToolCallStart(state *State, s string) int {
	lower := strings.ToLower(s)
	const marker = "minimaxtool_call"
	for offset := 0; offset < len(lower); {
		idx := strings.Index(lower[offset:], marker)
		if idx < 0 {
			return -1
		}
		idx += offset
		lineStart := strings.LastIndexAny(s[:idx], "\r\n") + 1
		if strings.TrimSpace(s[lineStart:idx]) == "" && !insideCodeFenceWithState(state, s[:idx]) {
			return idx
		}
		offset = idx + len(marker)
	}
	return -1
}

func findPartialMinimaxToolCallStart(s string) int {
	lower := strings.ToLower(s)
	const marker = "minimaxtool_call"
	maxLen := minInt(len(lower), len(marker)-1)
	for n := maxLen; n >= 4; n-- {
		tail := lower[len(lower)-n:]
		if strings.HasPrefix(marker, tail) {
			start := len(s) - n
			lineStart := strings.LastIndexAny(s[:start], "\r\n") + 1
			if strings.TrimSpace(s[lineStart:start]) == "" {
				return start
			}
		}
	}
	return -1
}

func includeDuplicateLeadingLessThan(s string, idx int) int {
	for idx > 0 && s[idx-1] == '<' {
		idx--
	}
	return idx
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func consumeToolCapture(state *State, toolNames []string) (prefix string, calls []toolcall.ParsedToolCall, suffix string, ready bool) {
	captured := state.capture.String()
	if captured == "" {
		return "", nil, "", false
	}

	// XML tool call extraction only.
	if xmlPrefix, xmlCalls, xmlSuffix, xmlReady := consumeXMLToolCapture(captured, toolNames); xmlReady {
		return xmlPrefix, xmlCalls, xmlSuffix, true
	}
	if parsed := toolcall.ParseStandaloneToolCallsDetailed(captured, toolNames); len(parsed.Calls) > 0 {
		return "", parsed.Calls, "", true
	}
	// If XML tags are present but block is incomplete, keep buffering.
	if hasOpenXMLToolTag(captured) {
		return "", nil, "", false
	}
	if shouldKeepBareInvokeCapture(captured) {
		return "", nil, "", false
	}
	if hasMalformedExecutableToolMarker(captured) {
		return "", nil, "", false
	}
	return captured, nil, "", true
}
