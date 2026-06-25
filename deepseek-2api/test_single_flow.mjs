// Early standalone PoW test harness. Read the token from the environment
// rather than hardcoding it — never commit real credentials.
// Usage: DS_TOKEN=<your_token> node test_single_flow.mjs
const token = process.env.DS_TOKEN || 'YOUR_TOKEN_HERE';

// Inline the PoW solver to test independently
const RC = [
  0x0000000000000001n, 0x0000000000008082n, 0x800000000000808an, 0x8000000080008000n,
  0x000000000000808bn, 0x0000000080000001n, 0x8000000080008081n, 0x8000000000008009n,
  0x000000000000008an, 0x0000000000000088n, 0x0000000080008009n, 0x000000008000000an,
  0x000000008000808bn, 0x800000000000008bn, 0x8000000000008089n, 0x8000000000008003n,
  0x8000000000008002n, 0x8000000000000080n, 0x000000000000800an, 0x800000008000000an,
  0x8000000080008081n, 0x8000000000008080n, 0x0000000080000001n, 0x8000000080008008n,
];
const MASK = 0xffffffffffffffffn;
const RATE = 136;

function rotl64(v, k) { return ((v << BigInt(k)) | (v >> BigInt(64 - k))) & MASK; }
function keccakF23(s) {
  let a = s.slice();
  for (let r = 1; r < 24; r++) {
    const c = [a[0]^a[5]^a[10]^a[15]^a[20], a[1]^a[6]^a[11]^a[16]^a[21], a[2]^a[7]^a[12]^a[17]^a[22],
               a[3]^a[8]^a[13]^a[18]^a[23], a[4]^a[9]^a[14]^a[19]^a[24]];
    const d = [c[4]^rotl64(c[1],1), c[0]^rotl64(c[2],1), c[1]^rotl64(c[3],1), c[2]^rotl64(c[4],1), c[3]^rotl64(c[0],1)];
    for (let i = 0; i < 25; i++) a[i] = (a[i] ^ d[i%5]) & MASK;
    const b = new Array(25);
    b[0]=a[0];b[10]=rotl64(a[1],1);b[20]=rotl64(a[2],62);b[5]=rotl64(a[3],28);b[15]=rotl64(a[4],27);
    b[16]=rotl64(a[5],36);b[1]=rotl64(a[6],44);b[11]=rotl64(a[7],6);b[21]=rotl64(a[8],55);b[6]=rotl64(a[9],20);
    b[7]=rotl64(a[10],3);b[17]=rotl64(a[11],10);b[2]=rotl64(a[12],43);b[12]=rotl64(a[13],25);b[22]=rotl64(a[14],39);
    b[23]=rotl64(a[15],41);b[8]=rotl64(a[16],45);b[18]=rotl64(a[17],15);b[3]=rotl64(a[18],21);b[13]=rotl64(a[19],8);
    b[14]=rotl64(a[20],18);b[24]=rotl64(a[21],2);b[9]=rotl64(a[22],61);b[19]=rotl64(a[23],56);b[4]=rotl64(a[24],14);
    for (let y = 0; y < 5; y++) {
      const t = [b[y*5], b[y*5+1], b[y*5+2], b[y*5+3], b[y*5+4]];
      for (let x = 0; x < 5; x++) a[y*5+x] = (t[x] ^ ((t[(x+1)%5] ^ MASK) & t[(x+2)%5])) & MASK;
    }
    a[0] = (a[0] ^ RC[r]) & MASK;
  }
  for (let i = 0; i < 25; i++) s[i] = a[i];
}
function readU64LE(buf, off) { let v = 0n; for (let i = 0; i < 8; i++) v |= BigInt(buf[off + i]) << BigInt(i * 8); return v & MASK; }
function writeU64LE(val, buf, off) { const v = val & MASK; for (let i = 0; i < 8; i++) buf[off + i] = Number((v >> BigInt(i * 8)) & 0xffn); }

