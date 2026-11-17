// REST v3 under /api/v3: repos, branches, commits, contents, labels, issues,
// pull requests. All bodies are JSON; every reply carries X-GitHub-Request-Id
// (added by app.ts) and application/json.
import {
  FIXED_ISO,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  diffFiles,
  parseCommit,
  storeCommitFile,
  storeCreateIssue,
  storeCreatePull,
  storeMergePull,
  pullMergeable,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

export interface RestResult {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const notFound = (message = 'Not Found'): RestResult => ({ status: 404, body: { message } });
const badRequest = (message: string): RestResult => ({ status: 400, body: { message } });

export function isoOfEpoch(epochSec: number): string {
  return new Date(epochSec * 1000).toISOString();
}

function userJson(): Record<string, unknown> {
  return { login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL, type: 'User', avatar_url: '' };
}

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R_${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.priv,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    pushed_at: FIXED_ISO,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    fork: false,
    archived: false,
    disabled: false,
    visibility: repo.priv ? 'private' : 'public',
  };
}

export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_${issue.id}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const l = repo.labels.get(name);
      return { name, color: l?.color ?? 'ededed', description: '' };
    }),
    user: userJson(),
    assignee: null,
    assignees: [],
    locked: false,
    comments: issue.comments.length,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: issue.state === 'closed' ? FIXED_ISO : null,
    html_url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

