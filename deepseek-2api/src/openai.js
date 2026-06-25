import { completion, parseSSEStream } from './chat.js';
import { resolveImageToRefId } from './upload.js';
import { enqueueRequest, dispatchQueued } from './queue.js';
import { recordTTFB, recordTokenSpeed } from './metrics.js';
import { getConversationId, resolveConversation, recordResponseMessageId } from './conversation.js';

// Flush SSE data immediately - prevents buffering in Node.js, nginx, and Cloudflare
function flushSSE(res) {
  if (res.flush) res.flush();
  else if (res._flush) res._flush();
  const socket = res.socket || res._socket;
  if (socket && typeof socket.setNoDelay === "function") socket.setNoDelay(true);
}

const MODEL_MAP = {
  'deepseek-v4-flash': 'default',
  'deepseek-v4-pro': 'expert',
  'deepseek-v4-vision': 'vision',
  'deepseek-v4-flash[1m]': 'default',
  'deepseek-v4-pro[1m]': 'expert',
  'deepseek-v4-vision[1m]': 'vision',
};

function mapModel(model) {
  const mapped = MODEL_MAP[model];
  if (!mapped) throw new Error(`Unknown model: ${model}. Available: ${Object.keys(MODEL_MAP).join(', ')}`);
  return mapped;
}

async function extractImages(messages, token) {
  const refFileIds = [];
  for (const msg of messages) {
    if (Array.isArray(msg.content)) {
      for (const part of msg.content) {
        if (part.type === 'image_url' && part.image_url?.url) {
          try {
            const fileId = await resolveImageToRefId(part.image_url.url, token);
            refFileIds.push(fileId);
          } catch (err) {
            console.error('Image upload failed:', err.message);
          }
        }
      }
    }
  }
  return refFileIds;
}

function textFromContent(content) {
  if (content == null) return '';
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content.map(part => {
      if (part.type === 'text') return part.text || '';
      if (part.type === 'image_url') return '[Image]';
      return JSON.stringify(part);
    }).filter(Boolean).join('\n');
  }
  return JSON.stringify(content);
}

function normalizeTools(tools) {
  if (!Array.isArray(tools)) return [];
  return tools
    .map(tool => tool?.type === 'function' ? tool.function : tool)
    .filter(fn => fn?.name)
    .map(fn => ({
      type: 'function',
      function: {
        name: fn.name,
        description: fn.description || '',
        parameters: fn.parameters || fn.input_schema || { type: 'object', properties: {} },
      },
    }));
}

function toolChoiceInstruction(toolChoice, tools) {
  if (!toolChoice || toolChoice === 'auto') return 'Use a tool only when it is helpful or required to answer correctly.';
  if (toolChoice === 'required') return 'You must call at least one tool.';
  if (toolChoice === 'none') return 'Do not call tools.';
  const forcedName = toolChoice?.function?.name;
  if (forcedName && tools.some(t => t.function.name === forcedName)) {
    return `You must call the function named ${forcedName}.`;
  }
  return 'Use a tool only when it is helpful or required to answer correctly.';
}

function buildToolInstructions(tools, toolChoice) {
  const normalized = normalizeTools(tools);
  if (!normalized.length || toolChoice === 'none') return '';

  const toolSchemas = normalized.map(tool => {
    const fn = tool.function;
    return `Action name: ${fn.name}\nDescription: ${fn.description || '(none)'}\nParameters: ${JSON.stringify(fn.parameters || { type: 'object', properties: {} })}`;
  }).join('\n\n');
  const names = normalized.map(t => t.function.name).join(', ');

  return `\n\n=== CLIENT TOOL CALL PROTOCOL ===
The upstream model has no native tool registry. These action names are client-side tools and are valid when listed here.
Available action names: ${names}

${toolSchemas}

${toolChoiceInstruction(toolChoice, normalized)}

If a tool is needed, do not answer normally. Output exactly one QNML block and nothing else:
<|QNML|tool_calls>
  <|QNML|invoke name="TOOL_NAME">
    <|QNML|parameter name="ARG"><![CDATA[value]]></|QNML|parameter>
  </|QNML|invoke>
</|QNML|tool_calls>

Rules:
- Use exact action names and exact parameter names from the schemas.
- Put one or more <|QNML|invoke> nodes inside the wrapper.
- Use <![CDATA[...]]> for string values; for objects or arrays, put valid JSON inside the parameter.
- Never emit empty required parameters. If required information is missing, ask normally.
- If no tool is needed, answer normally without a QNML block.
- Do not say a listed action is unavailable; emit QNML when you choose to call it.
=== END CLIENT TOOL CALL PROTOCOL ===`;
}

