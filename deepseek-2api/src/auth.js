import { config } from 'dotenv';
import { readFileSync, writeFileSync } from 'fs';
import { resolve, dirname } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ENV_PATH = resolve(__dirname, '..', '.env');

config();

const BASE_URL = 'https://chat.deepseek.com';

const MAX_CONCURRENT_PER_TOKEN = parseInt(process.env.MAX_CONCURRENT_PER_TOKEN || '2', 10);
const TOKEN_DEAD_THRESHOLD = parseInt(process.env.TOKEN_DEAD_THRESHOLD || '5', 10);

// Multi-token support: DS_TOKENS=token1,token2,token3 (comma-separated)
// Fallback: DS_TOKEN=single_token
// Account support: DS_ACCOUNTS=email1:pass1,email2:pass2 (auto-login to refresh tokens)
export function loadTokens() {
  const tokensStr = process.env.DS_TOKENS?.trim();
  if (tokensStr) {
    return tokensStr.split(',').map(t => t.trim()).filter(Boolean);
  }
  const single = process.env.DS_TOKEN?.trim();
  if (single) return [single];
  return [];
}

export function loadAccounts() {
  const accountsStr = process.env.DS_ACCOUNTS?.trim();
  if (!accountsStr) return [];
  return accountsStr.split(',').map(entry => {
    const [email, ...passParts] = entry.trim().split(':');
    const password = passParts.join(':');
    return email && password ? { email, password } : null;
  }).filter(Boolean);
}

const tokens = loadTokens();
const accounts = loadAccounts();

function generateDeviceId() {
  const bytes = new Uint8Array(48);
  for (let i = 0; i < 48; i++) bytes[i] = Math.floor(Math.random() * 256);
  return Buffer.from(bytes).toString('base64').replace(/=/g, '') + '==';
}

if (tokens.length === 0 && accounts.length === 0) {
  throw new Error('No DS_TOKEN/DS_TOKENS or DS_ACCOUNTS configured');
}

// DS_ACCOUNTS_EXTENDED=email:password:token_prefix — links existing tokens to accounts
function loadAccountTokens() {
  const extStr = process.env.DS_ACCOUNTS_EXTENDED?.trim();
  if (!extStr) return [];
  return extStr.split(',').map(entry => {
    const parts = entry.trim().split(':');
    if (parts.length >= 3) return { email: parts[0], password: parts[1], tokenPrefix: parts[2] };
    return null;
  }).filter(Boolean);
}

const accountTokenMap = loadAccountTokens();

// Token metadata: { token, email, password, visionCapable, lastUsed, errorCount, activeRequests, dead }
const tokenPool = tokens.map(t => ({
  token: t,
  email: null,
  password: null,
  visionCapable: null,
  lastUsed: 0,
  errorCount: 0,
  activeRequests: 0,
  dead: false,
}));

// Link existing tokens to accounts via token prefix
for (const entry of tokenPool) {
  if (!entry.token) continue;
  const prefix = entry.token.slice(0, 12);
  const match = accountTokenMap.find(a => a.tokenPrefix === prefix);
  if (match) {
    entry.email = match.email;
    entry.password = match.password;
  }
}

// Create pool entries for accounts without matching tokens (will login on init)
for (const acct of accounts) {
  const alreadyLinked = tokenPool.some(t => t.email === acct.email);
  if (!alreadyLinked) {
    tokenPool.push({
      token: null,
      email: acct.email,
      password: acct.password,
      visionCapable: null,
      lastUsed: 0,
      errorCount: 0,
      activeRequests: 0,
      dead: false,
    });
  }
}

import { loginHeaders, getHeaders, getDeviceId, proxiedFetch, getDeviceIdForToken } from './headers.js';

async function login(email, password) {
  // Use a fresh deviceId for login — real browser gets it from portal101.cn device fingerprint
  const loginDeviceId = 'B' + generateDeviceId();

  const res = await proxiedFetch(`${BASE_URL}/api/v0/users/login`, {
    method: 'POST',
    headers: loginHeaders(),
    body: JSON.stringify({ email, mobile: '', password, area_code: '', device_id: loginDeviceId, os: 'web' }),
  });

  // AWS WAF returns 202 with empty body — can't login from this IP
  if (res.status === 202) {
    throw new Error('WAF challenge (202) — login blocked from this IP, use external refresh');
  }

  const text = await res.text();
  if (!text) throw new Error('Empty response from login endpoint');

  const json = JSON.parse(text);
  if (json.code !== 0) throw new Error(`Login failed for ${email}: ${json.msg || JSON.stringify(json)}`);

  const bizCode = json.data?.biz_code;
  if (bizCode === 10) {
    throw new Error(`Account banned: ${email}`);
  }
  if (bizCode === 11) {
    throw new Error(`Account requires verification: ${email}`);
  }

  const token = json.data?.biz_data?.user?.token;
  if (!token) throw new Error(`Login succeeded but no token returned for ${email}`);
  return token;
}

