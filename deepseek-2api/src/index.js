import { config } from 'dotenv';
config();

import express from 'express';
import { initTokenPool, getPoolInfo, getTotalCapacity, addTokenToPool, loginAndAddToken, getAliveTokens, startHealthCheck } from './auth.js';
import { prewarmSessions, getSessionInfo } from './session.js';
import { getConversationInfo } from './conversation.js';
import { handleOpenAICompletion, handleOpenAIModels } from './openai.js';
import { handleDeepSeekCompletion } from './deepseek.js';
import { getQueueInfo } from './queue.js';
import { requestLogger, getRecentLogs, getLogStats, readHistoricalLogs, readChatLogs, listLogDates } from './logger.js';
import { getMetrics, getTimeseries } from './metrics.js';
import { getDispatcher } from './headers.js';
import { fileURLToPath } from 'url';
import { dirname, join } from 'path';
import dns from 'dns';

// 拦截 dns.lookup 以支持自定义域名解析（hosts 功能），绕过特定区域的网络限制
const originalLookup = dns.lookup;
dns.lookup = function(hostname, options, callback) {
  if (typeof options === 'function') {
    callback = options;
    options = {};
  }

  // 从环境变量 DNS_RESOLVE 中读取解析配置，格式为 domain:ip,domain:ip
  const dnsResolve = process.env.DNS_RESOLVE;
  if (dnsResolve) {
    const mappings = dnsResolve.split(',').map(m => m.trim());
    for (const mapping of mappings) {
      const [domain, ip] = mapping.split(':');
      if (domain === hostname && ip) {
        const family = ip.includes('.') ? 4 : 6;
        if (options && options.all) {
          return callback(null, [{ address: ip, family }]);
        }
        return callback(null, ip, family);
      }
    }
  }

  return originalLookup.call(dns, hostname, options, callback);
};

const __dirname = dirname(fileURLToPath(import.meta.url));
const startTime = Date.now();

const app = express();
const PORT = process.env.PORT || 3000;

app.use(express.json({ limit: '50mb' }));

// Request logging (writes to /srv/threadripper-backups/newapi/logs/deepseek-2api/)
app.use(requestLogger('deepseek-2api'));

// API Key auth middleware
app.use((req, res, next) => {
  const apiKey = process.env.API_KEY;
  if (!apiKey) return next();
  if (req.method === 'GET' && (req.path === '/admin' || req.path === '/admin/chat')) return next();

  const auth = req.headers['authorization'];
  if (auth === `Bearer ${apiKey}`) return next();

  res.status(401).json({ error: { message: 'Invalid API key' } });
});

// OpenAI format
app.post('/v1/chat/completions', handleOpenAICompletion);
app.get('/v1/models', handleOpenAIModels);

// DeepSeek native format
app.post('/api/v0/chat/completion', handleDeepSeekCompletion);

// Health check + pool info
app.get('/', (req, res) => {
  res.json({
    status: 'ok',
    version: '2.0.0',
    pool: getPoolInfo(),
    totalCapacity: getTotalCapacity(),
    queue: getQueueInfo(),
  });
});

// Admin panel
app.get('/admin', (req, res) => {
  res.sendFile(join(__dirname, 'admin', 'index.html'));
});

app.get('/admin/chat', (req, res) => {
  res.sendFile(join(__dirname, 'admin', 'chat.html'));
});

app.get('/admin/api/stats', (req, res) => {
  const uptimeSeconds = Math.floor((Date.now() - startTime) / 1000);
  res.json({
    status: 'ok',
    version: '2.0.0',
    uptimeSeconds,
    pool: getPoolInfo(),
    totalCapacity: getTotalCapacity(),
    queue: getQueueInfo(),
    sessions: getSessionInfo(),
    conversations: getConversationInfo(),
    logStats: getLogStats(),
  });
});

app.get('/admin/api/logs', (req, res) => {
  const count = Math.min(parseInt(req.query.count) || 50, 200);
  res.json({ logs: getRecentLogs(count), stats: getLogStats() });
});

app.get('/admin/api/logs/dates', (req, res) => {
  res.json({ dates: listLogDates() });
});

app.get('/admin/api/logs/history', (req, res) => {
  const { date } = req.query;
  if (!date) return res.status(400).json({ error: { message: 'date param required (YYYY-MM-DD)' } });
  const count = Math.min(parseInt(req.query.count) || 100, 10000);
  res.json({ logs: readHistoricalLogs(date, count) });
});

app.get('/admin/api/logs/chats', (req, res) => {
  const date = req.query.date || new Date().toISOString().slice(0, 10);
  const count = Math.min(parseInt(req.query.count) || 100, 10000);
  res.json({ chats: readChatLogs(date, count), total: count });
});

app.post('/admin/api/token/add', async (req, res) => {
  const { token } = req.body;
  if (!token || typeof token !== 'string') {
    return res.status(400).json({ error: { message: 'token required' } });
  }
  try {
    const added = await addTokenToPool(token);
    res.json({ success: true, visionCapable: added.visionCapable });
  } catch (err) {
    res.status(500).json({ error: { message: err.message } });
  }
});

app.post('/admin/api/token/login', async (req, res) => {
  const { email, password } = req.body;
  if (!email || !password) {
    return res.status(400).json({ error: { message: 'email and password required' } });
  }
  try {
    const token = await loginAndAddToken(email, password);
    res.json({ success: true, token: token.slice(0, 12) + '...' });
  } catch (err) {
    res.status(500).json({ error: { message: err.message } });
  }
});

// Performance monitoring
app.get('/performance', (req, res) => {
  res.sendFile(join(__dirname, 'performance', 'index.html'));
});

app.get('/performance/api/metrics', (req, res) => {
  res.json(getMetrics());
});

app.get('/performance/api/timeseries', (req, res) => {
  const range = req.query.range || '6h';
  const points = getTimeseries(range);
  const pool = getPoolInfo();
  const totalCap = getTotalCapacity();
  const queue = getQueueInfo();
  res.json({ points, pool, totalCapacity: totalCap, queue });
});

app.listen(PORT, async () => {
  console.log(`DeepSeek 2API running on http://localhost:${PORT}`);
  console.log(`OpenAI format:  POST /v1/chat/completions`);
  console.log(`DeepSeek format: POST /api/v0/chat/completion`);
  console.log(`Models: GET /v1/models`);
  console.log(`Admin panel: http://localhost:${PORT}/admin`);

  if (!process.env.API_KEY) {
    console.warn('\n⚠️  WARNING: API_KEY is not set — admin endpoints (/admin/api/*, /performance/api/*) are UNAUTHENTICATED.\n' +
      '   Set API_KEY in .env before exposing this service on a public network.\n');
  }

  await getDispatcher();
  await initTokenPool();

  const aliveTokens = getAliveTokens();
  await prewarmSessions(aliveTokens);

  startHealthCheck();
});
