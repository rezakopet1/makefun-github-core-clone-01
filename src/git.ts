// Git smart-HTTP protocol (upload-pack v2, receive-pack v0) over the store.
// Plain (non-deltified) packfiles only; deltas on receipt are resolved.
import { createHash } from 'node:crypto';
import {
  touchRepo,
  parseCommit,
  parseTag,
  parseTree,
  readPack,
  writePack,
  ZERO_OID,
  type GitObject,
  type Repo,
  type Store,
} from './store.ts';

const HEX_DIGITS = '0123456789abcdef';

/** 4-char lowercase hex of a pkt-line length (0..65520), as a string. */
function hex4(n: number): string {
  return (
    HEX_DIGITS[(n >> 12) & 0xf] + HEX_DIGITS[(n >> 8) & 0xf] + HEX_DIGITS[(n >> 4) & 0xf] + HEX_DIGITS[n & 0xf]
  );
}

/** Numeric value of one ASCII hex digit, or -1. */
function hexVal(c: number): number {
  if (c >= 48 && c <= 57) return c - 48;
  if (c >= 97 && c <= 102) return c - 87;
  if (c >= 65 && c <= 70) return c - 55;
  return -1;
}

/** One pkt-line frame for a string payload: a single allocation, no concat. */
export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const out = Buffer.allocUnsafe(4 + data.length);
  const len = data.length + 4;
  out[0] = HEX_DIGITS.charCodeAt((len >> 12) & 0xf);
  out[1] = HEX_DIGITS.charCodeAt((len >> 8) & 0xf);
  out[2] = HEX_DIGITS.charCodeAt((len >> 4) & 0xf);
  out[3] = HEX_DIGITS.charCodeAt(len & 0xf);
  data.copy(out, 4);
  return out;
}

/**
 * Frame several ASCII lines as pkt-lines into ONE string, then one Buffer.
 * Byte length (not string length) prefixes each line, so non-ASCII ref names
 * stay correct; the single Buffer.from at the end is the only allocation.
 */
function frameLines(lines: string[]): Buffer {
  let s = '';
  for (const line of lines) s += hex4(Buffer.byteLength(line) + 4) + line;
  return Buffer.from(s, 'utf8');
}

/** Frame lines plus a trailing flush (0000) as one buffer. */
function frameLinesFlush(lines: string[]): Buffer {
  let s = '';
  for (const line of lines) s += hex4(Buffer.byteLength(line) + 4) + line;
  return Buffer.from(`${s}0000`, 'utf8');
}

export const pktFlush = (): Buffer => Buffer.from('0000', 'ascii');
export const pktDelim = (): Buffer => Buffer.from('0001', 'ascii');

export interface Pkt {
  kind: 'data' | 'flush' | 'delim' | 'end';
  payload: Buffer;
}

