// The forge's HTTP server: health probe, REST v3, GraphQL v4, MCP and git
// smart HTTP, all from one in-memory store. Lifecycle stays in main.ts.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './src/store.ts';
import { handleRest, type RestResult } from './src/rest.ts';
import { handleGraphqlOperation } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const EXPECTED_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

let requestCounter = 0;
function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

/** One Buffer.from per JSON reply: stringify once, send the bytes. */
function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  if (body === null) {
    res.writeHead(status, { 'content-type': JSON_TYPE, 'x-github-request-id': nextRequestId(), ...headers });
    res.end();
    return;
  }
  const buf = Buffer.from(`${JSON.stringify(body)}\n`, 'utf8');
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': String(buf.length),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(buf);
}

/** Send a pre-serialized JSON body (one Buffer.from, no re-stringify). */
function sendText(res: ServerResponse, status: number, text: string, headers: Record<string, string> = {}): void {
  const buf = Buffer.from(text, 'utf8');
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': String(buf.length),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(buf);
}

function sendBytes(res: ServerResponse, status: number, body: Buffer, contentType: string): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': String(body.length),
    'x-github-request-id': nextRequestId(),
  });
  res.end(body);
}

function sendRestResult(res: ServerResponse, result: RestResult): void {
  if (result.status === 204) {
    res.writeHead(204, { 'content-type': JSON_TYPE, 'x-github-request-id': nextRequestId() });
    res.end();
    return;
  }
  if (result.bodyText !== undefined) {
    sendText(res, result.status, result.bodyText, result.headers ?? {});
    return;
  }
  sendJson(res, result.status, result.body, result.headers ?? {});
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > 32 * 1024 * 1024) {
        reject(new Error('body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function bearerToken(req: IncomingMessage): string | null {
  const auth = req.headers.authorization ?? '';
  if (!auth.startsWith('Bearer ')) return null;
  const token = auth.slice('Bearer '.length).trim();
  return token === '' ? null : token;
}

function tokenAccepted(req: IncomingMessage): boolean {
  const token = bearerToken(req);
  if (token === null) return false;
  if (EXPECTED_TOKEN !== '') return token === EXPECTED_TOKEN;
  return true;
}

export interface ParsedUrl {
  path: string;
  segs: string[];
  query: URLSearchParams;
}

/**
 * Manual request-target parsing, replacing `new URL(req.url, ...)` per
 * request. Semantics match URL parsing for the targets the suite sends:
 * the raw (still %-encoded) path split into segments with '.' / '..'
 * resolved and '\' treated as '/', and the query string parsed by
 * URLSearchParams (same '+' / %-decoding and first-value `get` as
 * `URL.searchParams`). Returns null for targets `new URL` would reject
 * (whitespace / control characters) so the caller answers 404 as before.
 */
function parseUrl(rawUrl: string | undefined): ParsedUrl | null {
  const raw = rawUrl ?? '/';
  for (let i = 0; i < raw.length; i++) {
    const c = raw.charCodeAt(i);
    if (c <= 0x20 || c === 0x7f) return null;
  }
  let pathPart = raw;
  let queryString = '';
  const q = raw.indexOf('?');
  if (q >= 0) {
    pathPart = raw.slice(0, q);
    queryString = raw.slice(q + 1);
  }
  const hash = pathPart.indexOf('#');
  if (hash >= 0) pathPart = pathPart.slice(0, hash);
  const hashQ = queryString.indexOf('#');
  if (hashQ >= 0) queryString = queryString.slice(0, hashQ);
  if (pathPart === '') pathPart = '/';
  const path = pathPart.replace(/\\/g, '/');
  const outSegs: string[] = [];
  for (const s of path.split('/')) {
    if (s === '' || s === '.') continue;
    if (s === '..') {
      outSegs.pop();
      continue;
    }
    outSegs.push(s);
  }
  return { path, segs: outSegs, query: new URLSearchParams(queryString) };
}

export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    let out: void | Promise<void>;
    try {
      // Body-less requests run fully synchronously: no async wrapper and no
      // promise per request. Only body reads go async (see below).
      out = handleRequest(store, req, res);
    } catch {
      if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' });
      else res.end();
      return;
    }
    if (out instanceof Promise) {
      out.catch(() => {
        if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' });
        else res.end();
      });
    }
  });
}

