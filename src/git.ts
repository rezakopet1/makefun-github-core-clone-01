// Git smart-HTTP protocol (upload-pack v2, receive-pack v0) over the store.
// Plain (non-deltified) packfiles only; deltas on receipt are resolved.
import { createHash } from 'node:crypto';
import {
  hashObject,
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

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.alloc(4);
  head.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, data]);
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
    const lenText = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(lenText)) break;
    const len = parseInt(lenText, 16);
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
  return [...repo.refs.entries()]
    .map(([name, oid]) => ({ name, oid }))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

export function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** v2 capability advertisement body for git-upload-pack. */
export function v2Advertisement(): Buffer {
  const lines = [
    'version 2\n',
    'agent=github-core-clone/1.0\n',
    'ls-refs=unborn\n',
    'fetch=shallow filter\n',
    'server-option\n',
    'object-format=sha1\n',
  ];
  return Buffer.concat([pktLine('# service=git-upload-pack\n'), pktFlush(), ...lines.map((l) => pktLine(l)), pktFlush()]);
}

/** v0 ref advertisement body for a service. */
export function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const preamble = pktLine(`# service=${service}\n`);
  const refs = sortedRefs(repo);
  const caps =
    service === 'git-receive-pack'
      ? 'report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0'
      : 'side-band-64k multi_ack_detailed thin-pack ofs-delta shallow agent=github-core-clone/1.0';
  const parts: Buffer[] = [preamble, pktFlush()];
  const head = headTarget(repo);
  const headOid = repo.refs.get(head);
  if (refs.length === 0) {
    const line = `${ZERO_OID} capabilities^{}\0${caps}\n`;
    parts.push(pktLine(line));
  } else {
    // HEAD first, with symref capability, then refs in sorted order.
    const headLine = `${headOid ?? ZERO_OID} HEAD\0${caps} symref=HEAD:${head}\n`;
    parts.push(pktLine(headLine));
    for (const ref of refs) {
      parts.push(pktLine(`${ref.oid} ${ref.name}\n`));
      const obj = repo.objects.get(ref.oid);
      if (obj && obj.type === 'tag') {
        parts.push(pktLine(`${repoPeel(repo, ref.oid)} ${ref.name}^{}\n`));
      }
    }
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
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
  const objects: GitObject[] = [];
  for (const oid of wanted) {
    if (haveReachable.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj) objects.push(obj);
  }
  // Deterministic pack order: commits, then trees, then blobs, then tags,
  // each sorted by oid.
  const rank = (t: string): number => (t === 'commit' ? 0 : t === 'tree' ? 1 : t === 'blob' ? 2 : 3);
  objects.sort((a, b) => {
    const ra = rank(a.type);
    const rb = rank(b.type);
    if (ra !== rb) return ra - rb;
    return hashObject(a.type, a.data) < hashObject(b.type, b.data) ? -1 : 1;
  });
  return { objects, shallow: [...shallow].sort(), unshallow: [...unshallow].sort() };
}

export function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  // Max pkt-line is 65520 bytes total (4 header + payload); the band byte
  // leaves 65515 bytes of pack data per frame.
  const CHUNK = 65515;
  for (let off = 0; off < pack.length; off += CHUNK) {
    const slice = pack.subarray(off, Math.min(pack.length, off + CHUNK));
    const framed = Buffer.concat([Buffer.from([1]), slice]);
    parts.push(pktLine(framed));
  }
  if (pack.length === 0) {
    // An empty pack still needs a sideband frame carrying the pack bytes.
    const empty = writePack([]);
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), empty])));
  }
  return Buffer.concat(parts);
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
    const line = p.payload.toString('utf8').replace(/\n$/, '');
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
  const parts: Buffer[] = [];
  const head = headTarget(repo);
  const headOid = repo.refs.get(head);
  const refs = sortedRefs(repo);
  const keep = (name: string): boolean =>
    req.prefixes.length === 0 || req.prefixes.some((p) => name.startsWith(p));
  const keepHead =
    req.prefixes.length === 0 || req.prefixes.some((p) => 'HEAD'.startsWith(p) || p.startsWith('HEAD'));
  if (req.symrefs && keepHead) {
    const target = headOid === undefined ? head : head;
    const line = headOid === undefined ? `unborn HEAD symref-target:${target}\n` : `${headOid} HEAD symref-target:${target}\n`;
    parts.push(pktLine(line));
  }
  for (const ref of refs) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (req.peel) {
      const obj = repo.objects.get(ref.oid);
      if (obj && obj.type === 'tag') line += ` peeled:${repoPeel(repo, ref.oid)}`;
    }
    parts.push(pktLine(`${line}\n`));
  }
  if (refs.length === 0 && req.unborn && !req.symrefs) {
    parts.push(pktLine(`unborn HEAD symref-target:${head}\n`));
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

export function v2FetchResponse(
  store: Store,
  repo: Repo,
  req: V2Request,
): Buffer {
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
  return Buffer.concat(parts);
}

export function v0FetchResponse(
  store: Store,
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | null,
  shallowBoundaries: string[],
): Buffer {
  const { objects, shallow, unshallow } = fetchObjects(store, repo, wants, haves, depth, shallowBoundaries);
  const pack = writePack(objects);
  const parts: Buffer[] = [];
  for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
  for (const u of unshallow) parts.push(pktLine(`unshallow ${u}\n`));
  parts.push(pktLine('NAK\n'));
  parts.push(sidebandPack(pack));
  parts.push(pktFlush());
  return Buffer.concat(parts);
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
    const lenText = body.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(lenText)) break;
    const len = parseInt(lenText, 16);
    if (len === 0) {
      packStart = off + 4;
      break;
    }
    if (len < 4 || off + len > body.length) break;
    const payload = body.subarray(off + 4, off + len);
    const line = payload.toString('utf8').replace(/\n$/, '');
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
  if (pack !== null && pack.length > 0) {
    try {
      const objects = readPack(pack);
      for (const [oid, obj] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
      }
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
    report.refs.push({ ref: cmd.ref, ok: true, reason: '' });
  }
  return report;
}

export function pushReportResponse(report: PushReport): Buffer {
  const lines: string[] = [report.unpackOk ? 'unpack ok\n' : 'unpack error\n'];
  for (const r of report.refs) {
    lines.push(r.ok ? `ok ${r.ref}\n` : `ng ${r.ref} ${r.reason}\n`);
  }
  const inner = Buffer.concat(lines.map((l) => pktLine(l)));
  const framed = pktLine(Buffer.concat([Buffer.from([1]), inner]));
  return Buffer.concat([framed, pktFlush()]);
}

/** Parse a v0 upload-pack request body. */
export function parseV0Fetch(body: Buffer): { wants: string[]; haves: string[]; depth: number | null; shallow: string[] } {
  const packets = decodePktLines(body);
  const out = { wants: [] as string[], haves: [] as string[], depth: null as number | null, shallow: [] as string[] };
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
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
