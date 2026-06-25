// The guest challenge doesn't work with these paths. Let me look at the JS bundle more carefully
// for what the actual auth flow looks like now.

// Key finding from the bundle analysis:
// - AWS WAF captcha SDK is loaded: "https://a0fea896111e.edge.captcha-sdk.awswaf.com/a0fea896111e/jsapi.js"
// - There's a silent WAF challenge: "https://a0fea896111e.edge.sdk.awswaf.com/a0fea896111e/3a8b889b892b/challenge.js"
// - This means the WAF requires solving a challenge to get a token/cookie
// - The 40003 might be because we're missing the WAF token cookie

// Let's check what cookies the WAF sets
async function testWAF() {
  const r = await fetch('https://chat.deepseek.com/', {
    headers: {
      'user-agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36',
    },
  });

  // Check Set-Cookie headers
  const setCookies = r.headers.getSetCookie?.() || [];
  console.log('Set-Cookie headers:', setCookies.length);
  for (const c of setCookies) {
    console.log('Cookie:', c.slice(0, 200));
  }

  // Check if there's a WAF challenge in the response
  const html = await r.text();
  const wafRefs = html.match(/awswaf|captcha-sdk|challenge\.js/gi);
  console.log('WAF refs in HTML:', wafRefs);
}

testWAF().catch(e => console.error(e));