function handleRequest(store: Store, req: IncomingMessage, res: ServerResponse): void | Promise<void> {
  const method = req.method ?? 'GET';
  const parsed = parseUrl(req.url);
  if (parsed === null) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  const { path, segs, query } = parsed;

  if (method === 'GET' && path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }

  // MCP: JSON-RPC over Streamable HTTP.
  if (path === '/mcp') {
    if (method === 'DELETE') {
      res.writeHead(200, { 'x-github-request-id': nextRequestId() });
      res.end();
      return;
    }
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    if (!tokenAccepted(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
    return handleMcpPost(store, req, res);
  }

  // GraphQL v4.
  if (path === '/api/graphql') {
    if (method !== 'POST') {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    if (!tokenAccepted(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
    return handleGraphqlPost(store, req, res);
  }

  // REST v3.
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    if (!tokenAccepted(req)) {
      sendJson(res, 401, { message: 'Requires authentication' });
      return;
    }
    const restSegs = segs.slice(2);
    const origin = `http://${req.headers.host ?? '127.0.0.1'}`;
    if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
      return handleRestWithBody(store, req, res, method, restSegs, query, origin);
    }
    const result = handleRest(store, method, restSegs, query, null, origin);
    sendRestResult(res, result);
    return;
  }

  // Git smart HTTP is handled in the second pass below.
  return handleGit(store, req, res, method, path, segs, query);
}

async function handleMcpPost(store: Store, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const raw = await readBody(req);
  let parsed: unknown = null;
  let parseFailed = false;
  if (raw.length > 0) {
    try {
      parsed = JSON.parse(raw.toString('utf8'));
    } catch {
      parseFailed = true;
    }
  }
  if (parseFailed) {
    sendJson(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  const outcome = handleMcp(store, parsed);
  if (outcome.body === null) {
    res.writeHead(outcome.status, { 'x-github-request-id': nextRequestId() });
    res.end();
    return;
  }
  sendJson(res, outcome.status, outcome.body, { 'content-type': JSON_TYPE });
}

async function handleGraphqlPost(store: Store, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const raw = await readBody(req);
  let parsed: unknown = null;
  if (raw.length > 0) {
    try {
      parsed = JSON.parse(raw.toString('utf8'));
    } catch {
      sendJson(res, 400, { message: 'malformed JSON body' });
      return;
    }
  }
  const outcome = handleGraphqlOperation(store, parsed);
  sendJson(res, outcome.status, outcome.body);
}

async function handleRestWithBody(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  method: string,
  restSegs: string[],
  query: URLSearchParams,
  origin: string,
): Promise<void> {
  const raw = await readBody(req);
  let body: Record<string, unknown> | null = null;
  if (raw.length > 0) {
    try {
      const parsed: unknown = JSON.parse(raw.toString('utf8'));
      body = (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : null) as Record<string, unknown> | null;
      if (parsed !== null && (typeof parsed !== 'object' || Array.isArray(parsed))) {
        sendJson(res, 400, { message: 'malformed JSON body' });
        return;
      }
    } catch {
      sendJson(res, 400, { message: 'malformed JSON body' });
      return;
    }
  }
  const result = handleRest(store, method, restSegs, query, body, origin);
  sendRestResult(res, result);
}

// ---- git smart HTTP (appended second pass) ---------------------------------
import {
  applyPush,
  lsRefsResponse,
  parseReceivePack,
  parseV0Fetch,
  parseV2Request,
  pktFlush,
  pushReportResponse,
  v0Advertisement,
  v0FetchResponse,
  v2Advertisement,
  v2FetchResponse,
} from './src/git.ts';
import type { Store } from './src/store.ts';

function handleGit(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  method: string,
  path: string,
  segs: string[],
  query: URLSearchParams,
): void | Promise<void> {
  // /{owner}/{repo}.git/...
  if (segs.length < 2 || !segs[1]?.endsWith('.git')) {
    sendJson(res, 404, { message: 'Not Found' });
    return;
  }
  const owner = decodeURIComponent(segs[0] as string);
  const repoName = decodeURIComponent((segs[1] as string).slice(0, -4));
  const repo = store.getRepo(owner, repoName);
  const tail = segs.slice(2);
  if (method === 'GET' && tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs') {
    const service = query.get('service');
    if (repo === undefined || (service !== 'git-upload-pack' && service !== 'git-receive-pack')) {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    const wantV2 = service === 'git-upload-pack' && (req.headers['git-protocol'] ?? '') === 'version=2';
    if (wantV2) {
      sendBytes(res, 200, v2Advertisement(), 'application/x-git-upload-pack-advertisement');
    } else if (service === 'git-upload-pack') {
      sendBytes(res, 200, v0Advertisement(repo, 'git-upload-pack'), 'application/x-git-upload-pack-advertisement');
    } else {
      sendBytes(res, 200, v0Advertisement(repo, 'git-receive-pack'), 'application/x-git-receive-pack-advertisement');
    }
    return;
  }
  if (method === 'POST' && tail.length === 1 && tail[0] === 'git-upload-pack') {
    if (repo === undefined) {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    return handleUploadPack(store, req, res, repo);
  }
  if (method === 'POST' && tail.length === 1 && tail[0] === 'git-receive-pack') {
    if (repo === undefined) {
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }
    return handleReceivePack(store, req, res, repo);
  }
  sendJson(res, 404, { message: 'Not Found' });
}

async function handleUploadPack(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  repo: NonNullable<ReturnType<Store['getRepo']>>,
): Promise<void> {
  const raw = await readBody(req);
  const isV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
  if (isV2) {
    const parsed = parseV2Request(raw);
    if (parsed === null) {
      sendBytes(res, 200, pktFlush(), 'application/x-git-upload-pack-result');
      return;
    }
    if (parsed.command === 'ls-refs') {
      sendBytes(res, 200, lsRefsResponse(repo, parsed), 'application/x-git-upload-pack-result');
      return;
    }
    if (parsed.command === 'fetch') {
      sendBytes(res, 200, v2FetchResponse(store, repo, parsed), 'application/x-git-upload-pack-result');
      return;
    }
    sendBytes(res, 200, pktFlush(), 'application/x-git-upload-pack-result');
    return;
  }
  const v0 = parseV0Fetch(raw);
  sendBytes(res, 200, v0FetchResponse(store, repo, v0.wants, v0.haves, v0.depth, v0.shallow), 'application/x-git-upload-pack-result');
}

async function handleReceivePack(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  repo: NonNullable<ReturnType<Store['getRepo']>>,
): Promise<void> {
  const raw = await readBody(req);
  const { commands, pack } = parseReceivePack(raw);
  const report = applyPush(store, repo, commands, pack);
  sendBytes(res, 200, pushReportResponse(report), 'application/x-git-receive-pack-result');
}
