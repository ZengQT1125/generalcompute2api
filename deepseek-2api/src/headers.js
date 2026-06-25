// Mimic real Chrome 120 browser session
const UA_VERSION = '120.0.0.0';
const UA_MAJOR = '120';

const BROWSER_HEADERS = {
  'user-agent': `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${UA_VERSION} Safari/537.36`,
  'accept-language': 'zh-CN,zh;q=0.9',
  'accept-encoding': 'gzip, deflate, br',
  'origin': 'https://chat.deepseek.com',
  'referer': 'https://chat.deepseek.com/',
  'sec-ch-ua': `"Not_A Brand";v="8", "Chromium";v="${UA_MAJOR}", "Google Chrome";v="${UA_MAJOR}"`,
  'sec-ch-ua-mobile': '?0',
  'sec-ch-ua-platform': '"Windows"',
  'sec-fetch-dest': 'empty',
  'sec-fetch-mode': 'cors',
  'sec-fetch-site': 'same-origin',
  'x-client-timezone-offset': '28800',
};

// Per-token cookie jar: smidV2, HWWAFSESTIME, HWWAFSESID, ds_session_id, thumbcache
const tokenCookies = new Map();

function randomHex(len) {
  const chars = '0123456789abcdef';
  let s = '';
  for (let i = 0; i < len; i++) s += chars[Math.floor(Math.random() * 16)];
  return s;
}

function randomAlphaNum(len) {
  const chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
  let s = '';
  for (let i = 0; i < len; i++) s += chars[Math.floor(Math.random() * chars.length)];
  return s;
}

function ensureCookies(token) {
  if (tokenCookies.has(token)) return tokenCookies.get(token);
  const smidV2 = `20260520${randomAlphaNum(10)}${randomHex(24)}`;
  const HWWAFSESTIME = `${Date.now()}`;
  const HWWAFSESID = `${randomAlphaNum(4)}${randomHex(12)}`;
  const dsSessionId = `${randomHex(32)}`;
  const thumbcacheKey = randomHex(32);
  const deviceId = generateDeviceId();
  const thumbcacheValue = deviceId;
  const cookie = `smidV2=${smidV2}; HWWAFSESTIME=${HWWAFSESTIME}; HWWAFSESID=${HWWAFSESID}; ds_session_id=${dsSessionId}; .thumbcache_${thumbcacheKey}=${encodeURIComponent(thumbcacheValue)}`;
  tokenCookies.set(token, { cookie, deviceId });
  return tokenCookies.get(token);
}

// Generate deviceId from fp-it-acc.portal101.cn format (base64-like)
function generateDeviceId() {
  const bytes = new Uint8Array(48);
  for (let i = 0; i < 48; i++) bytes[i] = Math.floor(Math.random() * 256);
  return Buffer.from(bytes).toString('base64').replace(/=/g, '') + '==';
}

export function getDeviceIdForToken(token) {
  const cached = tokenCookies.get(token);
  return cached?.deviceId || null;
}

// HIF (Hidden Integration Feature) token management
// DeepSeek uses hif-leim and hif-dliq headers for request validation
// These are fetched from hif-leim.deepseek.com/query and hif-dliq.deepseek.com/query
const HIF_BASE = 'https://hif-leim.deepseek.com/query';
const HIF_DLIQ_BASE = 'https://hif-dliq.deepseek.com/query';

const hifCache = new Map(); // key: token, value: { leim, dliq, expiresAt }

async function fetchHifToken(url, token) {
  const headers = {
    'user-agent': BROWSER_HEADERS['user-agent'],
    'accept': '*/*',
    'accept-language': 'zh-CN,zh;q=0.9',
    'accept-encoding': 'gzip, deflate, br',
    'origin': 'https://chat.deepseek.com',
    'referer': 'https://chat.deepseek.com/',
  };
  if (token) {
    headers['authorization'] = `Bearer ${token}`;
  }
  const res = await proxiedFetch(url, { headers });
  const json = await res.json();
  const value = json.data?.biz_data?.value;
  const ttl = parseInt(res.headers.get('x-hif-ttl') || '600', 10);
  return { value, ttl: (isNaN(ttl) || ttl <= 0) ? 600 : ttl };
}

