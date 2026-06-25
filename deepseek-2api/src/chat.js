import { setRequestToken, reportTokenError, reportTokenSuccess, markTokenDead } from './auth.js';
import { solvePowChallengeWithToken } from './pow.js';
import { getSession } from './session.js';
import { streamHeaders, proxiedFetch } from './headers.js';
import { enqueueRequest, dispatchQueued } from './queue.js';

const BASE_URL = 'https://chat.deepseek.com';

function deepSeekErrorMessage(json, fallback = 'DeepSeek error') {
  const biz = json?.data;
  const code = json?.code ?? biz?.biz_code;
  const msg = json?.msg || biz?.biz_msg || fallback;
  return { code, message: msg };
}

async function markTokenUnavailable(token) {
  // Muted tokens are unusable for completions until the mute expires, so mark dead immediately.
  markTokenDead(token);
  const { invalidateTokenSessions } = await import('./session.js');
  invalidateTokenSessions(token.slice(0, 12));
}

function isDeepSeekJsonError(json) {
  return json?.code !== undefined || json?.data?.biz_code !== undefined || json?.data?.biz_msg;
}

async function throwDeepSeekErrorFromJson(json, slot) {
  const { code, message } = deepSeekErrorMessage(json);

  if (!isDeepSeekJsonError(json)) return;

  if (code === 40003) {
    reportTokenError(slot.token);
    throw new Error('Token invalid (40003)');
  }
  if (code === 40004) {
    reportTokenError(slot.token);
    const entry = (await import('./auth.js')).getPoolInfo().find(t => slot.token.startsWith(t.token.replace('...', '')));
    console.error(`Account BANNED during completion: ${entry?.email || slot.token.slice(0, 12)}...`);
    throw new Error('Account banned (40004)');
  }
  if (code === 40301) {
    const { invalidateTokenSessions } = await import('./session.js');
    invalidateTokenSessions(slot.token.slice(0, 12));
    throw new Error('Session rate limited (40301) — sessions rotated');
  }
  if (code === 429) {
    throw new Error('Rate limited (429)');
  }
  if (code === 5 || json?.data?.biz_code === 5) {
    await markTokenUnavailable(slot.token);
    const until = json?.data?.biz_data?.mute_until;
    throw new Error(`DeepSeek user muted (biz_code=5)${until ? ` until ${until}` : ''}: ${message}`);
  }

  throw new Error(`DeepSeek error${code != null ? ` ${code}` : ''}: ${message}`);
}

// resolveSession(token): optional async hook. When provided (conversation-affinity
// mode), it is called AFTER the token slot is acquired and must return
// { sessionId, parentMessageId, affinity }. When omitted, a session is
// acquired/rotated via getSession and parentMessageId defaults to null (legacy
// behaviour).
// getPrompt(affinity): optional sync hook returning the prompt string to send,
// chosen based on whether affinity engaged. When omitted, the `prompt` arg is
// used as-is. This keeps prompt selection sequenced after session resolution.
export async function completion({ modelType, prompt, thinkingEnabled = false, searchEnabled = false, parentMessageId = null, refFileIds = [], preferVision = false, resolveSession = null, getPrompt = null }) {
  // Step 1: Acquire token slot first — PoW and completion must use the same token
  const slot = await enqueueRequest(preferVision);

  try {
    // Step 2: Solve PoW using the same token
    const { powResponse } = await solvePowChallengeWithToken(slot.token);

    let sessionIdFinal;
    let parentMessageIdFinal = parentMessageId;
    let affinity = false;
    if (resolveSession) {
      // Affinity mode: the caller decides which session + parent to use,
      // bound to the token that was just acquired.
      const resolved = await resolveSession(slot.token);
      sessionIdFinal = resolved.sessionId;
      parentMessageIdFinal = resolved.parentMessageId;
      affinity = !!resolved.affinity;
    } else {
      setRequestToken(slot.token);
      const session = await getSession(slot.token, modelType);
      setRequestToken(null);
      sessionIdFinal = session.id;
    }

    const promptFinal = getPrompt ? getPrompt(affinity) : prompt;

    const body = {
      chat_session_id: sessionIdFinal,
      parent_message_id: parentMessageIdFinal,
      model_type: modelType,
      prompt: promptFinal,
      ref_file_ids: refFileIds,
      thinking_enabled: thinkingEnabled,
      search_enabled: searchEnabled,
      action: null,
      preempt: false,
    };

    const res = await proxiedFetch(`${BASE_URL}/api/v0/chat/completion`, {
      method: 'POST',
      headers: await streamHeaders(slot.token, powResponse),
      body: JSON.stringify(body),
    });

    const contentType = res.headers.get('content-type') || '';
    if (contentType.includes('application/json')) {
      const text = await res.text();
      try {
        await throwDeepSeekErrorFromJson(JSON.parse(text), slot);
      } catch (err) {
        if (err instanceof SyntaxError) throw new Error(`Completion request returned invalid JSON: ${text}`);
        throw err;
      }
    }

    if (!res.ok) {
      const text = await res.text();
      // Check for specific DeepSeek error codes
      try {
        await throwDeepSeekErrorFromJson(JSON.parse(text), slot);
      } catch (parseErr) {
        if (parseErr instanceof SyntaxError) {
          throw new Error(`Completion request failed: ${res.status} ${text}`);
        }
        throw parseErr;
      }
    }

    // DeepSeek sometimes returns HTTP 200 + text/event-stream but the body is a plain JSON error,
    // e.g. {"code":0,"data":{"biz_code":5,"biz_msg":"user is muted"}}.
    // Probe the first chunk so those errors are surfaced instead of being parsed as an empty SSE stream.
    const reader = res.body.getReader();
    const first = await reader.read();
    if (first.done) {
      reader.releaseLock();
      throw new Error('Completion stream ended before any data');
    }

    const firstText = new TextDecoder().decode(first.value, { stream: true }).trim();
    if (firstText.startsWith('{')) {
      try {
        await throwDeepSeekErrorFromJson(JSON.parse(firstText), slot);
      } catch (err) {
        if (!(err instanceof SyntaxError)) throw err;
      }
    }

    const bodyWithFirstChunk = new ReadableStream({
      start(controller) {
        controller.enqueue(first.value);
        const pump = () => reader.read().then(({ done, value }) => {
          if (done) {
            reader.releaseLock();
            controller.close();
            return;
          }
          controller.enqueue(value);
          return pump();
        }).catch(err => {
          try { reader.releaseLock(); } catch {}
          controller.error(err);
        });
        return pump();
      },
      cancel(reason) {
        return reader.cancel(reason).finally(() => {
          try { reader.releaseLock(); } catch {}
        });
      },
    });

    reportTokenSuccess(slot.token);
    return { body: bodyWithFirstChunk, slot };
  } catch (err) {
    setRequestToken(null);
    slot.release();
    dispatchQueued();
    throw err;
  }
}

