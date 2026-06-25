import { completion, parseSSEStream } from './chat.js';
import { pickToken } from './auth.js';
import { dispatchQueued } from './queue.js';

// Flush SSE data immediately
function flushSSE(res) {
  if (res.flush) res.flush();
  else if (res._flush) res._flush();
  const socket = res.socket || res._socket;
  if (socket && typeof socket.setNoDelay === 'function') socket.setNoDelay(true);
}

const MODEL_MAP = {
  'deepseek-v4-flash': 'default',
  'deepseek-v4-pro': 'expert',
  'deepseek-v4-vision': 'vision',
  'deepseek-v4-flash[1m]': 'default',
  'deepseek-v4-pro[1m]': 'expert',
  'deepseek-v4-vision[1m]': 'vision',
};

export async function handleDeepSeekCompletion(req, res) {
  const body = req.body;
  const modelType = body.model_type || MODEL_MAP[body.model] || 'default';
  const prompt = body.prompt || '';
  const thinkingEnabled = body.thinking_enabled ?? false;
  const searchEnabled = body.search_enabled ?? false;
  const parentMessageId = body.parent_message_id ?? null;
  const refFileIds = body.ref_file_ids ?? [];
  let slot = null;

  if (!prompt) {
    return res.status(400).json({ code: 1, msg: 'prompt is required' });
  }

  let onClose = null;
  try {
    const result = await completion({ modelType, prompt, thinkingEnabled, searchEnabled, parentMessageId, refFileIds });
    const streamBody = result.body;
    slot = result.slot;

    // Detect client disconnect so we can cancel the upstream stream and release
    // the token slot instead of blocking on the reader until upstream ends.
    let clientGone = false;
    onClose = () => {
      clientGone = true;
      try { streamBody.cancel(); } catch {}
    };
    req.on('close', onClose);

    // Set TCP_NODELAY on the socket immediately
    const socket = req.socket || req.connection;
    if (socket && typeof socket.setNoDelay === 'function') socket.setNoDelay(true);

    res.writeHead(200, {
      'Content-Type': 'text/event-stream; charset=utf-8',
      'Cache-Control': 'no-cache',
      'X-Accel-Buffering': 'no',
    });

    const reader = streamBody.getReader();
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done || clientGone) break;
        res.write(value);
        flushSSE(res);
      }
    } finally {
      reader.releaseLock();
    }
    res.end();
  } catch (err) {
    console.error('DeepSeek completion error:', err.message);
    if (!res.headersSent) {
      res.status(500).json({ code: 1, msg: err.message });
    } else {
      res.end();
    }
  } finally {
    if (onClose) req.off('close', onClose);
    if (slot) {
      slot.release();
      dispatchQueued();
    }
  }
}