async function refreshHifTokens(token) {
  try {
    const [leimResult, dliqResult] = await Promise.allSettled([
      fetchHifToken(HIF_BASE, token),
      fetchHifToken(HIF_DLIQ_BASE, token),
    ]);

    const leim = leimResult.status === 'fulfilled' ? leimResult.value : null;
    const dliq = dliqResult.status === 'fulfilled' ? dliqResult.value : null;

    if (leim?.value && dliq?.value) {
      const ttl = Math.min(leim.ttl, dliq.ttl);
      hifCache.set(token, {
        leim: leim.value,
        dliq: dliq.value,
        expiresAt: Date.now() + ttl * 1000,
      });
    } else if (leim?.value || dliq?.value) {
      // Cache partial result — at least one header is available
      const existing = hifCache.get(token) || {};
      hifCache.set(token, {
        leim: leim?.value || existing.leim,
        dliq: dliq?.value || existing.dliq,
        expiresAt: Date.now() + Math.min(leim?.ttl || 600, dliq?.ttl || 600) * 1000,
      });
    }
  } catch (e) {
    // HIF fetch failed — requests will proceed without it
  }
}

async function getHifHeaders(token) {
  const cached = hifCache.get(token);
  if (cached && cached.expiresAt > Date.now()) {
    return { 'x-hif-leim': cached.leim, 'x-hif-dliq': cached.dliq };
  }

  await refreshHifTokens(token);
  const refreshed = hifCache.get(token);
  if (refreshed) {
    return { 'x-hif-leim': refreshed.leim, 'x-hif-dliq': refreshed.dliq };
  }
  return {};
}

// Proxy dispatcher for bypassing IP-based rate limits
let proxyDispatcher = null;

export async function getDispatcher() {
  if (proxyDispatcher !== null) return proxyDispatcher;
  const proxyUrl = process.env.HTTPS_PROXY || process.env.https_proxy || process.env.HTTP_PROXY || process.env.http_proxy;
  if (!proxyUrl) {
    proxyDispatcher = false;
    return false;
  }
  try {
    const { ProxyAgent } = await import('undici');
    proxyDispatcher = new ProxyAgent(proxyUrl);
    console.log(`Proxy enabled: ${proxyUrl}`);
    return proxyDispatcher;
  } catch (e) {
    throw new Error(`Failed to init proxy (${proxyUrl}): ${e.message}`);
  }
}

// Common headers for API requests
export async function apiHeaders(token, extra = {}) {
  const cookieData = ensureCookies(token);
  const hifHeaders = await getHifHeaders(token);
  return {
    'authorization': `Bearer ${token}`,
    'content-type': 'application/json',
    'accept': '*/*',
    'x-app-version': '2.0.0',
    'x-client-version': '2.0.0',
    'x-client-platform': 'web',
    'x-client-locale': 'zh_CN',
    'cookie': cookieData.cookie,
    ...BROWSER_HEADERS,
    ...hifHeaders,
    ...extra,
  };
}

// Headers for SSE streaming requests
export async function streamHeaders(token, powResponse, extra = {}) {
  const cookieData = ensureCookies(token);
  const hifHeaders = await getHifHeaders(token);
  return {
    'authorization': `Bearer ${token}`,
    'content-type': 'application/json',
    'x-ds-pow-response': powResponse,
    'accept': 'text/event-stream',
    'x-app-version': '2.0.0',
    'x-client-version': '2.0.0',
    'x-client-platform': 'web',
    'x-client-locale': 'zh_CN',
    'cookie': cookieData.cookie,
    ...BROWSER_HEADERS,
    ...hifHeaders,
    ...extra,
  };
}

// Headers for GET requests (no content-type)
export async function getHeaders(token, extra = {}) {
  const cookieData = ensureCookies(token);
  return {
    'authorization': `Bearer ${token}`,
    'accept': '*/*',
    'x-app-version': '2.0.0',
    'x-client-version': '2.0.0',
    'x-client-platform': 'web',
    'x-client-locale': 'zh_CN',
    'cookie': cookieData.cookie,
    ...BROWSER_HEADERS,
    ...extra,
  };
}

// Headers for login (no token)
export function loginHeaders(extra = {}) {
  return {
    'content-type': 'application/json',
    'accept': '*/*',
    'x-app-version': '2.0.0',
    'x-client-version': '2.0.0',
    'x-client-platform': 'web',
    'x-client-locale': 'zh_CN',
    ...BROWSER_HEADERS,
    ...extra,
  };
}

// Persistent device ID (per process lifetime) — used for settings endpoint
const deviceId = randomHex(8) + '-' + randomHex(4) + '-' + randomHex(4) + '-' + randomHex(4) + '-' + randomHex(12);
export function getDeviceId() { return deviceId; }

// Wrap fetch to use proxy dispatcher when available
export async function proxiedFetch(url, options = {}) {
  const dispatcher = await getDispatcher();
  if (dispatcher) {
    options.dispatcher = dispatcher;
  }
  return fetch(url, options);
}
