export interface Env {
  HLS_BUCKET: R2Bucket;
  PLAYBACK_TOKEN_SECRET: string;
  PLAYBACK_ALLOWED_ORIGIN: string;
}

type TokenPayload = {
  sub: string;
  uid?: string;
  path: string;
  exp: number;
  ipHash?: string;
  uaHash?: string;
};

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    if (request.method === 'OPTIONS') {
      return withCors(new Response(null, { status: 204 }), env);
    }

    const url = new URL(request.url);
    if (!url.pathname.startsWith('/hls/')) {
      return withCors(new Response('Not found', { status: 404 }), env);
    }

    try {
      const token = url.searchParams.get('token');
      if (!token) {
        return withCors(new Response('Missing token', { status: 403 }), env);
      }

      const objectKey = decodeURIComponent(url.pathname.replace(/^\/hls\//, ''));
      const payload = await verifyToken(token, env.PLAYBACK_TOKEN_SECRET);
      if (!objectKey.startsWith(payload.path)) {
        return withCors(new Response('Forbidden', { status: 403 }), env);
      }

      const object = await env.HLS_BUCKET.get(objectKey);
      if (!object) {
        return withCors(new Response('Not found', { status: 404 }), env);
      }

      const headers = new Headers();
      object.writeHttpMetadata(headers);
      headers.set('X-Content-Type-Options', 'nosniff');

      if (objectKey.endsWith('.m3u8')) {
        headers.set('Cache-Control', 'private, no-store');
        headers.set('Content-Type', 'application/vnd.apple.mpegurl');
        const playlistText = await object.text();
        return withCors(
          new Response(rewritePlaylist(playlistText, url, token), { status: 200, headers }),
          env
        );
      }

      headers.set('Cache-Control', 'private, max-age=300');
      return withCors(new Response(object.body, { status: 200, headers }), env);
    } catch {
      return withCors(new Response('Forbidden', { status: 403 }), env);
    }
  },
};

async function verifyToken(token: string, secret: string): Promise<TokenPayload> {
  const [encodedPayload, encodedSignature] = token.split('.');
  if (!encodedPayload || !encodedSignature) {
    throw new Error('Invalid token format');
  }

  const expected = await hmacSha256(secret, encodedPayload);
  const actual = fromBase64Url(encodedSignature);
  if (!timingSafeEqual(expected, actual)) {
    throw new Error('Invalid token signature');
  }

  const payload = JSON.parse(new TextDecoder().decode(fromBase64Url(encodedPayload))) as TokenPayload;
  if (payload.exp <= Math.floor(Date.now() / 1000)) {
    throw new Error('Token expired');
  }

  return payload;
}

function rewritePlaylist(playlistText: string, requestURL: URL, token: string) {
  return playlistText
    .split('\n')
    .map((line) => {
      const trimmed = line.trim();
      if (!trimmed) {
        return line;
      }
      if (trimmed.startsWith('#EXT-X-KEY:')) {
        return rewriteTagURI(line, requestURL, token);
      }
      if (trimmed.startsWith('#')) {
        return line;
      }
      return appendToken(line, requestURL, token);
    })
    .join('\n');
}

function rewriteTagURI(line: string, requestURL: URL, token: string) {
  return line.replace(/URI="([^"]+)"/, (_match, uri) => {
    return `URI="${appendToken(uri, requestURL, token)}"`;
  });
}

function appendToken(target: string, requestURL: URL, token: string) {
  const resolved = new URL(target, requestURL);
  resolved.searchParams.set('token', token);
  return resolved.toString();
}

async function hmacSha256(secret: string, value: string) {
  const key = await crypto.subtle.importKey(
    'raw',
    new TextEncoder().encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const signature = await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(value));
  return new Uint8Array(signature);
}

function fromBase64Url(value: string) {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/');
  const padding = normalized.length % 4 === 0 ? '' : '='.repeat(4 - (normalized.length % 4));
  const raw = atob(`${normalized}${padding}`);
  return Uint8Array.from(raw, (char) => char.charCodeAt(0));
}

function timingSafeEqual(a: Uint8Array, b: Uint8Array) {
  if (a.length !== b.length) {
    return false;
  }

  let result = 0;
  for (let index = 0; index < a.length; index += 1) {
    result |= a[index] ^ b[index];
  }
  return result === 0;
}

function withCors(response: Response, env: Env) {
  const headers = new Headers(response.headers);
  headers.set('Access-Control-Allow-Origin', env.PLAYBACK_ALLOWED_ORIGIN);
  headers.set('Access-Control-Allow-Methods', 'GET,HEAD,OPTIONS');
  headers.set('Access-Control-Allow-Headers', 'Content-Type,Range');
  headers.set('Vary', 'Origin');
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}