function buildPrompt(messages, tools = [], toolChoice = 'auto') {
  let prompt = '';
  for (const msg of messages) {
    if (msg.role === 'system') {
      prompt += `[System]: ${textFromContent(msg.content)}\n\n`;
    } else if (msg.role === 'user') {
      prompt += `[User]: ${textFromContent(msg.content)}\n\n`;
    } else if (msg.role === 'assistant') {
      const content = textFromContent(msg.content);
      if (content) prompt += `[Assistant]: ${content}\n\n`;
      if (Array.isArray(msg.tool_calls) && msg.tool_calls.length) {
        prompt += `[Assistant tool calls]: ${JSON.stringify(msg.tool_calls)}\n\n`;
      }
    } else if (msg.role === 'tool') {
      const name = msg.name || msg.tool_call_id || 'tool';
      prompt += `[Tool result ${name}]: ${textFromContent(msg.content)}\n\n`;
    } else if (msg.role === 'function') {
      prompt += `[Function result ${msg.name || 'function'}]: ${textFromContent(msg.content)}\n\n`;
    }
  }
  return (prompt.trim() + buildToolInstructions(tools, toolChoice)).trim();
}

// Affinity-mode prompt: send only the newest user turn, or the newest tool
// result turn after a client-side tool call. DeepSeek keeps prior turns
// server-side via parent_message_id.
function buildLatestPrompt(messages, tools = [], toolChoice = 'auto') {
  const trailingToolMessages = [];
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role === 'tool' || messages[i].role === 'function') {
      trailingToolMessages.unshift(messages[i]);
      continue;
    }
    break;
  }
  if (trailingToolMessages.length) return buildPrompt(trailingToolMessages, tools, toolChoice);

  // Find the last user message for its text; if none, fall back to full prompt.
  let lastUser = null;
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role === 'user') { lastUser = messages[i]; break; }
  }
  if (!lastUser) return buildPrompt(messages, tools, toolChoice);
  const text = textFromContent(lastUser.content);
  return (text + buildToolInstructions(tools, toolChoice)).trim();
}

function tryParseJson(value) {
  try { return JSON.parse(value); } catch { return null; }
}

function decodeToolText(value) {
  const trimmed = String(value ?? '').trim();
  const cdata = trimmed.match(/^<!\[CDATA\[([\s\S]*?)\]\]>$/);
  return cdata ? cdata[1] : trimmed;
}

function normalizeToolArguments(args) {
  // OpenAI spec: `arguments` must always be a JSON string.
  if (args == null) return '{}';
  if (typeof args === 'string') {
    const trimmed = args.trim();
    if (!trimmed) return '{}';
    // If it's already valid JSON, re-stringify for canonical form.
    const parsed = tryParseJson(trimmed);
    return parsed === null ? trimmed : JSON.stringify(parsed);
  }
  // Object/number/boolean -> stringify.
  try {
    return JSON.stringify(args);
  } catch {
    return '{}';
  }
}

function allowedToolName(name, tools) {
  if (!name || typeof name !== 'string') return '';
  if (!Array.isArray(tools)) return '';
  return tools.find(t => t.function.name === name)?.function.name || '';
}

function toOpenAIToolCalls(calls, tools) {
  return calls
    .map((call, index) => {
      const fn = call.function || call;
      const name = allowedToolName(fn.name || fn.tool || fn.tool_name || fn.function_name, tools);
      if (!name) return null;
      return {
        id: call.id || `call_${Date.now().toString(36)}_${index}_${Math.random().toString(36).slice(2, 8)}`,
        type: 'function',
        function: {
          name,
          arguments: normalizeToolArguments(fn.arguments ?? fn.args ?? fn.input ?? fn.parameters ?? call.arguments ?? {}),
        },
      };
    })
    .filter(Boolean);
}

// Extract the JSON inside the LAST <tag>...</tag> block. Using the last closing
// tag avoids matching a stray half-opened tag that the model may have written
// inside its reasoning (e.g. "I will now emit <tool_calls>...").
function extractJsonBlock(text, tag) {
  const open = `<${tag}`;
  const close = `</${tag}>`;
  const lastClose = text.toLowerCase().lastIndexOf(close);
  if (lastClose === -1) return null;
  const lastOpen = text.toLowerCase().lastIndexOf(open, lastClose);
  if (lastOpen === -1) return null;
  const inner = text.slice(lastOpen + open.length, lastClose);
  // strip any attributes on the opening tag up to '>'
  const gt = inner.indexOf('>');
  const body = gt === -1 ? inner : inner.slice(gt + 1);
  const trimmed = body.trim();
  return trimmed || null;
}