async function checkVisionCapability(token) {
  try {
    const res = await proxiedFetch(`${BASE_URL}/api/v0/client/settings?did=${getDeviceId()}&scope=model`, {
      headers: await getHeaders(token),
    });
    const json = await res.json();
    const configs = json.data?.biz_data?.settings?.model_configs?.value || [];
    const visionConfig = configs.find(c => c.model_type === 'vision');
    if (visionConfig) {
      return visionConfig.switchable === true;
    }
    return false;
  } catch {
    return null;
  }
}

// Check if a token is still valid — uses /users/current which actually validates the token
// (unlike /client/settings which returns code:0 even for invalid tokens)
async function validateToken(token) {
  try {
    const res = await proxiedFetch(`${BASE_URL}/api/v0/users/current`, {
      headers: await getHeaders(token),
    });
    const json = await res.json();
    return json.code === 0;
  } catch {
    return false;
  }
}

// Refresh a dead token entry — login if account credentials exist
async function refreshToken(entry) {
  if (!entry.password) return false;
  try {
    const newToken = await login(entry.email, entry.password);
    entry.token = newToken;
    entry.errorCount = 0;
    entry.dead = false;
    const vision = await checkVisionCapability(newToken);
    entry.visionCapable = vision;
    console.log(`  Refreshed token for ${entry.email}: ${newToken.slice(0, 12)}... vision=${vision}`);
    return true;
  } catch (err) {
    console.warn(`  Refresh failed for ${entry.email}: ${err.message}`);
    // If account is banned, mark dead permanently
    if (err.message.includes('banned')) {
      entry.dead = true;
      entry.errorCount = TOKEN_DEAD_THRESHOLD;
    }
    return false;
  }
}

export async function initTokenPool() {
  console.log(`Token pool: ${tokenPool.length} entries (${tokens.length} tokens + ${accounts.length} accounts), max ${MAX_CONCURRENT_PER_TOKEN} concurrent each`);

  // Validate existing tokens, mark dead ones (auto-refresh if account linked)
  for (const entry of tokenPool) {
    if (entry.token) {
      const valid = await validateToken(entry.token);
      if (!valid) {
        console.log(`  ${entry.token.slice(0, 12)}... INVALID — ${entry.password ? 'attempting refresh' : 'no account to refresh'}`);
        if (entry.password) {
          const ok = await refreshToken(entry);
          if (ok) {
            // Remove duplicate account-only entries that now have same email
            const dupIdx = tokenPool.findIndex(t => t !== entry && t.email === entry.email && !t.token);
            if (dupIdx !== -1) {
              console.log(`  Removing duplicate account entry for ${entry.email}`);
              tokenPool.splice(dupIdx, 1);
            }
          }
        } else {
          entry.dead = true;
          entry.errorCount = TOKEN_DEAD_THRESHOLD;
        }
      }
    }
  }

  // Login account-only entries (no token yet) — remove if banned/WAF-blocked
  for (let i = tokenPool.length - 1; i >= 0; i--) {
    const entry = tokenPool[i];
    if (!entry.token && entry.password) {
      const ok = await refreshToken(entry);
      if (!ok && entry.dead) {
        // Banned or permanently failed — remove from pool
        console.log(`  Removing banned/failed account entry for ${entry.email}`);
        tokenPool.splice(i, 1);
      }
    }
  }

  // Also remove account entries with no token and no password (stale NONE entries)
  for (let i = tokenPool.length - 1; i >= 0; i--) {
    if (!tokenPool[i].token && !tokenPool[i].password) {
      console.log(`  Removing stale NONE entry at index ${i}`);
      tokenPool.splice(i, 1);
    }
  }

  // Check vision capability for valid tokens
  for (const entry of tokenPool) {
    if (entry.token && !entry.dead) {
      const vision = await checkVisionCapability(entry.token);
      entry.visionCapable = vision;
      const label = vision === true ? 'vision=YES' : vision === false ? 'vision=NO' : 'vision=UNKNOWN';
      console.log(`  ${entry.token.slice(0, 12)}... ${label} ${entry.email ? `(${entry.email})` : ''}`);
    }
  }

  const alive = tokenPool.filter(t => !t.dead).length;
  console.log(`Pool ready: ${alive}/${tokenPool.length} tokens alive`);
  persistTokensToEnv();
}