export function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let off = 0;
  while (off + 4 <= buf.length) {
    const h0 = hexVal(buf[off] as number);
    const h1 = hexVal(buf[off + 1] as number);
    const h2 = hexVal(buf[off + 2] as number);
    const h3 = hexVal(buf[off + 3] as number);
    if (h0 < 0 || h1 < 0 || h2 < 0 || h3 < 0) break;
    const len = (h0 << 12) | (h1 << 8) | (h2 << 4) | h3;
    if (len === 0) {
      out.push({ kind: 'flush', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    if (len === 1) {
      out.push({ kind: 'delim', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    if (len === 2) {
      out.push({ kind: 'end', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    if (off + len > buf.length) break;
    out.push({ kind: 'data', payload: buf.subarray(off + 4, off + len) });
    off += len;
  }
  return out;
}

export function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const cached = repo.sortedRefsCache;
  if (cached !== null) return cached;
  const refs = [...repo.refs.entries()]
    .map(([name, oid]) => ({ name, oid }))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  repo.sortedRefsCache = refs;
  return refs;
}

export function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** v2 capability advertisement body for git-upload-pack: a constant, built once. */
const V2_ADVERTISEMENT: Buffer = (() => {
  const lines = [
    'version 2\n',
    'agent=github-core-clone/1.0\n',
    'ls-refs=unborn\n',
    'fetch=shallow filter\n',
    'server-option\n',
    'object-format=sha1\n',
  ];
  return Buffer.concat([pktLine('# service=git-upload-pack\n'), pktFlush(), frameLinesFlush(lines)]);
})();

export function v2Advertisement(): Buffer {
  return V2_ADVERTISEMENT;
}

/** v0 ref advertisement body for a service; cached per repo version. */
export function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const cached = service === 'git-upload-pack' ? repo.v0UpCache : repo.v0RecvCache;
  if (cached !== null) return cached;
  const refs = sortedRefs(repo);
  const caps =
    service === 'git-receive-pack'
      ? 'report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0'
      : 'side-band-64k multi_ack_detailed thin-pack ofs-delta shallow agent=github-core-clone/1.0';
  const head = headTarget(repo);
  const headOid = repo.refs.get(head);
  const lines: string[] = [];
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    // HEAD first, with symref capability, then refs in sorted order.
    lines.push(`${headOid ?? ZERO_OID} HEAD\0${caps} symref=HEAD:${head}\n`);
    for (const ref of refs) {
      lines.push(`${ref.oid} ${ref.name}\n`);
      const obj = repo.objects.get(ref.oid);
      if (obj && obj.type === 'tag') {
        lines.push(`${repoPeel(repo, ref.oid)} ${ref.name}^{}\n`);
      }
    }
  }
  const out = Buffer.concat([
    pktLine(`# service=${service}\n`),
    pktFlush(),
    frameLinesFlush(lines),
  ]);
  if (service === 'git-upload-pack') repo.v0UpCache = out;
  else repo.v0RecvCache = out;
  return out;
}

function repoPeel(repo: Repo, oid: string): string {
  let cur = oid;
  for (let i = 0; i < 10; i++) {
    const obj = repo.objects.get(cur);
    if (!obj || obj.type !== 'tag') return cur;
    cur = parseTag(obj.data).object;
  }
  return cur;
}

/** Objects to send for wants, minus what the client already holds. */
export function fetchObjects(
  store: Store,
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | null,
  shallowBoundaries: string[],
): { objects: GitObject[]; shallow: string[]; unshallow: string[] } {
  // What the client already holds: reachable from its haves, but never past a
  // shallow boundary (a shallow clone holds the boundary commit and its tree,
  // not the boundary's parents).
  const shallowSet = new Set(shallowBoundaries);
  const haveReachable = new Set<string>();
  {
    const queue = [...haves];
    while (queue.length) {
      const oid = queue.pop() as string;
      if (haveReachable.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      haveReachable.add(oid);
      if (obj.type === 'commit') {
        const c = parseCommit(obj.data);
        for (const t of store.reachable(repo, [c.tree])) haveReachable.add(t);
        if (!shallowSet.has(oid)) for (const p of c.parents) queue.push(p);
      } else if (obj.type === 'tag') {
        queue.push(parseTag(obj.data).object);
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.data)) queue.push(e.oid);
      }
    }
  }
  // Depth handling: walk commits from each want, counting generations.
  const wanted = new Set<string>();
  const shallow = new Set<string>();
  const unshallow = new Set<string>();
  if (depth !== null) {
    // BFS by generation from wants; commits at generation depth-1 are the
    // shallow boundary: their trees/blobs are sent, their parents are not.
    interface Item {
      oid: string;
      gen: number;
    }
    const queue: Item[] = wants.map((w) => ({ oid: w, gen: 0 }));
    const seenCommit = new Set<string>();
    while (queue.length) {
      const { oid, gen } = queue.shift() as Item;
      if (seenCommit.has(oid)) continue;
      seenCommit.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      if (obj.type === 'tag') {
        wanted.add(oid);
        const target = parseTag(obj.data).object;
        if (!seenCommit.has(target)) queue.push({ oid: target, gen });
        continue;
      }
      if (obj.type !== 'commit') {
        wanted.add(oid);
        continue;
      }
      const commit = parseCommit(obj.data);
      if (gen >= depth) continue;
      wanted.add(oid);
      // Add this commit's tree closure.
      for (const t of store.reachable(repo, [commit.tree])) wanted.add(t);
      if (gen === depth - 1) {
        shallow.add(oid);
      } else {
        for (const p of commit.parents) queue.push({ oid: p, gen: gen + 1 });
      }
    }
    // Unshallow: boundaries the client held that are now fully included.
    for (const b of shallowBoundaries) {
      if (seenCommit.has(b) && !shallow.has(b)) unshallow.add(b);
    }
  } else {
    for (const w of wants) {
      for (const oid of store.reachable(repo, [w])) wanted.add(oid);
    }
  }
  const paired: Array<{ obj: GitObject; oid: string }> = [];
  for (const oid of wanted) {
    if (haveReachable.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj) paired.push({ obj, oid });
  }
  // Deterministic pack order: commits, then trees, then blobs, then tags,
  // each sorted by oid (the oid is the map key: no SHA-1 recomputation).
  const rank = (t: string): number => (t === 'commit' ? 0 : t === 'tree' ? 1 : t === 'blob' ? 2 : 3);
  paired.sort((a, b) => {
    const ra = rank(a.obj.type);
    const rb = rank(b.obj.type);
    if (ra !== rb) return ra - rb;
    return a.oid < b.oid ? -1 : a.oid > b.oid ? 1 : 0;
  });
  const objects: GitObject[] = paired.map((p) => p.obj);
  return { objects, shallow: [...shallow].sort(), unshallow: [...unshallow].sort() };
}

export function sidebandPack(pack: Buffer): Buffer {
  // Max pkt-line is 65520 bytes total (4 header + payload); the band byte
  // leaves 65515 bytes of pack data per frame. One allocation per frame.
  const CHUNK = 65515;
  const data = pack.length === 0 ? writePack([]) : pack;
  const frames = Math.max(1, Math.ceil(data.length / CHUNK));
  const parts: Buffer[] = new Array(frames);
  for (let i = 0; i < frames; i++) {
    const off = i * CHUNK;
    const len = Math.min(CHUNK, data.length - off);
    const frame = Buffer.allocUnsafe(4 + 1 + len);
    const total = 5 + len;
    frame[0] = HEX_DIGITS.charCodeAt((total >> 12) & 0xf);
    frame[1] = HEX_DIGITS.charCodeAt((total >> 8) & 0xf);
    frame[2] = HEX_DIGITS.charCodeAt((total >> 4) & 0xf);
    frame[3] = HEX_DIGITS.charCodeAt(total & 0xf);
    frame[4] = 1;
    data.copy(frame, 5, off, off + len);
    parts[i] = frame;
  }
  return Buffer.concat(parts);
}

/** Strip one trailing '\n' without a regex. */
function stripNl(line: string): string {
  return line.endsWith('\n') ? line.slice(0, -1) : line;
}

/** Parse a v2 upload-pack request body into ls-refs / fetch arguments. */
export interface V2Request {
  command: string;
  args: Map<string, string[]>;
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  prefixes: string[];
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
}

export function parseV2Request(body: Buffer): V2Request | null {
  const packets = decodePktLines(body);
  const req: V2Request = {
    command: '',
    args: new Map(),
    wants: [],
    haves: [],
    shallow: [],
    depth: null,
    prefixes: [],
    peel: false,
    symrefs: false,
    unborn: false,
  };
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = stripNl(p.payload.toString('utf8'));
    if (line.startsWith('command=')) {
      req.command = line.slice('command='.length);
      continue;
    }
    // Arguments use `key=value` (agent=, object-format=) or `key value`
    // (want, have, shallow, deepen, ref-prefix) or a bare flag (peel).
    const eq = line.indexOf('=');
    const sp = line.indexOf(' ');
    let key: string;
    let val: string;
    if (eq >= 0 && (sp < 0 || eq < sp)) {
      key = line.slice(0, eq);
      val = line.slice(eq + 1);
    } else if (sp >= 0) {
      key = line.slice(0, sp);
      val = line.slice(sp + 1);
    } else {
      key = line;
      val = '';
    }
    if (!req.args.has(key)) req.args.set(key, []);
    req.args.get(key)?.push(val);
    if (key === 'want') req.wants.push(val);
    else if (key === 'have') req.haves.push(val);
    else if (key === 'shallow') req.shallow.push(val);
    else if (key === 'deepen') req.depth = parseInt(val, 10);
    else if (key === 'ref-prefix') req.prefixes.push(val);
    else if (key === 'peel') req.peel = true;
    else if (key === 'symrefs') req.symrefs = true;
    else if (key === 'unborn') req.unborn = true;
  }
  if (req.command === '') return null;
  return req;
}

// ---- upload-pack responses + receive-pack (appended second pass) -----------

export function lsRefsResponse(repo: Repo, req: V2Request): Buffer {
  const cacheKey = `${req.prefixes.join('\0')}|${req.peel ? 1 : 0}${req.symrefs ? 1 : 0}${req.unborn ? 1 : 0}`;
  const cached = repo.lsRefsCache.get(cacheKey);
  if (cached !== undefined) return cached;
  const head = headTarget(repo);
  const headOid = repo.refs.get(head);
  const refs = sortedRefs(repo);
  const keep = (name: string): boolean =>
    req.prefixes.length === 0 || req.prefixes.some((p) => name.startsWith(p));
  const keepHead =
    req.prefixes.length === 0 || req.prefixes.some((p) => 'HEAD'.startsWith(p) || p.startsWith('HEAD'));
  const lines: string[] = [];
  if (req.symrefs && keepHead) {
    const target = head;
    lines.push(headOid === undefined ? `unborn HEAD symref-target:${target}\n` : `${headOid} HEAD symref-target:${target}\n`);
  }
  for (const ref of refs) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (req.peel) {
      const obj = repo.objects.get(ref.oid);
      if (obj && obj.type === 'tag') line += ` peeled:${repoPeel(repo, ref.oid)}`;
    }
    lines.push(`${line}\n`);
  }
  if (refs.length === 0 && req.unborn && !req.symrefs) {
    lines.push(`unborn HEAD symref-target:${head}\n`);
  }
  const out = frameLinesFlush(lines);
  repo.lsRefsCache.set(cacheKey, out);
  return out;
}

/**
 * Fetch cache key. Fast path for the common single-want, no-haves case
 * (every clone/fetch in the suite): no spread, no sort, no join.
 */
function fetchCacheKey(
  prefix: string,
  wants: string[],
  haves: string[],
  depth: number | null,
  shallow: string[],
): string {
  if (wants.length === 1 && haves.length === 0 && shallow.length === 0) {
    return `${prefix}|${wants[0]}|${depth ?? ''}`;
  }
  const w = wants.length <= 1 ? wants.join(',') : [...wants].sort().join(',');
  const h = haves.length <= 1 ? haves.join(',') : [...haves].sort().join(',');
  const s = shallow.length <= 1 ? shallow.join(',') : [...shallow].sort().join(',');
  return `${prefix}|${w}|${h}|${depth ?? ''}|${s}`;
}

export function v2FetchResponse(
  store: Store,
  repo: Repo,
  req: V2Request,
): Buffer {
  const cacheKey = fetchCacheKey('v2', req.wants, req.haves, req.depth, req.shallow);
  const cached = repo.fetchCache.get(cacheKey);
  if (cached !== undefined) return cached;
  const { objects, shallow, unshallow } = fetchObjects(store, repo, req.wants, req.haves, req.depth, req.shallow);
  const pack = writePack(objects);
  const parts: Buffer[] = [];
  parts.push(pktLine('acknowledgments\n'));
  parts.push(pktLine(req.haves.length ? 'ready\n' : 'NAK\n'));
  parts.push(pktDelim());
  if (shallow.length > 0 || unshallow.length > 0 || req.depth !== null) {
    parts.push(pktLine('shallow-info\n'));
    for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
    for (const u of unshallow) parts.push(pktLine(`unshallow ${u}\n`));
    parts.push(pktDelim());
  }
  parts.push(pktLine('packfile\n'));
  parts.push(sidebandPack(pack));
  parts.push(pktFlush());
  const out = Buffer.concat(parts);
  repo.fetchCache.set(cacheKey, out);
  return out;
}

export function v0FetchResponse(
  store: Store,
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | null,
  shallowBoundaries: string[],
): Buffer {
  const cacheKey = fetchCacheKey('v0', wants, haves, depth, shallowBoundaries);
  const cached = repo.fetchCache.get(cacheKey);
  if (cached !== undefined) return cached;
  const { objects, shallow, unshallow } = fetchObjects(store, repo, wants, haves, depth, shallowBoundaries);
  const pack = writePack(objects);
  const parts: Buffer[] = [];
  for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
  for (const u of unshallow) parts.push(pktLine(`unshallow ${u}\n`));
  parts.push(pktLine('NAK\n'));
  parts.push(sidebandPack(pack));
  parts.push(pktFlush());
  const out = Buffer.concat(parts);
  repo.fetchCache.set(cacheKey, out);
  return out;
}

export interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export interface PushReport {
  unpackOk: boolean;
  refs: Array<{ ref: string; ok: boolean; reason: string }>;
}

/** Parse a receive-pack body: command pkt-lines, then a packfile. */
export function parseReceivePack(body: Buffer): { commands: PushCommand[]; pack: Buffer | null } {
  const commands: PushCommand[] = [];
  let off = 0;
  let packStart = -1;
  while (off + 4 <= body.length) {
    const h0 = hexVal(body[off] as number);
    const h1 = hexVal(body[off + 1] as number);
    const h2 = hexVal(body[off + 2] as number);
    const h3 = hexVal(body[off + 3] as number);
    if (h0 < 0 || h1 < 0 || h2 < 0 || h3 < 0) break;
    const len = (h0 << 12) | (h1 << 8) | (h2 << 4) | h3;
    if (len === 0) {
      packStart = off + 4;
      break;
    }
    if (len < 4 || off + len > body.length) break;
    const payload = body.subarray(off + 4, off + len);
    const line = stripNl(payload.toString('utf8'));
    const nul = line.indexOf('\0');
    const head = nul < 0 ? line : line.slice(0, nul);
    const bits = head.split(' ');
    if (bits.length >= 3) {
      commands.push({ oldOid: bits[0] as string, newOid: bits[1] as string, ref: bits.slice(2).join(' ') });
    }
    off += len;
  }
  let pack: Buffer | null = null;
  if (packStart >= 0 && packStart < body.length) pack = body.subarray(packStart);
  return { commands, pack };
}

export function applyPush(
  store: Store,
  repo: Repo,
  commands: PushCommand[],
  pack: Buffer | null,
): PushReport {
  const report: PushReport = { unpackOk: true, refs: [] };
  let refChanged = false;
  if (pack !== null && pack.length > 0) {
    try {
      const objects = readPack(pack);
      let inserted = false;
      for (const [oid, obj] of objects) {
        if (!repo.objects.has(oid)) { repo.objects.set(oid, obj); inserted = true; }
      }
      if (inserted) touchRepo(repo);
    } catch {
      report.unpackOk = false;
      for (const c of commands) report.refs.push({ ref: c.ref, ok: false, reason: 'unpack failed' });
      return report;
    }
  }
  for (const cmd of commands) {
    const current = repo.refs.get(cmd.ref);
    if (cmd.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        report.refs.push({ ref: cmd.ref, ok: false, reason: 'ref does not exist' });
      } else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) {
        report.refs.push({ ref: cmd.ref, ok: false, reason: 'old value does not match' });
      } else {
        repo.refs.delete(cmd.ref);
        refChanged = true;
        report.refs.push({ ref: cmd.ref, ok: true, reason: '' });
      }
      continue;
    }
    if (!repo.objects.has(cmd.newOid)) {
      report.refs.push({ ref: cmd.ref, ok: false, reason: 'object not found' });
      continue;
    }
    if (current === undefined) {
      if (cmd.oldOid !== ZERO_OID) {
        report.refs.push({ ref: cmd.ref, ok: false, reason: 'old value does not match' });
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      refChanged = true;
      report.refs.push({ ref: cmd.ref, ok: true, reason: '' });
      continue;
    }
    if (cmd.oldOid !== current) {
      report.refs.push({ ref: cmd.ref, ok: false, reason: 'old value does not match' });
      continue;
    }
    if (!store.isAncestor(repo, current, cmd.newOid)) {
      report.refs.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(cmd.ref, cmd.newOid);
    refChanged = true;
    report.refs.push({ ref: cmd.ref, ok: true, reason: '' });
  }
  if (refChanged) touchRepo(repo);
  return report;
}

export function pushReportResponse(report: PushReport): Buffer {
  const lines: string[] = [report.unpackOk ? 'unpack ok\n' : 'unpack error\n'];
  for (const r of report.refs) {
    lines.push(r.ok ? `ok ${r.ref}\n` : `ng ${r.ref} ${r.reason}\n`);
  }
  const inner = frameLines(lines);
  const framed = Buffer.allocUnsafe(4 + 1 + inner.length);
  const total = 5 + inner.length;
  framed[0] = HEX_DIGITS.charCodeAt((total >> 12) & 0xf);
  framed[1] = HEX_DIGITS.charCodeAt((total >> 8) & 0xf);
  framed[2] = HEX_DIGITS.charCodeAt((total >> 4) & 0xf);
  framed[3] = HEX_DIGITS.charCodeAt(total & 0xf);
  framed[4] = 1;
  inner.copy(framed, 5);
  return Buffer.concat([framed, pktFlush()]);
}

/** Parse a v0 upload-pack request body. */
export function parseV0Fetch(body: Buffer): { wants: string[]; haves: string[]; depth: number | null; shallow: string[] } {
  const packets = decodePktLines(body);
  const out = { wants: [] as string[], haves: [] as string[], depth: null as number | null, shallow: [] as string[] };
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = stripNl(p.payload.toString('utf8'));
    const firstSpace = line.indexOf(' ');
    const key = firstSpace < 0 ? line : line.slice(0, firstSpace);
    const rest = firstSpace < 0 ? '' : line.slice(firstSpace + 1);
    if (key === 'want') out.wants.push(rest.split(' ')[0] as string);
    else if (key === 'have') out.haves.push(rest);
    else if (key === 'deepen') out.depth = parseInt(rest, 10);
    else if (key === 'shallow') out.shallow.push(rest);
  }
  return out;
}

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}
