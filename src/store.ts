// In-memory store for the github-core-clone-01 forge: one process, no persistence,
// no clock and no randomness in anything observable. Every timestamp is the fixed
// trial instant and every id is a sequential counter, so two runs on the same
// requests leave byte-identical state.
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const FIXED_ISO = '2026-11-14T06:30:00Z';
export const FIXED_EPOCH = 1700000000;
export const ZERO_OID = '0'.repeat(40);

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject {
  type: GitObjectType;
  data: Buffer;
}

export function hashObject(type: GitObjectType, data: Buffer): string {
  const h = createHash('sha1');
  h.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  h.update(data);
  return h.digest('hex');
}

export function parseTree(data: Buffer): Array<{ mode: string; name: string; oid: string }> {
  const entries: Array<{ mode: string; name: string; oid: string }> = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    const nul = data.indexOf(0x00, sp + 1);
    entries.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return entries;
}

export function parseCommit(data: Buffer): { tree: string; parents: string[]; message: string } {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank < 0 ? text : text.slice(0, blank);
  const message = blank < 0 ? '' : text.slice(blank + 2);
  let tree = '';
  const parents: string[] = [];
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
  }
  return { tree, parents, message };
}

export function parseTag(data: Buffer): { object: string; type: string } {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank < 0 ? text : text.slice(0, blank);
  let object = '';
  let type = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
  }
  return { object, type };
}

export function buildTree(entries: Array<{ mode: string; name: string; oid: string }>): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
    const bn = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
    return Buffer.compare(an, bn);
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface IssueComment {
  id: number;
  body: string;
  seq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  seq: number;
  comments: IssueComment[];
}

export interface FileChange {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  files: FileChange[];
  seq: number;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  priv: boolean;
  defaultBranch: string;
  seq: number;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  labels: Map<string, { name: string; color: string }>;
  issues: Issue[];
  pulls: Pull[];
  nextIssue: number;
  nextPull: number;
  nextComment: number;
  version: number;
  fetchCache: Map<string, Buffer>;
}

function b64urlEncode(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64url');
}

export function globalId(kind: string, ...parts: (string | number)[]): string {
  return b64urlEncode([kind, ...parts.map(String)].join(':'));
}

export function decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const bits = text.split(':');
    if (bits.length < 2) return null;
    return { kind: bits[0] as string, parts: bits.slice(1) };
  } catch {
    return null;
  }
}

export class Store {
  repos = new Map<string, Repo>();
  repoOrder: Repo[] = [];
  private nextRepoId = 1;
  private issueIdCounter = 1;
  private pullIdCounter = 1;
  private commentIdCounter = 1;
  private seq = 0;

  nextIssueId(): number {
    return this.issueIdCounter++;
  }

  nextPullId(): number {
    return this.pullIdCounter++;
  }

