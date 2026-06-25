import { pickToken, setRequestToken } from './auth.js';
import { solvePowChallengeForUpload } from './pow.js';
import { apiHeaders, getHeaders, proxiedFetch } from './headers.js';

const BASE_URL = 'https://chat.deepseek.com';

// Strip characters that could break out of the Content-Disposition /
// Content-Type header fields in the hand-built multipart body.
function sanitizeHeaderField(s) {
  return String(s).replace(/["\r\n]/g, '');
}

export async function uploadFile(fileBuffer, filename, mimeType, token) {
  const powResponse = await solvePowChallengeForUpload(token);

  const safeName = sanitizeHeaderField(filename);
  const safeMime = sanitizeHeaderField(mimeType);
  const boundary = `----FormBoundary${Date.now()}`;
  const header = Buffer.from(
    `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${safeName}"\r\nContent-Type: ${safeMime}\r\n\r\n`
  );
  const footer = Buffer.from(`\r\n--${boundary}--\r\n`);
  const body = Buffer.concat([header, fileBuffer, footer]);

  const res = await proxiedFetch(`${BASE_URL}/api/v0/file/upload_file`, {
    method: 'POST',
    headers: {
      ...await apiHeaders(token, {
        'x-ds-pow-response': powResponse,
        'x-file-size': body.length.toString(),
        'x-model-type': 'vision',
        'content-type': `multipart/form-data; boundary=${boundary}`,
      }),
    },
    body,
  });

  const json = await res.json();
  const fileId = json.data?.biz_data?.id;
  if (!fileId) throw new Error(`File upload failed: ${JSON.stringify(json)}`);

  return fileId;
}

export async function waitForFileReady(fileId, token, maxAttempts = 30, intervalMs = 2000) {
  for (let i = 0; i < maxAttempts; i++) {
    const res = await proxiedFetch(`${BASE_URL}/api/v0/file/fetch_files?file_ids=${fileId}`, {
      headers: await getHeaders(token),
    });
    const json = await res.json();
    const files = json.data?.biz_data?.files;
    if (files && files.length > 0) {
      const status = files[0].status?.toUpperCase();
      if (status === 'SUCCESS' || status === 'PROCESSED' || status === 'READY' || status === 'DONE') {
        return files[0];
      }
      if (status === 'FAILED') {
        throw new Error(`File processing failed: ${files[0].error_code || 'unknown'}`);
      }
    }

    await new Promise(r => setTimeout(r, intervalMs));
  }

  throw new Error(`File not ready after ${maxAttempts * intervalMs / 1000}s`);
}

export async function uploadImageFromFile(fileBuffer, filename, mimeType, token) {
  const fileId = await uploadFile(fileBuffer, filename, mimeType, token);
  await waitForFileReady(fileId, token);
  return fileId;
}

const MIME_MAP = {
  'jpg': 'image/jpeg',
  'jpeg': 'image/jpeg',
  'png': 'image/png',
  'gif': 'image/gif',
  'webp': 'image/webp',
  'svg': 'image/svg+xml',
  'bmp': 'image/bmp',
};

function extFromDataUrl(dataUrl) {
  const match = dataUrl.match(/^data:(image\/\w+);/);
  if (!match) return { ext: 'png', mime: 'image/png' };
  const mime = match[1];
  const ext = mime.split('/')[1] || 'png';
  return { ext: ext === 'jpeg' ? 'jpg' : ext, mime };
}

export async function resolveImageToRefId(imageUrl, token) {
  let buffer, filename, mimeType;

  if (imageUrl.startsWith('data:')) {
    const { ext, mime } = extFromDataUrl(imageUrl);
    const base64 = imageUrl.split(',')[1];
    if (!base64) throw new Error('Invalid data URL: no base64 data');
    buffer = Buffer.from(base64, 'base64');
    filename = `image.${ext}`;
    mimeType = mime;
  } else {
    const res = await proxiedFetch(imageUrl);
    if (!res.ok) throw new Error(`Failed to download image: ${res.status}`);
    const contentType = res.headers.get('content-type') || 'image/png';
    buffer = Buffer.from(await res.arrayBuffer());
    const urlPath = new URL(imageUrl).pathname;
    const ext = urlPath.split('.').pop()?.toLowerCase() || 'png';
    const mappedMime = MIME_MAP[ext] || contentType;
    filename = `image.${mappedMime.split('/')[1] === 'jpeg' ? 'jpg' : (mappedMime.split('/')[1] || 'png')}`;
    mimeType = mappedMime;
  }

  return uploadImageFromFile(buffer, filename, mimeType, token);
}