function deepSeekHashV1(data) {
  const s = new Array(25).fill(0n);
  const buf = Buffer.from(data);
  let off = 0;
  while (off + RATE <= buf.length) { for (let i = 0; i < RATE / 8; i++) s[i] = (s[i] ^ readU64LE(buf, off + i * 8)) & MASK; keccakF23(s); off += RATE; }
  const tail = Buffer.alloc(RATE); buf.copy(tail, 0, off); tail[buf.length - off] = 0x06; tail[RATE - 1] |= 0x80;
  for (let i = 0; i < RATE / 8; i++) s[i] = (s[i] ^ readU64LE(tail, i * 8)) & MASK; keccakF23(s);
  const out = Buffer.alloc(32); for (let i = 0; i < 4; i++) writeU64LE(s[i], out, i * 8); return out;
}

function solvePow(challengeHex, salt, expireAt, difficulty) {
  const target = Buffer.from(challengeHex, 'hex');
  const prefix = `${salt}_${expireAt}_`;
  console.log('Solving PoW, prefix:', prefix);
  const t0 = Date.now();
  for (let n = 0; n < difficulty; n++) {
    const hash = deepSeekHashV1(prefix + n.toString());
    if (hash.equals(target)) {
      console.log('Found nonce:', n, 'in', Date.now() - t0, 'ms');
      return n;
    }
  }
  throw new Error('PoW failed');
}

async function test() {
  // Step 1: Get challenge ONCE
  const r1 = await fetch('https://chat.deepseek.com/api/v0/chat/create_pow_challenge', {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      'authorization': 'Bearer ' + token,
      'x-app-version': '2.0.0',
      'x-client-version': '2.0.0',
      'x-client-platform': 'web',
    },
    body: JSON.stringify({ target_path: '/api/v0/chat/completion' }),
  });
  const j1 = await r1.json();
  const c = j1.data.biz_data.challenge;
  console.log('Challenge:', c.challenge.slice(0, 20), 'difficulty:', c.difficulty);

  // Step 2: Solve
  const answer = solvePow(c.challenge, c.salt, c.expire_at, c.difficulty);
  const powResponse = Buffer.from(JSON.stringify({
    algorithm: c.algorithm,
    challenge: c.challenge,
    salt: c.salt,
    answer: answer,
    signature: c.signature,
    target_path: c.target_path,
  })).toString('base64');
  console.log('PoW b64 (first 50):', powResponse.slice(0, 50));

  // Verify: decode and check challenge matches
  const decoded = JSON.parse(Buffer.from(powResponse, 'base64').toString());
  console.log('Decoded challenge matches:', decoded.challenge === c.challenge);
  console.log('Decoded answer:', decoded.answer);

  // Step 3: Create session
  const r2 = await fetch('https://chat.deepseek.com/api/v0/chat_session/create', {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      'authorization': 'Bearer ' + token,
      'x-app-version': '2.0.0',
      'x-client-version': '2.0.0',
      'x-client-platform': 'web',
    },
    body: JSON.stringify({}),
  });
  const j2 = await r2.json();
  const sid = j2.data.biz_data.chat_session.id;
  console.log('Session:', sid);

  // Step 4: Call completion WITH PoW
  const r3 = await fetch('https://chat.deepseek.com/api/v0/chat/completion', {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      'authorization': 'Bearer ' + token,
      'x-ds-pow-response': powResponse,
      'x-app-version': '2.0.0',
      'x-client-version': '2.0.0',
      'x-client-platform': 'web',
      'x-client-locale': 'zh_CN',
      'accept': 'text/event-stream',
    },
    body: JSON.stringify({
      chat_session_id: sid,
      parent_message_id: null,
      model_type: 'default',
      prompt: '1+1=?',
      ref_file_ids: [],
      thinking_enabled: false,
      search_enabled: false,
      preempt: false,
    }),
  });
  console.log('Status:', r3.status);
  console.log('CT:', r3.headers.get('content-type'));
  const text = await r3.text();
  console.log('Response:', text.slice(0, 800));
}

test().catch(e => console.error('Error:', e.message));