export function acquireToken(preferVision = false) {
  const liveCandidates = tokenPool.filter(t => !t.dead && t.activeRequests < MAX_CONCURRENT_PER_TOKEN && t.token);
  if (liveCandidates.length === 0) return null;

  let candidates = liveCandidates;
  if (preferVision) {
    const visionTokens = liveCandidates.filter(t => t.visionCapable === true);
    if (visionTokens.length > 0) candidates = visionTokens;
  } else {
    const nonVisionTokens = liveCandidates.filter(t => t.visionCapable !== true);
    if (nonVisionTokens.length > 0) candidates = nonVisionTokens;
  }
  // If the preferred subset is empty, fall back to liveCandidates (don't block
  // a vision request when only non-vision tokens are free, and vice versa).

  candidates.sort((a, b) => a.activeRequests - b.activeRequests);
  const chosen = candidates[0];
  chosen.activeRequests++;
  chosen.lastUsed = Date.now();

  let released = false;
  const release = () => {
    if (released) return;
    released = true;
    chosen.activeRequests = Math.max(0, chosen.activeRequests - 1);
  };

  return { token: chosen.token, account: chosen, release };
}

export function reportTokenError(token) {
  const entry = tokenPool.find(t => t.token === token);
  if (!entry) return;
  entry.errorCount++;

  if (entry.errorCount >= TOKEN_DEAD_THRESHOLD) {
    markTokenDead(entry);
  }
}

// Force a token into the dead state regardless of its current errorCount.
// Used when DeepSeek explicitly mutes/bans an account (biz_code=5, 40004).
export function markTokenDead(tokenOrEntry) {
  const entry = typeof tokenOrEntry === 'string'
    ? tokenPool.find(t => t.token === tokenOrEntry)
    : tokenOrEntry;
  if (!entry || entry.dead) return;
  entry.errorCount = TOKEN_DEAD_THRESHOLD;
  entry.dead = true;
  console.warn(`Token ${entry.token.slice(0, 12)}... marked DEAD (forced)`);

  if (entry.password) {
    refreshToken(entry).then(ok => {
      if (ok) {
        invalidateTokenSessions(entry.token);
      }
    }).catch(err => {
      console.warn(`refresh then-callback failed for ${entry.email}: ${err.message}`);
    });
  }
}

export function reportTokenSuccess(token) {
  const entry = tokenPool.find(t => t.token === token);
  if (!entry) return;
  entry.errorCount = 0;
  entry.lastUsed = Date.now();
  if (entry.dead) {
    entry.dead = false;
    console.log(`Token ${token.slice(0, 12)}... revived (was dead, now working)`);
  }
}

// Invalidate cached sessions for a token (after refresh)
function invalidateTokenSessions(token) {
  const prefix = token.slice(0, 12);
  // Dynamic import to avoid circular dependency
  import('./session.js').then(m => m.invalidateTokenSessions(prefix)).catch(() => {});
  // Also drop conversation-affinity bindings pointing at this token's sessions,
  // so chained turns don't keep targeting a now-stale session id.
  import('./conversation.js').then(m => m.invalidateByTokenPrefix(prefix)).catch(() => {});
}

// Legacy: pickToken returns just the token string
let tokenIndex = 0;
export function pickToken(preferVision = false) {
  let candidates = tokenPool.filter(t => !t.dead && t.token);

  if (preferVision) {
    const visionTokens = candidates.filter(t => t.visionCapable === true);
    if (visionTokens.length > 0) candidates = visionTokens;
  } else {
    const nonVisionTokens = candidates.filter(t => t.visionCapable !== true);
    if (nonVisionTokens.length > 0) candidates = nonVisionTokens;
  }

  if (candidates.length === 0) {
    // Absolute fallback — use any token with a token string
    candidates = tokenPool.filter(t => t.token);
    if (candidates.length === 0) throw new Error('No tokens available in pool');
  }

  const idx = tokenIndex % candidates.length;
  const chosen = candidates[idx];
  tokenIndex++;
  return chosen.token;
}

// Legacy: sticky per-request token
let currentRequestToken = null;

export function setRequestToken(token) {
  currentRequestToken = token;
}

export function getRequestToken() {
  return currentRequestToken;
}