export function pullJson(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: `PR_${pull.id}`,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: pullMergeable(store, repo, pull) === 'MERGEABLE',
    head: { ref: pull.head, sha: pull.headSha, label: `${repo.owner}:${pull.head}`, user: userJson(), repo: repoJson(repo) },
    base: { ref: pull.base, sha: pull.baseSha, label: `${repo.owner}:${pull.base}`, user: userJson(), repo: repoJson(repo) },
    user: userJson(),
    assignee: null,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    closed_at: pull.state === 'closed' ? FIXED_ISO : null,
    html_url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

function parseAuthor(line: string): { name: string; email: string; date: string } {
  const m = /^(.*) <(.*)> (\d+) ([+-]\d{4})$/.exec(line);
  if (!m) return { name: VIEWER_NAME, email: VIEWER_EMAIL, date: FIXED_ISO };
  return { name: m[1] as string, email: m[2] as string, date: isoOfEpoch(Number(m[3])) };
}

function commitJson(store: Store, repo: Repo, sha: string): Record<string, unknown> | null {
  const obj = repo.objects.get(sha);
  if (!obj || obj.type !== 'commit') return null;
  const text = obj.data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank < 0 ? text : text.slice(0, blank);
  const message = blank < 0 ? '' : text.slice(blank + 2);
  let authorLine = '';
  let committerLine = '';
  const parents: string[] = [];
  for (const line of head.split('\n')) {
    if (line.startsWith('author ')) authorLine = line.slice(7);
    else if (line.startsWith('committer ')) committerLine = line.slice(10);
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
  }
  const author = parseAuthor(authorLine);
  const committer = parseAuthor(committerLine);
  const cleanMessage = message.replace(/\n+$/, '');
  return {
    sha,
    node_id: `C_${sha}`,
    commit: {
      message: cleanMessage,
      author: { name: author.name, email: author.email, date: author.date },
      committer: { name: committer.name, email: committer.email, date: committer.date },
    },
    author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    parents: parents.map((p) => ({ sha: p })),
    url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${sha}`,
  };
}

function commitList(store: Store, repo: Repo, tip: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length) {
    const oid = queue.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    out.push(oid);
    for (const p of parseCommit(obj.data).parents) queue.push(p);
  }
  return out;
}

function paginate<T>(items: T[], query: URLSearchParams): { page: T[]; link: string | null; total: number } {
  const perPage = Math.max(1, Math.min(100, Number(query.get('per_page') ?? '30') || 30));
  const page = Math.max(1, Number(query.get('page') ?? '1') || 1);
  const start = (page - 1) * perPage;
  return { page: items.slice(start, start + perPage), link: null, total: items.length };
}

export function linkHeader(origin: string, path: string, query: URLSearchParams, page: number, perPage: number, total: number): string | null {
  const pages = Math.ceil(total / perPage);
  if (pages <= 1) return null;
  const links: string[] = [];
  const mk = (p: number, rel: string): string => {
    const q = new URLSearchParams(query);
    q.set('page', String(p));
    q.set('per_page', String(perPage));
    return `<${origin}${path}?${q.toString()}>; rel="${rel}"`;
  };
  if (page < pages) links.push(mk(page + 1, 'next'), mk(pages, 'last'));
  if (page > 1) links.push(mk(1, 'first'), mk(page - 1, 'prev'));
  return links.length ? links.join(', ') : null;
}

export type Body = Record<string, unknown>;

export function handleRest(
  store: Store,
  method: string,
  segs: string[],
  query: URLSearchParams,
  body: Body | null,
  origin: string,
): RestResult {
  // segs are the path parts after /api/v3
  if (segs.length === 0) {
    if (method === 'GET') {
      return { status: 200, body: { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, headers: { 'x-oauth-scopes': 'repo, read:org' } };
    }
    return notFound();
  }
  const [first] = segs as [string];
  if (first === 'meta' && method === 'GET') {
    return { status: 200, body: { installed_version: '1.0.0' } };
  }
  if (first === 'rate_limit' && method === 'GET') {
    return { status: 200, body: { resources: { core: { limit: 5000, remaining: 4999, reset: 1700000000 } }, rate: { limit: 5000, remaining: 4999, reset: 1700000000 } } };
  }
  if (first === 'user') {
    if (segs.length === 1 && method === 'GET') return { status: 200, body: userJson() };
    if (segs.length === 2 && segs[1] === 'repos') {
      if (method === 'GET') {
        const repos = store.viewerRepos();
        const perPage = Math.max(1, Math.min(100, Number(query.get('per_page') ?? '30') || 30));
        const pageNum = Math.max(1, Number(query.get('page') ?? '1') || 1);
        const start = (pageNum - 1) * perPage;
        const pageItems = repos.slice(start, start + perPage);
        const link = linkHeader(origin, '/api/v3/user/repos', query, pageNum, perPage, repos.length);
        return { status: 200, body: pageItems.map(repoJson), headers: link ? { link } : {} };
      }
      if (method === 'POST') {
        const name = typeof body?.name === 'string' ? (body.name as string) : '';
        if (name === '') return badRequest('name is required');
        const created = store.createRepo(name, body?.private === true, typeof body?.description === 'string' ? (body.description as string) : '');
        if (created === null) return { status: 422, body: { message: 'Repository already exists' } };
        return { status: 201, body: repoJson(created) };
      }
    }
    return notFound();
  }
  if (first !== 'repos') return notFound();
  const owner = segs[1] ?? '';
  const name = segs[2] ?? '';
  if (owner === '' || name === '') return notFound();
  const repo = store.getRepo(owner, name);
  if (repo === undefined) return notFound();
  const rest = segs.slice(3);
  if (rest.length === 0) {
    if (method === 'GET') return { status: 200, body: repoJson(repo) };
    if (method === 'PATCH') {
      if (typeof body?.description === 'string') repo.description = body.description as string;
      if (typeof body?.private === 'boolean') repo.priv = body.private as boolean;
      if (typeof body?.default_branch === 'string') repo.defaultBranch = body.default_branch as string;
      return { status: 200, body: repoJson(repo) };
    }
    if (method === 'DELETE') {
      store.deleteRepo(repo);
      return { status: 204, body: null };
    }
    return notFound();
  }
  const [section] = rest as [string];
  if (section === 'branches') {
    if (rest.length === 1 && method === 'GET') {
      const list = [...repo.refs.entries()]
        .filter(([r]) => r.startsWith('refs/heads/'))
        .map(([r, sha]) => ({ name: r.slice('refs/heads/'.length), commit: { sha, url: '' }, protected: false }))
        .sort((a, b) => (a.name < b.name ? -1 : 1));
      return { status: 200, body: list };
    }
    if (rest.length === 2 && method === 'GET') {
      const branch = rest[1] as string;
      const sha = store.branchTip(repo, branch);
      if (sha === undefined) return notFound();
      return { status: 200, body: { name: branch, commit: { sha, url: '' }, protected: false } };
    }
    return notFound();
  }
  if (section === 'commits') {
    if (rest.length === 1 && method === 'GET') {
      const refName = query.get('sha') ?? repo.defaultBranch;
      let tip = store.branchTip(repo, refName);
      if (tip === undefined && /^[0-9a-f]{40}$/.test(refName)) tip = refName;
      if (tip === undefined) return { status: 200, body: [] };
      const shas = commitList(store, repo, tip);
      const { page } = paginate(shas, query);
      const items = page.map((s) => commitJson(store, repo, s)).filter((x): x is Record<string, unknown> => x !== null);
      return { status: 200, body: items };
    }
    if (rest.length === 2 && method === 'GET') {
      const item = commitJson(store, repo, rest[1] as string);
      if (item === null) return notFound();
      return { status: 200, body: item };
    }
    return notFound();
  }
  if (section === 'git') {
    return handleGitData(store, repo, method, rest.slice(1), body);
  }
  if (section === 'contents') {
    return handleContents(store, repo, method, rest.slice(1), query, body);
  }
  if (section === 'labels') {
    if (method === 'POST') {
      const labelName = typeof body?.name === 'string' ? (body.name as string) : '';
      if (labelName === '') return badRequest('name is required');
      const color = typeof body?.color === 'string' ? (body.color as string) : 'ededed';
      repo.labels.set(labelName, { name: labelName, color });
      return { status: 201, body: { name: labelName, color, description: '' } };
    }
    if (method === 'GET') {
      return { status: 200, body: [...repo.labels.values()].map((l) => ({ name: l.name, color: l.color, description: '' })) };
    }
    return notFound();
  }
  if (section === 'issues') {
    return handleIssues(store, repo, method, rest.slice(1), query, body);
  }
  if (section === 'pulls') {
    return handlePulls(store, repo, method, rest.slice(1), query, body);
  }
  return notFound();
}

function handleGitData(store: Store, repo: Repo, method: string, rest: string[], body: Body | null): RestResult {
  if (rest[0] === 'refs') {
    if (method === 'POST' && rest.length === 1) {
      const ref = typeof body?.ref === 'string' ? (body.ref as string) : '';
      const sha = typeof body?.sha === 'string' ? (body.sha as string) : '';
      if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha)) return { status: 422, body: { message: 'invalid ref or sha' } };
      if (repo.refs.has(ref)) return { status: 422, body: { message: 'Reference already exists' } };
      if (!repo.objects.has(sha)) return { status: 422, body: { message: 'object does not exist' } };
      repo.refs.set(ref, sha);
      const type = repo.objects.get(sha)?.type ?? 'commit';
      return { status: 201, body: { ref, object: { sha, type }, node_id: `R_${sha}` } };
    }
    if (method === 'GET' && rest.length === 1) {
      const list = [...repo.refs.entries()]
        .map(([r, sha]) => ({ ref: r, object: { sha, type: repo.objects.get(sha)?.type ?? 'commit' } }))
        .sort((a, b) => (a.ref < b.ref ? -1 : 1));
      return { status: 200, body: list };
    }
    if (method === 'GET' && rest.length >= 3 && rest[1] === 'heads') {
      const branch = rest.slice(2).join('/');
      const sha = store.branchTip(repo, branch);
      if (sha === undefined) return notFound();
      return { status: 200, body: { ref: `refs/heads/${branch}`, object: { sha, type: 'commit' } } };
    }
    if (method === 'GET' && rest.length >= 3 && rest[1] === 'tags') {
      const tag = rest.slice(2).join('/');
      const sha = repo.refs.get(`refs/tags/${tag}`);
      if (sha === undefined) return notFound();
      return { status: 200, body: { ref: `refs/tags/${tag}`, object: { sha, type: repo.objects.get(sha)?.type ?? 'tag' } } };
    }
    return notFound();
  }
  return notFound();
}

// ---- contents / issues / pulls (appended second pass) -----------------------

function resolveRefTip(store: Store, repo: Repo, ref: string | null): string | undefined {
  if (ref === null) return store.branchTip(repo, repo.defaultBranch);
  const byBranch = store.branchTip(repo, ref);
  if (byBranch !== undefined) return byBranch;
  if (/^[0-9a-f]{40}$/.test(ref) && repo.objects.has(ref)) return ref;
  return undefined;
}

function handleContents(store: Store, repo: Repo, method: string, rest: string[], query: URLSearchParams, body: Body | null): RestResult {
  const path = rest.map(decodeURIComponent).join('/');
  const ref = query.get('ref');
  if (method === 'GET') {
    const tip = resolveRefTip(store, repo, ref);
    if (tip === undefined) return notFound();
    const files = store.treeMap(repo, tip);
    if (path === '') {
      // Root listing.
      const entries = dirEntries(store, repo, files, '');
      return { status: 200, body: entries };
    }
    const blobOid = files.get(path);
    if (blobOid !== undefined) {
      const data = repo.objects.get(blobOid)?.data ?? Buffer.alloc(0);
      return {
        status: 200,
        body: {
          type: 'file',
          encoding: 'base64',
          size: data.length,
          name: path.split('/').pop() ?? path,
          path,
          content: data.toString('base64'),
          sha: blobOid,
          url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${path}`,
          html_url: '',
          download_url: '',
        },
      };
    }
    const prefix = `${path}/`;
    if ([...files.keys()].some((p) => p.startsWith(prefix))) {
      return { status: 200, body: dirEntries(store, repo, files, path) };
    }
    return notFound();
  }
  if (method === 'PUT') {
    const branch = typeof body?.branch === 'string' ? (body.branch as string) : repo.defaultBranch;
    const message = typeof body?.message === 'string' ? (body.message as string) : `Update ${path}`;
    const contentB64 = typeof body?.content === 'string' ? (body.content as string) : '';
    const content = Buffer.from(contentB64.replace(/\s/g, ''), 'base64');
    const sha = typeof body?.sha === 'string' ? (body.sha as string) : undefined;
    const result = storeCommitFile(store, repo, branch, path, content, message, sha, false);
    if (result.status === 409) return { status: 409, body: { message: result.message ?? 'sha does not match' } };
    if (result.status === 404) return notFound();
    return {
      status: result.status,
      body: {
        content: { name: path.split('/').pop() ?? path, path, sha: result.contentSha ?? '', size: content.length, type: 'file', encoding: 'base64' },
        commit: { sha: result.commitSha ?? '', message },
      },
    };
  }
  if (method === 'DELETE') {
    const branch = typeof body?.branch === 'string' ? (body.branch as string) : repo.defaultBranch;
    const message = typeof body?.message === 'string' ? (body.message as string) : `Delete ${path}`;
    const sha = typeof body?.sha === 'string' ? (body.sha as string) : undefined;
    const result = storeCommitFile(store, repo, branch, path, Buffer.alloc(0), message, sha, true);
    if (result.status === 404) return notFound();
    if (result.status === 409) return { status: 409, body: { message: result.message ?? 'sha does not match' } };
    return { status: 200, body: { commit: { sha: result.commitSha ?? '', message } } };
  }
  return notFound();
}