function stripToolBlocks(text) {
  return text
    .replace(/<\|QNML\|tool_calls\b[^>]*>[\s\S]*?<\/\|QNML\|tool_calls>/gi, '')
    .replace(/<tool_calls\b[^>]*>[\s\S]*?<\/tool_calls>/gi, '')
    .replace(/<tool_call\b[^>]*>[\s\S]*?<\/tool_call>/gi, '')
    .trim();
}

function parseQNMLToolCalls(text) {
  const calls = [];
  const invokeRe = /<\|QNML\|invoke\s+name=["']([^"']+)["'][^>]*>([\s\S]*?)<\/\|QNML\|invoke>/gi;
  for (const invoke of text.matchAll(invokeRe)) {
    const input = {};
    const paramRe = /<\|QNML\|parameter\s+name=["']([^"']+)["'][^>]*>([\s\S]*?)<\/\|QNML\|parameter>/gi;
    for (const param of invoke[2].matchAll(paramRe)) {
      const raw = decodeToolText(param[2]);
      input[param[1]] = tryParseJson(raw) ?? raw;
    }
    calls.push({ name: invoke[1], arguments: input });
  }
  return calls;
}

function parseNamedXMLToolCalls(text) {
  const calls = [];
  const callRe = /<tool_call\s+name=["']([^"']+)["'][^>]*>([\s\S]*?)<\/tool_call>/gi;
  for (const call of text.matchAll(callRe)) {
    const body = decodeToolText(call[2]);
    calls.push({ name: call[1], arguments: tryParseJson(body) ?? body });
  }
  return calls;
}

function parseJSONToolCalls(value) {
  if (!value) return [];
  if (Array.isArray(value)) return value.flatMap(parseJSONToolCalls);
  if (typeof value !== 'object') return [];

  const nested = value.tool_calls || value.tools || value.calls || value.function_call;
  if (nested) return parseJSONToolCalls(nested);

  const fn = value.function && typeof value.function === 'object' ? value.function : value;
  const name = fn.name || fn.tool || fn.tool_name || fn.function_name;
  if (!name) return [];
  return [{
    id: value.id,
    name,
    arguments: fn.arguments ?? fn.args ?? fn.input ?? fn.parameters ?? value.arguments ?? {},
  }];
}

function parseTextKVToolCall(text) {
  const name = text.match(/function\.name\s*:\s*([^\n\r]+)/i)?.[1]?.trim();
  const args = text.match(/function\.arguments\s*:\s*([\s\S]+)/i)?.[1]?.trim();
  if (!name) return [];
  return [{ name, arguments: tryParseJson(args) ?? args ?? {} }];
}