export async function getToken(preferVision = false) {
  if (currentRequestToken) return currentRequestToken;
  return pickToken(preferVision);
}

// Legacy: email/password login (adds to pool dynamically)
export async function loginAndAddToken(email, password) {
  const token = await login(email, password);
  const existing = tokenPool.find(t => t.token === token);
  if (!existing) {
    const vision = await checkVisionCapability(token);
    tokenPool.push({ token, email, password, visionCapable: vision, lastUsed: 0, errorCount: 0, activeRequests: 0, dead: false });
    persistTokensToEnv();
  }
  return token;
}

function persistTokensToEnv() {
  try {
    let content = readFileSync(ENV_PATH, 'utf-8');
    const aliveTokens = tokenPool.filter(t => t.token && !t.dead).map(t => t.token);
    if (aliveTokens.length === 0) return;

    const line = `DS_TOKENS=${aliveTokens.join(',')}`;
    const lines = content.split(/\r?\n/);
    let found = false;
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].startsWith('DS_TOKENS=')) {
        lines[i] = line;
        found = true;
        break;
      }
    }
    if (!found) lines.push(line);

    writeFileSync(ENV_PATH, lines.join('\n'));
  } catch (err) {
    console.warn('Failed to persist tokens to .env:', err.message);
  }
}

export async function addTokenToPool(tokenStr) {
  const trimmed = tokenStr.trim();
  const existing = tokenPool.find(t => t.token === trimmed);
  if (existing) return existing;
  const vision = await checkVisionCapability(trimmed);
  const entry = { token: trimmed, email: null, password: null, visionCapable: vision, lastUsed: 0, errorCount: 0, activeRequests: 0, dead: false };
  tokenPool.push(entry);
  persistTokensToEnv();
  return entry;
}

// Periodic health check — validate alive tokens and detect banned accounts early
// Only checks tokens that haven't been used recently (idle tokens) to reduce request volume
const HEALTH_CHECK_INTERVAL = parseInt(process.env.HEALTH_CHECK_INTERVAL || '600', 10) * 1000; // seconds, default 10 min
const IDLE_THRESHOLD = parseInt(process.env.IDLE_THRESHOLD || '1800', 10) * 1000; // seconds, default 30 min

async function healthCheck() {
  const now = Date.now();
  // Only check idle tokens — recently used ones are assumed valid
  const idle = tokenPool.filter(t => !t.dead && t.token && (now - t.lastUsed) > IDLE_THRESHOLD);
  if (idle.length === 0) return;

  console.log(`Health check: ${idle.length} idle tokens (of ${tokenPool.filter(t => !t.dead).length} alive)`);

  for (const entry of idle) {
    const valid = await validateToken(entry.token);
    if (!valid) {
      console.log(`Health check: ${entry.token.slice(0, 12)}... INVALID — ${entry.password ? 'refreshing' : 'marking dead'}`);
      if (entry.password) {
        const ok = await refreshToken(entry);
        if (!ok && entry.dead) {
          console.log(`Health check: ${entry.email} is BANNED, removing from pool`);
          const idx = tokenPool.indexOf(entry);
          if (idx !== -1) tokenPool.splice(idx, 1);
        }
      } else {
        entry.dead = true;
        entry.errorCount = TOKEN_DEAD_THRESHOLD;
      }
    } else {
      entry.lastUsed = now; // reset idle timer on successful check
    }
  }
}

let healthCheckTimer = null;

export function startHealthCheck() {
  if (healthCheckTimer) return;
  healthCheckTimer = setInterval(healthCheck, HEALTH_CHECK_INTERVAL);
  console.log(`Health check enabled: every ${HEALTH_CHECK_INTERVAL / 1000}s`);
}

export function stopHealthCheck() {
  if (healthCheckTimer) {
    clearInterval(healthCheckTimer);
    healthCheckTimer = null;
  }
}

export function getPoolInfo() {
  return tokenPool.filter(t => !t.dead).map(t => ({
    token: t.token ? t.token.slice(0, 12) + '...' : 'NONE',
    email: t.email || null,
    visionCapable: t.visionCapable,
    errorCount: t.errorCount,
    activeRequests: t.activeRequests,
    dead: t.dead,
    maxConcurrent: MAX_CONCURRENT_PER_TOKEN,
  }));
}

export function getAliveTokens() {
  return tokenPool.filter(t => !t.dead && t.token).map(t => t.token);
}

export function getTotalCapacity() {
  return tokenPool.filter(t => !t.dead && t.token).length * MAX_CONCURRENT_PER_TOKEN;
}