export async function* parseSSEStream(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let messageIds = {};
  let currentFragmentType = null;

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        const leftover = buffer.trim();
        if (leftover.startsWith('{')) {
          try {
            const parsed = JSON.parse(leftover);
            const { code, message } = deepSeekErrorMessage(parsed);
            yield { type: 'error', code, message };
          } catch {}
        }
        break;
      }

      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() || '';

      for (const line of lines) {
        if (line.startsWith('event:')) {
          if (line.slice(6).trim() === 'close') return;
          continue;
        }
        if (!line.startsWith('data:')) continue;
        const data = line.slice(5).trim();
        if (!data) continue;

        try {
          const parsed = JSON.parse(data);

          // Check for error codes mid-stream
          if (parsed.code === 40003) {
            yield { type: 'error', code: 40003, message: 'Token invalid' };
            return;
          }
          if (parsed.code === 40004) {
            yield { type: 'error', code: 40004, message: 'Account banned' };
            return;
          }
          if (parsed.code === 40301) {
            yield { type: 'error', code: 40301, message: 'Session rate limited' };
            return;
          }
          if (parsed.data?.biz_code) {
            yield { type: 'error', code: parsed.data.biz_code, message: parsed.data.biz_msg || 'DeepSeek biz error' };
            return;
          }

          if (parsed.request_message_id != null) {
            messageIds.requestMessageId = parsed.request_message_id;
            messageIds.responseMessageId = parsed.response_message_id;
          }

          if (parsed.v?.response?.fragments) {
            for (const frag of parsed.v.response.fragments) {
              if (frag.type === 'THINK' && frag.content) {
                currentFragmentType = 'THINK';
                yield { type: 'thinking', content: frag.content, messageIds };
              } else if (frag.type === 'RESPONSE' && frag.content) {
                currentFragmentType = 'RESPONSE';
                yield { type: 'content', content: frag.content, messageIds };
              }
            }
            if (parsed.v.response.accumulated_token_usage != null) {
              yield { type: 'usage', usage: parsed.v.response.accumulated_token_usage, messageIds };
            }
          }

          if (parsed.p && parsed.o) {
            if (parsed.p === 'response/fragments/-1/content' && parsed.o === 'APPEND' && typeof parsed.v === 'string') {
              yield { type: currentFragmentType === 'THINK' ? 'thinking' : 'content', content: parsed.v, messageIds };
            } else if (parsed.p === 'response/fragments/-1/content' && !parsed.o && typeof parsed.v === 'string') {
              yield { type: currentFragmentType === 'THINK' ? 'thinking' : 'content', content: parsed.v, messageIds };
            } else if (parsed.p === 'response/status' && parsed.v === 'FINISHED') {
              yield { type: 'done', messageIds };
            } else if (parsed.p === 'response' && parsed.o === 'BATCH' && Array.isArray(parsed.v)) {
              for (const item of parsed.v) {
                if (item.p === 'accumulated_token_usage') {
                  yield { type: 'usage', usage: item.v, messageIds };
                }
              }
            } else if (parsed.p === 'response/fragments' && parsed.o === 'APPEND' && Array.isArray(parsed.v)) {
              for (const frag of parsed.v) {
                if (frag.type === 'RESPONSE' && frag.content) {
                  currentFragmentType = 'RESPONSE';
                  yield { type: 'content', content: frag.content, messageIds };
                } else if (frag.type === 'THINK' && frag.content) {
                  currentFragmentType = 'THINK';
                  yield { type: 'thinking', content: frag.content, messageIds };
                }
              }
            }
            continue;
          }

          // Content deltas with NO `p` field are plain text chunks. Packets
          // that carry a `p` path we don't explicitly handle above (e.g.
          // "response/fragments/-1/status" -> "FINISHED",
          // "response/conversation_mode" -> "SEARCH", "response/search/...")
          // are protocol control messages and must NOT be emitted as content —
          // otherwise their string values (FINISHED/SEARCH/...) leak into the
          // stream and corrupt tool-call block parsing.
          if (typeof parsed.v === 'string' && !parsed.p) {
            yield { type: currentFragmentType === 'THINK' ? 'thinking' : 'content', content: parsed.v, messageIds };
          }

          if (Array.isArray(parsed.v)) {
            for (const item of parsed.v) {
              if (item.p === 'accumulated_token_usage') {
                yield { type: 'usage', usage: item.v, messageIds };
              }
            }
          }
        } catch {
          // skip unparseable lines
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}