  nextCommentId(): number {
    return this.commentIdCounter++;
  }

  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  createRepo(name: string, priv: boolean, description: string): Repo | null {
    const k = this.key(VIEWER_LOGIN, name);
    if (this.repos.has(k)) return null;
    const repo: Repo = {
      id: this.nextRepoId++,
      owner: VIEWER_LOGIN,
      name,
      description,
      priv,
      defaultBranch: 'main',
      seq: this.nextSeq(),
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextIssue: 1,
      nextPull: 1,
      nextComment: 1,
      version: 0,
      fetchCache: new Map(),
    };
    this.repos.set(k, repo);
    this.repoOrder.push(repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(this.key(repo.owner, repo.name));
    const i = this.repoOrder.indexOf(repo);
    if (i >= 0) this.repoOrder.splice(i, 1);
  }

  viewerRepos(): Repo[] {
    return this.repoOrder.filter((r) => r.owner === VIEWER_LOGIN);
  }

  // ---- git objects -------------------------------------------------------

  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  blob(repo: Repo, data: Buffer): string {
    return this.putObject(repo, 'blob', data);
  }

  treeFromMap(repo: Repo, files: Map<string, string>): string {
    interface Node {
      files: Map<string, string>;
      dirs: Map<string, Node>;
    }
    const make = (): Node => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, oid] of [...files.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
      const segs = path.split('/');
      let node = root;
      for (let i = 0; i < segs.length - 1; i++) {
        const s = segs[i] as string;
        if (!node.dirs.has(s)) node.dirs.set(s, make());
        node = node.dirs.get(s) as Node;
      }
      node.files.set(segs[segs.length - 1] as string, oid);
    }
    const build = (node: Node): string => {
      const entries: Array<{ mode: string; name: string; oid: string }> = [];
      for (const [name, oid] of node.files) entries.push({ mode: '100644', name, oid });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return build(root);
  }

  treeMap(repo: Repo, commitOid: string): Map<string, string> {
    const out = new Map<string, string>();
    const commit = repo.objects.get(commitOid);
    if (!commit || commit.type !== 'commit') return out;
    const { tree } = parseCommit(commit.data);
    const walk = (treeOid: string, prefix: string): void => {
      const t = repo.objects.get(treeOid);
      if (!t || t.type !== 'tree') return;
      for (const e of parseTree(t.data)) {
        const p = prefix === '' ? e.name : `${prefix}/${e.name}`;
        if (e.mode === '40000') walk(e.oid, p);
        else out.set(p, e.oid);
      }
    };
    walk(tree, '');
    return out;
  }

  commitObject(repo: Repo, treeOid: string, parents: string[], message: string): string {
    const who = `${VIEWER_NAME} <${VIEWER_EMAIL}> ${FIXED_EPOCH} +0000`;
    const lines = [`tree ${treeOid}`];
    for (const p of parents) lines.push(`parent ${p}`);
    lines.push(`author ${who}`, `committer ${who}`, '');
    const msg = message.endsWith('\n') ? message : `${message}\n`;
    return this.putObject(repo, 'commit', Buffer.from(`${lines.join('\n')}\n${msg}`, 'utf8'));
  }

  reachable(repo: Repo, roots: Iterable<string>): Set<string> {
    const seen = new Set<string>();
    const queue: string[] = [];
    for (const r of roots) if (/^[0-9a-f]{40}$/.test(r)) queue.push(r);
    while (queue.length) {
      const oid = queue.pop() as string;
      if (seen.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (!obj) continue;
      seen.add(oid);
      if (obj.type === 'commit') {
        const c = parseCommit(obj.data);
        queue.push(c.tree);
        for (const p of c.parents) queue.push(p);
      } else if (obj.type === 'tag') {
        queue.push(parseTag(obj.data).object);
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.data)) if (e.mode !== '160000') queue.push(e.oid);
      }
    }
    return seen;
  }

  isAncestor(repo: Repo, anc: string, desc: string): boolean {
    if (anc === desc) return true;
    const seen = new Set<string>();
    const queue = [desc];
    while (queue.length) {
      const oid = queue.pop() as string;
      if (oid === anc) return true;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      for (const p of parseCommit(obj.data).parents) queue.push(p);
    }
    return false;
  }

  mergeBase(repo: Repo, a: string, b: string): string | null {
    const ancA = new Set<string>();
    const queue = [a];
    while (queue.length) {
      const oid = queue.pop() as string;
      if (ancA.has(oid)) continue;
      ancA.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      for (const p of parseCommit(obj.data).parents) queue.push(p);
    }
    const qb = [b];
    const seenB = new Set<string>();
    while (qb.length) {
      const oid = qb.pop() as string;
      if (ancA.has(oid)) return oid;
      if (seenB.has(oid)) continue;
      seenB.add(oid);
      const obj = repo.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      for (const p of parseCommit(obj.data).parents) qb.push(p);
    }
    return null;
  }