function dedupeToolCalls(calls) {
  const seen = new Set();
  return calls.filter(call => {
    const key = `${call.function.name}\0${call.function.arguments}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

function parseToolCallsFromText(text, tools) {
  if (!text) return null;

  const qnmlCalls = toOpenAIToolCalls(parseQNMLToolCalls(text), tools);
  if (qnmlCalls.length) return { toolCalls: dedupeToolCalls(qnmlCalls), content: stripToolBlocks(text) };

  const namedXMLCalls = toOpenAIToolCalls(parseNamedXMLToolCalls(text), tools);
  if (namedXMLCalls.length) return { toolCalls: dedupeToolCalls(namedXMLCalls), content: stripToolBlocks(text) };

  const blocks = [
    extractJsonBlock(text, 'tool_calls'),
    extractJsonBlock(text, 'tool_call'),
  ].filter(Boolean);

  for (const block of blocks) {
    const parsed = tryParseJson(block);
    if (!parsed) continue;
    const toolCalls = toOpenAIToolCalls(parseJSONToolCalls(parsed), tools);
    if (toolCalls.length) return { toolCalls: dedupeToolCalls(toolCalls), content: stripToolBlocks(text) };
  }

  const trimmed = text.trim().replace(/^```(?:json)?\s*/i, '').replace(/```$/i, '').trim();
  const parsed = tryParseJson(trimmed);
  if (parsed) {
    const toolCalls = toOpenAIToolCalls(parseJSONToolCalls(parsed), tools);
    if (toolCalls.length) return { toolCalls: dedupeToolCalls(toolCalls), content: '' };
  }

  const kvCalls = toOpenAIToolCalls(parseTextKVToolCall(text), tools);
  if (kvCalls.length) return { toolCalls: dedupeToolCalls(kvCalls), content: stripToolBlocks(text) };

  return null;
}

// Stream parsed tool_calls incrementally per the OpenAI streaming protocol:
// first a chunk carrying id/type/name + empty arguments, then the arguments
// string split into fixed-size deltas, so clients built for incremental
// arguments (Codex/OpenAI-compatible clients) work correctly.
const ARGS_CHUNK_SIZE = 24;
function streamToolCallsIncremental(res, writeOpts, toolCalls) {
  for (let i = 0; i < toolCalls.length; i++) {
    const tc = toolCalls[i];
    // Opening chunk: index, id, type, name, and empty arguments.
    writeSSE(res, {
      ...writeOpts,
      choices: [{ index: 0, delta: { tool_calls: [{ index: i, id: tc.id, type: 'function', function: { name: tc.function.name, arguments: '' } }] }, finish_reason: null }],
    });
    const args = tc.function.arguments || '';
    for (let j = 0; j < args.length; j += ARGS_CHUNK_SIZE) {
      writeSSE(res, {
        ...writeOpts,
        choices: [{ index: 0, delta: { tool_calls: [{ index: i, function: { arguments: args.slice(j, j + ARGS_CHUNK_SIZE) } }] }, finish_reason: null }],
      });
    }
  }
}

function writeSSE(res, payload) {
  res.write(`data: ${JSON.stringify(payload)}\n\n`);
  flushSSE(res);
}

export async function handleOpenAICompletion(req, res) {
  const { model, messages, stream = false, max_tokens } = req.body;
  const tools = normalizeTools(req.body.tools);
  const toolChoice = req.body.tool_choice ?? 'auto';
  const toolCallingEnabled = tools.length > 0 && toolChoice !== 'none';

  if (!model || !messages || !messages.length) {
    return res.status(400).json({ error: { message: 'model and messages are required' } });
  }

  const modelType = mapModel(model);
  const fullPrompt = buildPrompt(messages, tools, toolChoice);
  // Latest-turn-only prompt: used when conversation affinity engages, so the
  // upstream gets just the new user message (DeepSeek keeps the rest server-side
  // via parent_message_id). Falls back to fullPrompt when affinity is off.
  const latestPrompt = buildLatestPrompt(messages, tools, toolChoice);
  const thinkingEnabled = req.body.thinking_enabled ?? !toolCallingEnabled;
  const searchEnabled = req.body.search_enabled ?? (modelType !== 'vision');
  // Default: send thinking as a separate reasoning_content field for OpenAI-compatible clients.
  // Set merge_thinking=true or MERGE_THINKING=true to merge into content with <arg_key> tags instead
  const mergeThinking = req.body.merge_thinking ?? (process.env.MERGE_THINKING === 'true');

  const conversationId = getConversationId(req, messages);

  const requestId = `chatcmpl-${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const requestStart = Date.now();
  let result;

  try {
    let refFileIds = [];
    let uploadSlot = null;
    if (modelType === 'vision') {
      uploadSlot = await enqueueRequest(true);
      try {
        refFileIds = await extractImages(messages, uploadSlot.token);
      } finally {
        uploadSlot.release();
        dispatchQueued();
      }
    }

    // Conversation affinity: resolve a DeepSeek session + parent_message_id for
    // this conversation, bound to the token that completion() acquires. The
    // prompt is selected after resolution via getPrompt (latest-only when
    // affinity engages, full history otherwise).
    const resolveSession = conversationId
      ? async (token) => {
          const r = await resolveConversation({ conversationId, modelType, token });
          return { sessionId: r.sessionId, parentMessageId: r.parentMessageId, affinity: r.affinity };
        }
      : null;
    const getPrompt = (affinity) => affinity ? latestPrompt : fullPrompt;

    result = await completion({ modelType, prompt: fullPrompt, thinkingEnabled, searchEnabled, refFileIds, preferVision: modelType === 'vision', resolveSession, getPrompt });
  } catch (err) {
    console.error('Completion error:', err.message);
    return res.status(500).json({ error: { message: err.message } });
  }

  const { body: streamBody, slot } = result;

  // Detect client disconnect so we can cancel the upstream stream and release
  // the token slot instead of blocking on parseSSEStream until upstream ends.
  let clientGone = false;
  const onClose = () => {
    clientGone = true;
    try { streamBody.cancel(); } catch {}
  };
  req.on('close', onClose);

  try {
    if (stream) {
      // Set TCP_NODELAY on the socket immediately to prevent Nagle buffering
      const _socket = req.socket || req.connection;
      if (_socket && typeof _socket.setNoDelay === "function") _socket.setNoDelay(true);
      res.writeHead(200, {
        'Content-Type': 'text/event-stream',
        'Cache-Control': 'no-cache',
        'Connection': 'keep-alive',
        'X-Accel-Buffering': 'no',
      });

      writeSSE(res, {
        id: requestId,
        object: 'chat.completion.chunk',
        created: Math.floor(Date.now() / 1000),
        model,
        choices: [{ index: 0, delta: { role: 'assistant' }, finish_reason: null }],
      });

      let inThinkingPhase = thinkingEnabled;
      let thinkingTagOpened = false;
      let firstChunkTime = null;
      let streamUsage = 0;
      // When tool-calling, content (model output) is buffered until the stream
      // ends so we can parse <tool_calls> blocks. Thinking stays separate and is
      // streamed live (see below) so reasoning never leaks into `content`.
      let contentBuffer = '';

      for await (const event of parseSSEStream(streamBody)) {
        if (clientGone) break;
        if (event.type === 'error') {
          if (event.code === 40004) {
            console.error(`Account BANNED in stream: ${slot.token.slice(0, 12)}...`);
          }
          throw new Error(event.message || `DeepSeek error ${event.code}`);
        }
        if (event.messageIds?.responseMessageId) {
          recordResponseMessageId(conversationId, event.messageIds.responseMessageId);
        }
        if (event.type === 'content') {
          if (toolCallingEnabled) {
            // Buffer model output; do NOT touch inThinkingPhase here so that
            // any interleaved thinking events keep streaming as reasoning_content.
            contentBuffer += event.content;
            continue;
          }
          if (mergeThinking && thinkingTagOpened) {
            thinkingTagOpened = false;
            writeSSE(res, {
              id: requestId, object: 'chat.completion.chunk', created: Math.floor(Date.now() / 1000), model,
              choices: [{ index: 0, delta: { content: '\n</think>\n' }, finish_reason: null }],
            });
          }
          inThinkingPhase = false;
          writeSSE(res, {
            id: requestId,
            object: 'chat.completion.chunk',
            created: Math.floor(Date.now() / 1000),
            model,
            choices: [{ index: 0, delta: { content: event.content }, finish_reason: null }],
          });
        } else if (event.type === 'thinking') {
          if (toolCallingEnabled) {
            // Stream reasoning live as reasoning_content; never mix it into
            // contentBuffer (which is the tool-call source) so thinking cannot
            // leak into the returned `content` or corrupt tool-call parsing.
            writeSSE(res, {
              id: requestId,
              object: 'chat.completion.chunk',
              created: Math.floor(Date.now() / 1000),
              model,
              choices: [{ index: 0, delta: { reasoning_content: event.content }, finish_reason: null }],
            });
            continue;
          }
          if (!inThinkingPhase) continue;
          if (mergeThinking) {
            if (!thinkingTagOpened) {
              thinkingTagOpened = true;
              writeSSE(res, {
                id: requestId, object: 'chat.completion.chunk', created: Math.floor(Date.now() / 1000), model,
                choices: [{ index: 0, delta: { content: '<think>\n' }, finish_reason: null }],
              });
            }
            writeSSE(res, {
              id: requestId,
              object: 'chat.completion.chunk',
              created: Math.floor(Date.now() / 1000),
              model,
              choices: [{ index: 0, delta: { content: event.content }, finish_reason: null }],
            });
          } else {
            writeSSE(res, {
              id: requestId,
              object: 'chat.completion.chunk',
              created: Math.floor(Date.now() / 1000),
              model,
              choices: [{ index: 0, delta: { reasoning_content: event.content }, finish_reason: null }],
            });
          }
        } else if (event.type === 'usage') {
          streamUsage = event.usage;
        } else if (event.type === 'done') {
          if (mergeThinking && thinkingTagOpened) {
            thinkingTagOpened = false;
            writeSSE(res, {
              id: requestId, object: 'chat.completion.chunk', created: Math.floor(Date.now() / 1000), model,
              choices: [{ index: 0, delta: { content: '\n</think>\n' }, finish_reason: null }],
            });
          }

          const writeOpts = {
            id: requestId,
            object: 'chat.completion.chunk',
            created: Math.floor(Date.now() / 1000),
            model,
          };

          const parsedToolCalls = toolCallingEnabled ? parseToolCallsFromText(contentBuffer, tools) : null;
          if (parsedToolCalls?.toolCalls?.length) {
            // Emit tool_calls with incremental arguments chunks, then finish.
            streamToolCallsIncremental(res, writeOpts, parsedToolCalls.toolCalls);
            writeSSE(res, {
              ...writeOpts,
              choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }],
            });
          } else {
            // No tool call: fall back to a normal response using buffered output.
            if (toolCallingEnabled && contentBuffer) {
              writeSSE(res, {
                ...writeOpts,
                choices: [{ index: 0, delta: { content: contentBuffer }, finish_reason: null }],
              });
            }
            writeSSE(res, {
              ...writeOpts,
              choices: [{ index: 0, delta: {}, finish_reason: 'stop' }],
            });
          }
          res.write('data: [DONE]\n\n');
          flushSSE(res);
          // Record token speed at stream end
          const streamDuration = Date.now() - requestStart;
          if (streamUsage > 0 && streamDuration > 0) {
            recordTokenSpeed(model, streamUsage, streamDuration);
          }
        }
      }
      res.end();
    } else {
      let fullContent = '';
      let fullThinking = '';
      let usage = 0;
      let inThinkingPhase = thinkingEnabled;

      for await (const event of parseSSEStream(streamBody)) {
        if (clientGone) break;
        if (event.type === 'error') {
          if (event.code === 40004) {
            console.error(`Account BANNED in stream: ${slot.token.slice(0, 12)}...`);
          }
          throw new Error(event.message || `DeepSeek error ${event.code}`);
        }
        if (event.messageIds?.responseMessageId) {
          recordResponseMessageId(conversationId, event.messageIds.responseMessageId);
        }
        if (event.type === 'content') {
          fullContent += event.content;
          inThinkingPhase = false;
        } else if (event.type === 'thinking' && inThinkingPhase) {
          fullThinking += event.content;
        } else if (event.type === 'usage') usage = event.usage;
      }

      // Record TTFB and token speed for non-streaming
      const totalDuration = Date.now() - requestStart;
      recordTTFB(model, totalDuration);
      if (usage > 0 && totalDuration > 0) {
        recordTokenSpeed(model, usage, totalDuration);
      }

      // Parse tool calls only from model output, never from thinking. Thinking
      // leaking into the parse source caused tool-call misfires and exposed
      // reasoning in `content`.
      const parsedToolCalls = toolCallingEnabled ? parseToolCallsFromText(fullContent, tools) : null;
      const message = parsedToolCalls?.toolCalls?.length
        ? {
            role: 'assistant',
            content: parsedToolCalls.content || null,
            tool_calls: parsedToolCalls.toolCalls,
            ...((!mergeThinking && fullThinking) ? { reasoning_content: fullThinking } : {}),
          }
        : {
            role: 'assistant',
            content: mergeThinking && fullThinking
              ? `<think>\n${fullThinking}\n</think>\n${fullContent}`
              : fullContent,
            ...((!mergeThinking && fullThinking) ? { reasoning_content: fullThinking } : {}),
          };

      const response = {
        id: requestId,
        object: 'chat.completion',
        created: Math.floor(Date.now() / 1000),
        model,
        choices: [{
          index: 0,
          message,
          finish_reason: parsedToolCalls?.toolCalls?.length ? 'tool_calls' : 'stop',
        }],
        usage: {
          prompt_tokens: 0,
          completion_tokens: usage,
          total_tokens: usage,
        },
      };
      res.json(response);
    }
  } catch (err) {
    console.error('Stream error:', err.message);
    if (!res.headersSent) {
      res.status(500).json({ error: { message: err.message } });
    } else {
      res.end();
    }
  } finally {
    req.off('close', onClose);
    slot.release();
    dispatchQueued();
  }
}

export function handleOpenAIModels(req, res) {
  res.json({
    object: 'list',
    data: Object.keys(MODEL_MAP).map((id, i) => ({
      id,
      object: 'model',
      created: 1700000000,
      owned_by: 'deepseek',
    })),
  });
}