function dirEntries(store: Store, repo: Repo, files: Map<string, string>, dir: string): Array<Record<string, unknown>> {
  const prefix = dir === '' ? '' : `${dir}/`;
  const children = new Map<string, { dir: boolean; oid: string }>();
  for (const [path, oid] of files) {
    if (!path.startsWith(prefix)) continue;
    const restPath = path.slice(prefix.length);
    const first = restPath.split('/')[0] as string;
    if (restPath.includes('/')) {
      if (!children.has(first)) children.set(first, { dir: true, oid: '' });
    } else {
      children.set(first, { dir: false, oid });
    }
  }
  const out: Array<Record<string, unknown>> = [];
  for (const name of [...children.keys()].sort()) {
    const child = children.get(name) as { dir: boolean; oid: string };
    const childPath = dir === '' ? name : `${dir}/${name}`;
    if (child.dir) {
      const sub = new Map<string, string>();
      for (const [p, o] of files) {
        if (p.startsWith(`${childPath}/`)) sub.set(p.slice(childPath.length + 1), o);
      }
      const treeOid = store.treeFromMap(repo, sub);
      out.push({ type: 'dir', name, path: childPath, sha: treeOid, size: 0, url: '', html_url: '' });
    } else {
      const data = repo.objects.get(child.oid)?.data ?? Buffer.alloc(0);
      out.push({ type: 'file', name, path: childPath, sha: child.oid, size: data.length, url: '', html_url: '', download_url: '' });
    }
  }
  return out;
}