  peel(repo: Repo, oid: string): string {
    let cur = oid;
    for (let i = 0; i < 10; i++) {
      const obj = repo.objects.get(cur);
      if (!obj || obj.type !== 'tag') return cur;
      cur = parseTag(obj.data).object;
    }
    return cur;
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  branchTip(repo: Repo, branch: string): string | undefined {
    return repo.refs.get(`refs/heads/${branch}`);
  }
}

// ---- contents / commits / issues / pulls (appended second pass) ------------

export interface ContentResult {
  status: 201 | 200 | 409 | 422 | 404;
  commitSha?: string;
  contentSha?: string;
  message?: string;
}

function lineCounts(a: Buffer | null, b: Buffer | null): { additions: number; deletions: number } {
  const la = a === null ? [] : a.toString('utf8').split('\n');
  const lb = b === null ? [] : b.toString('utf8').split('\n');
  const count = new Map<string, number>();
  for (const l of la) count.set(l, (count.get(l) ?? 0) + 1);
  let additions = 0;
  for (const l of lb) {
    const c = count.get(l) ?? 0;
    if (c > 0) count.set(l, c - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const c of count.values()) deletions += c;
  return { additions, deletions };
}

export function diffFiles(
  store: Store,
  repo: Repo,
  baseCommit: string | null,
  headCommit: string,
): FileChange[] {
  const baseMap = baseCommit === null ? new Map<string, string>() : store.treeMap(repo, baseCommit);
  const headMap = store.treeMap(repo, headCommit);
  const out: FileChange[] = [];
  const paths = new Set([...baseMap.keys(), ...headMap.keys()]);
  for (const path of [...paths].sort()) {
    const b = baseMap.get(path);
    const h = headMap.get(path);
    if (b === h) continue;
    if (b === undefined) {
      const data = repo.objects.get(h as string)?.data ?? Buffer.alloc(0);
      const { additions } = lineCounts(null, data);
      out.push({ filename: path, status: 'added', additions: Math.max(additions, 1), deletions: 0, changes: Math.max(additions, 1) });
    } else if (h === undefined) {
      const data = repo.objects.get(b)?.data ?? Buffer.alloc(0);
      const { deletions } = lineCounts(data, null);
      out.push({ filename: path, status: 'removed', additions: 0, deletions: Math.max(deletions, 1), changes: Math.max(deletions, 1) });
    } else {
      const bd = repo.objects.get(b)?.data ?? Buffer.alloc(0);
      const hd = repo.objects.get(h)?.data ?? Buffer.alloc(0);
      const { additions, deletions } = lineCounts(bd, hd);
      const a = Math.max(additions, 1);
      out.push({ filename: path, status: 'modified', additions: a, deletions, changes: a + deletions });
    }
  }
  return out;
}

export function storeCommitFile(
  store: Store,
  repo: Repo,
  branch: string,
  path: string,
  content: Buffer,
  message: string,
  sha?: string,
  isDelete = false,
): ContentResult {
  let tip = store.branchTip(repo, branch);
  if (tip === undefined && branch !== repo.defaultBranch) {
    // Create the branch from the default tip (or unborn) before committing.
    const base = store.branchTip(repo, repo.defaultBranch);
    if (base !== undefined) {
      repo.refs.set(`refs/heads/${branch}`, base);
      tip = base;
    }
  }
  const files = tip === undefined ? new Map<string, string>() : store.treeMap(repo, tip);
  const existing = files.get(path);
  if (isDelete) {
    if (existing === undefined) return { status: 404, message: 'Not Found' };
    if (sha !== undefined && sha.toLowerCase() !== existing.toLowerCase()) return { status: 409, message: 'sha does not match' };
    files.delete(path);
  } else {
    if (existing !== undefined) {
      if (sha === undefined) return { status: 409, message: 'sha required to update a file' };
      if (sha.toLowerCase() !== existing.toLowerCase()) return { status: 409, message: 'sha does not match the current file' };
    }
    const blobOid = store.blob(repo, content);
    files.set(path, blobOid);
    const treeOid = store.treeFromMap(repo, files);
    const commitSha = store.commitObject(repo, treeOid, tip === undefined ? [] : [tip], message);
    repo.refs.set(`refs/heads/${branch}`, commitSha);
    touchRepo(repo);
    return { status: existing === undefined ? 201 : 200, commitSha, contentSha: blobOid };
  }
  const treeOid = store.treeFromMap(repo, files);
  const commitSha = store.commitObject(repo, treeOid, tip === undefined ? [] : [tip], message);
  repo.refs.set(`refs/heads/${branch}`, commitSha);
  touchRepo(repo);
  return { status: 200, commitSha, contentSha: existing };
}

export function storeCreateIssue(
  store: Store,
  repo: Repo,
  title: string,
  body: string,
  labels: string[],
): Issue | { error: string } {
  for (const l of labels) if (!repo.labels.has(l)) return { error: `label ${l} does not exist` };
  const issue: Issue = {
    id: store.nextIssueId(),
    number: repo.nextIssue++,
    title,
    body,
    state: 'open',
    labels: [...labels],
    seq: store.nextSeq(),
    comments: [],
  };
  repo.issues.push(issue);
  return issue;
}

export function storeCreatePull(
  store: Store,
  repo: Repo,
  title: string,
  body: string,
  head: string,
  base: string,
): Pull | { error: string } {
  const headSha = store.branchTip(repo, head);
  const baseSha = store.branchTip(repo, base);
  if (headSha === undefined || baseSha === undefined) return { error: 'head or base branch not found' };
  const mergeBaseOid = store.mergeBase(repo, headSha, baseSha);
  const files = diffFiles(store, repo, mergeBaseOid, headSha);
  const pull: Pull = {
    id: store.nextPullId(),
    number: repo.nextPull++,
    title,
    body,
    state: 'open',
    head,
    base,
    headSha,
    baseSha,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    files,
    seq: store.nextSeq(),
  };
  repo.pulls.push(pull);
  return pull;
}

export function pullMergeable(store: Store, repo: Repo, pull: Pull): 'MERGEABLE' | 'CONFLICTING' {
  const headTip = store.branchTip(repo, pull.head);
  const baseTip = store.branchTip(repo, pull.base);
  if (headTip === undefined || baseTip === undefined) return 'CONFLICTING';
  const mb = store.mergeBase(repo, headTip, baseTip);
  if (mb === null) return 'MERGEABLE';
  const baseMap = store.treeMap(repo, mb);
  const headMap = store.treeMap(repo, headTip);
  const curMap = store.treeMap(repo, baseTip);
  for (const path of new Set([...headMap.keys(), ...baseMap.keys()])) {
    const fromBase = baseMap.get(path);
    const inHead = headMap.get(path);
    const inCur = curMap.get(path);
    if (inHead === fromBase) continue;
    if (inCur === fromBase) continue;
    if (inHead === inCur) continue;
    return 'CONFLICTING';
  }
  return 'MERGEABLE';
}

export function storeMergePull(
  store: Store,
  repo: Repo,
  pull: Pull,
): { status: 200 | 405; sha?: string; message?: string } {
  if (pull.state !== 'open' || pull.merged) return { status: 405, message: 'pull request is not open' };
  if (pullMergeable(store, repo, pull) === 'CONFLICTING') return { status: 405, message: 'merge conflict' };
  const headTip = store.branchTip(repo, pull.head) as string;
  const baseTip = store.branchTip(repo, pull.base) as string;
  const mb = store.mergeBase(repo, headTip, baseTip);
  const headMap = store.treeMap(repo, headTip);
  const curMap = store.treeMap(repo, baseTip);
  const merged = new Map(curMap);
  const changed = diffFiles(store, repo, mb, headTip);
  for (const f of changed) {
    const oid = headMap.get(f.filename);
    if (oid === undefined) merged.delete(f.filename);
    else merged.set(f.filename, oid);
  }
  const treeOid = store.treeFromMap(repo, merged);
  const sha = store.commitObject(repo, treeOid, [baseTip, headTip], `Merge pull request #${pull.number} from ${pull.head}`);
  repo.refs.set(`refs/heads/${pull.base}`, sha);
  touchRepo(repo);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = FIXED_ISO;
  pull.mergeCommitSha = sha;
  pull.baseSha = sha;
  return { status: 200, sha, message: `Merged #${pull.number}` };
}

// Fast zlib-stream inflate: one inflateSync with { info: true } reports the exact
// number of input bytes the stream consumed (engine.bytesWritten), so a pack entry
// needs a single inflate instead of a binary search full of throwing probes.
// Feature-detected once: older Node returns a plain Buffer without `engine`.
const INFLATE_INFO_WORKS: boolean = (() => {
  try {
    const probe = inflateSync(deflateSync(Buffer.from([1])), { info: true }) as unknown;
    return (
      !!probe &&
      typeof probe === 'object' &&
      'engine' in (probe as Record<string, unknown>) &&
      typeof (probe as { engine?: { bytesWritten?: unknown } }).engine?.bytesWritten === 'number'
    );
  } catch {
    return false;
  }
})();

/** Inflate one pack entry starting at `start`; returns the data and bytes consumed. */
function inflateEntry(buf: Buffer, start: number, end: number): { data: Buffer; consumed: number } {
  if (INFLATE_INFO_WORKS) {
    const r = inflateSync(buf.subarray(start, end), { info: true }) as unknown as {
      buffer: Buffer;
      engine: { bytesWritten: number };
    };
    const consumed = r.engine.bytesWritten;
    if (Number.isInteger(consumed) && consumed > 0 && start + consumed <= end) return { data: r.buffer, consumed };
    // Fall through to the slow path if the engine did not report a usable length.
  }
  const data = inflateSync(buf.subarray(start, end));
  return { data, consumed: zlibConsumed(buf, start, end) };
}

/** Bump a repo's mutation version and drop its cached fetch responses. */
export function touchRepo(repo: Repo): void {
  repo.version += 1;
  repo.fetchCache.clear();
}

// Packfile helpers (plain objects only, no deltas) ---------------------------

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

export function writePack(objects: GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) {
    const data = obj.data;
    const code = TYPE_CODE[obj.type];
    let size = data.length;
    const bytes: number[] = [];
    let first = (code << 4) | (size & 0x0f);
    size = Math.floor(size / 16);
    while (size > 0) {
      bytes.push(first | 0x80);
      first = size & 0x7f;
      size = Math.floor(size / 128);
    }
    bytes.push(first);
    parts.push(Buffer.from(bytes));
    parts.push(deflateSync(data, { level: 1 }));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

export function readPack(pack: Buffer): Map<string, GitObject> {
  const out = new Map<string, GitObject>();
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack header');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('bad pack version');
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  let offset = 12;
  interface Raw {
    offset: number;
    typeCode: number;
    size: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
  }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
  for (let i = 0; i < count; i++) {
    const entryOffset = offset;
    let byte = pack[offset] as number;
    const first = byte;
    let size = byte & 0x0f;
    let shift = 4;
    offset += 1;
    while (byte & 0x80) {
      byte = pack[offset] as number;
      offset += 1;
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    const typeCode = (first >> 4) & 0x07;
    const entry: Raw = { offset: entryOffset, typeCode, size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      byte = pack[offset] as number;
      offset += 1;
      let value = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[offset] as number;
        offset += 1;
        value = ((value + 1) * 128) + (byte & 0x7f);
      }
      entry.baseOffset = entryOffset - value;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`unknown pack type ${typeCode}`);
    }
    const inflated = inflateEntry(pack, offset, trailerStart);
    if (inflated.data.length !== size) throw new Error('pack entry size mismatch');
    entry.data = inflated.data;
    offset = offset + inflated.consumed;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  const resolved = new Map<number, { type: GitObjectType; data: Buffer; oid: string }>();
  const byOid = new Map<string, { type: GitObjectType; data: Buffer }>();
  const resolve = (entry: Raw): { type: GitObjectType; data: Buffer; oid: string } => {
    const cached = resolved.get(entry.offset);
    if (cached) return cached;
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset as number);
      if (!baseEntry) throw new Error('ofs-delta base missing');
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const base = byOid.get(entry.baseOid as string);
      if (!base) throw new Error('ref-delta base missing');
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else {
      type = CODE_TYPE[entry.typeCode] as GitObjectType;
      data = entry.data;
    }
    const oid = hashObject(type, data);
    const result = { type, data, oid };
    resolved.set(entry.offset, result);
    byOid.set(oid, result);
    return result;
  };
  for (const entry of raw) {
    const r = resolve(entry);
    if (!out.has(r.oid)) out.set(r.oid, { type: r.type, data: r.data });
  }
  return out;
}

function zlibConsumed(buf: Buffer, start: number, end: number): number {
  // Inflate incrementally with a small window probe: use node:zlib's
  // synchronous inflate on a truncated slice is unreliable, so binary-search
  // the smallest slice that inflates without error and yields a complete
  // stream. Pack entries are small in this suite; a linear scan in 1-byte
  // steps from a lower bound is acceptable but slow for 20 KiB blobs, so
  // use the fact that deflate streams end on a byte boundary: try slices
  // with exponential growth then binary search.
  let lo = start + 1;
  let hi = end;
  const ok = (n: number): boolean => {
    try {
      inflateSync(buf.subarray(start, n));
      return true;
    } catch {
      return false;
    }
  };
  if (!ok(hi)) throw new Error('zlib stream does not terminate');
  while (lo < hi) {
    const mid = Math.floor((lo + hi) / 2);
    if (ok(mid)) hi = mid;
    else lo = mid + 1;
  }
  return lo - start;
}

function readDeltaVarint(data: Buffer, offset: number): { value: number; next: number } {
  let value = 0;
  let shift = 0;
  let i = offset;
  for (;;) {
    const b = data[i] as number;
    i += 1;
    value |= (b & 0x7f) << shift;
    shift += 7;
    if (!(b & 0x80)) break;
  }
  return { value, next: i };
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const src = readDeltaVarint(delta, off);
  off = src.next;
  if (src.value !== base.length) throw new Error('delta base size mismatch');
  const tgt = readDeltaVarint(delta, off);
  off = tgt.next;
  const out: Buffer[] = [];
  let total = 0;
  while (off < delta.length) {
    const op = delta[off] as number;
    off += 1;
    if (op & 0x80) {
      let cpOff = 0;
      let cpSize = 0;
      for (let i = 0; i < 4; i++) {
        if (op & (1 << i)) {
          cpOff |= (delta[off] as number) << (8 * i);
          off += 1;
        }
      }
      for (let i = 0; i < 3; i++) {
        if (op & (0x10 << i)) {
          cpSize |= (delta[off] as number) << (8 * i);
          off += 1;
        }
      }
      if (cpSize === 0) cpSize = 0x10000;
      out.push(base.subarray(cpOff, cpOff + cpSize));
      total += cpSize;
    } else if (op > 0) {
      out.push(delta.subarray(off, off + op));
      total += op;
      off += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(out);
  if (result.length !== tgt.value || total !== tgt.value) throw new Error('delta result size mismatch');
  return result;
}
