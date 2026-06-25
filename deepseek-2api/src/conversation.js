// Conversation affinity: keep one DeepSeek session per logical conversation and
// chain turns via parent_message_id, so the proxy sends only the latest user
// turn instead of flattening the whole history (which caused role confusion).
//
// A conversation is identified by:
//   1. explicit request header  X-Conversation-Id   (preferred — your agent sets it)
//   2. fallback: hash of the full messages array     (auto, keeps multi-turn clients working)
//
// State is in-memory only and evicted by TTL / size. It maps:
//   conversationId -> { sessionId, parentMessageId, tokenPrefix, createdAt, lastSeen, turns }
//
// All functions are no-ops when ENABLE_CONVERSATION_AFFINITY is not "true", so
// legacy clients that don't send an id and don't want affinity are unaffected.

import { createHash } from 'crypto';
import { createSession } from './session.js';

const ENABLED = process.env.ENABLE_CONVERSATION_AFFINITY === 'true';
const TTL_MS = parseInt(process.env.CONVERSATION_TTL_MS || String(30 * 60 * 1000), 10); // 30 min idle -> evict
const MAX_CONVERSATIONS = parseInt(process.env.MAX_CONVERSATIONS || '500', 10);
// Hard cap on turns per DeepSeek session before we rotate (defence against the
// non-deterministic empty-content seen beyond ~12 chained turns).
const MAX_TURNS_PER_SESSION = parseInt(process.env.MAX_TURNS_PER_SESSION || '10', 10);

const store = new Map(); // conversationId -> entry

function hashMessages(messages) {
  // Stable hash of the message sequence. Includes roles so two different
  // conversations with identical concatenated text don't collide.
  const h = createHash('sha1');
  for (const m of messages) {
    h.update(String(m.role || ''));
    h.update('\0');
    const c = typeof m.content === 'string' ? m.content : JSON.stringify(m.content || '');
    h.update(c);
    h.update('\0');
  }
  return h.digest('hex').slice(0, 24);
}

function now() { return Date.now(); }

function evictExpired() {
  const cutoff = now() - TTL_MS;
  for (const [id, e] of store) {
    if (e.lastSeen < cutoff) store.delete(id);
  }
}

function enforceCapacity() {
  if (store.size <= MAX_CONVERSATIONS) return;
  // Drop the oldest-seen entries first.
  const entries = [...store.entries()].sort((a, b) => a[1].lastSeen - b[1].lastSeen);
  const drop = store.size - MAX_CONVERSATIONS;
  for (let i = 0; i < drop; i++) store.delete(entries[i][0]);
}

// Resolve the conversation id for an incoming request. Returns null when
// affinity is disabled or no id can be derived (e.g. empty messages).
export function getConversationId(req, messages) {
  if (!ENABLED) return null;
  const explicit = req.headers['x-conversation-id'];
  if (explicit && typeof explicit === 'string' && explicit.trim()) return explicit.trim();
  if (Array.isArray(messages) && messages.length) return 'auto:' + hashMessages(messages);
  return null;
}

// Decide whether this request should run in affinity mode, and if so return the
// continuation parameters (sessionId + parentMessageId). Creates/rotates the
// DeepSeek session lazily on first sight or when limits are hit.
//
//   modelType: the DeepSeek model_type for this request
//   token:     the DeepSeek bearer token acquired for THIS request (sessions are
//              per-token, so we must create/lookup with the same token)
//
// Returns { affinity: bool, sessionId, parentMessageId, promptMode }
//   promptMode 'latest'  -> caller sends only the latest user turn
//   promptMode 'full'    -> caller sends the full history (fallback / non-affinity)
export async function resolveConversation({ conversationId, modelType, token }) {
  if (!ENABLED || !conversationId) {
    return { affinity: false, sessionId: null, parentMessageId: null, promptMode: 'full' };
  }

  evictExpired();
  enforceCapacity();

  const tokenPrefix = token.slice(0, 12);
  const existing = store.get(conversationId);
  const fresh = !existing
    || existing.tokenPrefix !== tokenPrefix   // landed on a different token -> new session
    || existing.turns >= MAX_TURNS_PER_SESSION; // rotate to avoid degradation

  if (fresh) {
    // Lazily create a DeepSeek session bound to this token. Failure bubbles up
    // to the caller, which should fall back to full-history mode.
    const session = await createSession(token, modelType);
    const entry = {
      sessionId: session.id,
      parentMessageId: null,
      tokenPrefix,
      modelType,
      createdAt: now(),
      lastSeen: now(),
      turns: 0,
    };
    store.set(conversationId, entry);
    return { affinity: true, sessionId: entry.sessionId, parentMessageId: null, promptMode: 'latest' };
  }

  existing.lastSeen = now();
  return { affinity: true, sessionId: existing.sessionId, parentMessageId: existing.parentMessageId, promptMode: 'latest' };
}

// Record the response_message_id emitted by the upstream stream so the NEXT
// turn can chain off it. Called from the SSE loop in openai.js.
export function recordResponseMessageId(conversationId, responseMessageId) {
  if (!ENABLED || !conversationId || responseMessageId == null) return;
  const entry = store.get(conversationId);
  if (!entry) return;
  entry.parentMessageId = responseMessageId;
  entry.turns = (entry.turns || 0) + 1;
  entry.lastSeen = now();
}

// Invalidate a conversation (e.g. when its token gets marked dead/refreshed).
export function invalidateConversation(conversationId) {
  if (conversationId) store.delete(conversationId);
}

// Invalidate all conversations bound to a token prefix (used after token refresh).
export function invalidateByTokenPrefix(tokenPrefix) {
  if (!tokenPrefix) return;
  for (const [id, e] of store) {
    if (e.tokenPrefix === tokenPrefix) store.delete(id);
  }
}

export function getConversationInfo() {
  if (!ENABLED) return { enabled: false };
  let oldest = Infinity;
  for (const e of store.values()) oldest = Math.min(oldest, e.lastSeen);
  return {
    enabled: true,
    active: store.size,
    maxConversations: MAX_CONVERSATIONS,
    ttlMs: TTL_MS,
    maxTurnsPerSession: MAX_TURNS_PER_SESSION,
  };
}