function handleIssues(store: Store, repo: Repo, method: string, rest: string[], query: URLSearchParams, body: Body | null): RestResult {
  if (rest.length === 0) {
    if (method === 'POST') {
      const title = typeof body?.title === 'string' ? (body.title as string) : '';
      const issueBody = typeof body?.body === 'string' ? (body.body as string) : '';
      const labels = Array.isArray(body?.labels) ? (body.labels as unknown[]).filter((x): x is string => typeof x === 'string') : [];
      const created = storeCreateIssue(store, repo, title, issueBody, labels);
      if ('error' in created) return { status: 422, body: { message: created.error } };
      return { status: 201, body: issueJson(repo, created) };
    }
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let list = repo.issues.filter((i) => state === 'all' || i.state === state);
      const sort = query.get('sort') ?? 'created';
      const direction = query.get('direction') ?? 'desc';
      if (sort === 'created') {
        list = [...list].sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
      }
      const { page } = paginate(list, query);
      return { status: 200, body: page.map((i) => issueJson(repo, i)) };
    }
    return notFound();
  }
  const num = Number(rest[0]);
  if (!Number.isInteger(num)) return notFound();
  const issue = repo.issues.find((i) => i.number === num);
  if (issue === undefined) return notFound();
  if (rest.length === 1) {
    if (method === 'GET') return { status: 200, body: issueJson(repo, issue) };
    if (method === 'PATCH') {
      if (typeof body?.state === 'string' && (body.state === 'open' || body.state === 'closed')) issue.state = body.state as 'open' | 'closed';
      if (typeof body?.title === 'string') issue.title = body.title as string;
      if (typeof body?.body === 'string') issue.body = body.body as string;
      if (Array.isArray(body?.labels)) {
        const labels = (body.labels as unknown[]).filter((x): x is string => typeof x === 'string');
        for (const l of labels) if (!repo.labels.has(l)) return { status: 422, body: { message: `label ${l} does not exist` } };
        issue.labels = labels;
      }
      return { status: 200, body: issueJson(repo, issue) };
    }
    return notFound();
  }
  if (rest[1] === 'comments') {
    if (method === 'POST') {
      const text = typeof body?.body === 'string' ? (body.body as string) : '';
      const comment = { id: store.nextCommentId(), body: text, seq: store.nextSeq() };
      issue.comments.push(comment);
      return { status: 201, body: { id: comment.id, body: text, user: userJson(), created_at: FIXED_ISO, updated_at: FIXED_ISO } };
    }
    if (method === 'GET') {
      return {
        status: 200,
        body: issue.comments.map((c) => ({ id: c.id, body: c.body, user: userJson(), created_at: FIXED_ISO, updated_at: FIXED_ISO })),
      };
    }
  }
  return notFound();
}

function handlePulls(store: Store, repo: Repo, method: string, rest: string[], query: URLSearchParams, body: Body | null): RestResult {
  if (rest.length === 0) {
    if (method === 'POST') {
      const title = typeof body?.title === 'string' ? (body.title as string) : '';
      const pullBody = typeof body?.body === 'string' ? (body.body as string) : '';
      const head = typeof body?.head === 'string' ? (body.head as string) : '';
      const base = typeof body?.base === 'string' ? (body.base as string) : repo.defaultBranch;
      const headName = head.includes(':') ? (head.split(':').pop() as string) : head;
      const baseName = base.includes(':') ? (base.split(':').pop() as string) : base;
      const created = storeCreatePull(store, repo, title, pullBody, headName, baseName);
      if ('error' in created) return { status: 422, body: { message: created.error } };
      return { status: 201, body: pullJson(store, repo, created) };
    }
    if (method === 'GET') {
      const state = query.get('state') ?? 'open';
      let list = repo.pulls.filter((p) => state === 'all' || p.state === state);
      list = [...list].sort((a, b) => b.seq - a.seq);
      const { page } = paginate(list, query);
      return { status: 200, body: page.map((p) => pullJson(store, repo, p)) };
    }
    return notFound();
  }
  const num = Number(rest[0]);
  if (!Number.isInteger(num)) return notFound();
  const pull = repo.pulls.find((p) => p.number === num);
  if (pull === undefined) return notFound();
  if (rest.length === 1) {
    if (method === 'GET') return { status: 200, body: pullJson(store, repo, pull) };
    return notFound();
  }
  if (rest[1] === 'files' && method === 'GET') {
    const headMap = store.treeMap(repo, pull.headSha);
    return {
      status: 200,
      body: pull.files.map((f) => ({
        filename: f.filename,
        status: f.status,
        additions: f.additions,
        deletions: f.deletions,
        changes: f.changes,
        sha: headMap.get(f.filename) ?? '',
        patch: '',
      })),
    };
  }
  if (rest[1] === 'merge' && method === 'PUT') {
    const merged = storeMergePull(store, repo, pull);
    if (merged.status === 405) return { status: 405, body: { message: merged.message ?? 'merge conflict' } };
    return { status: 200, body: { merged: true, sha: merged.sha ?? '', message: merged.message ?? '' } };
  }
  return notFound();
}
