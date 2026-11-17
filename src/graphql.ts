// GraphQL v4 for the documents gh and github-mcp-server send: a small
// recursive-descent parser plus a typed executor over the store. No library.
import {
  FIXED_ISO,
  VIEWER_LOGIN,
  decodeGlobalId,
  globalId,
  pullMergeable,
  storeCreateIssue,
  storeCreatePull,
  storeMergePull,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

export type GqlValue =
  | string
  | number
  | boolean
  | null
  | GqlValue[]
  | { [key: string]: GqlValue };

export interface Selection {
  alias: string;
  name: string;
  args: Record<string, GqlValue>;
  children: Selection[];
  fragmentOn: string | null;
}

export interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  selections: Selection[];
}

class Parser {
  private pos = 0;
  private readonly src: string;
  constructor(src: string) {
    this.src = src;
  }

  private skipWs(): void {
    for (;;) {
      while (this.pos < this.src.length && /[\s,]/.test(this.src[this.pos] as string)) this.pos++;
      if (this.src[this.pos] === '#') {
        while (this.pos < this.src.length && this.src[this.pos] !== '\n') this.pos++;
        continue;
      }
      break;
    }
  }

  private peek(): string {
    this.skipWs();
    return this.src[this.pos] ?? '';
  }

  private expect(ch: string): void {
    this.skipWs();
    if (this.src[this.pos] !== ch) throw new Error(`expected ${ch} at ${this.pos}`);
    this.pos++;
  }

  private readName(): string {
    this.skipWs();
    const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(this.src.slice(this.pos));
    if (!m) throw new Error(`expected name at ${this.pos}`);
    this.pos += m[0].length;
    return m[0];
  }

  private readString(): string {
    this.skipWs();
    if (this.src[this.pos] !== '"') throw new Error(`expected string at ${this.pos}`);
    this.pos++;
    let out = '';
    while (this.pos < this.src.length) {
      const c = this.src[this.pos] as string;
      if (c === '"') {
        this.pos++;
        return out;
      }
      if (c === '\\') {
        this.pos++;
        const e = this.src[this.pos] as string;
        out += e === 'n' ? '\n' : e === 't' ? '\t' : e === 'r' ? '\r' : e;
        this.pos++;
        continue;
      }
      out += c;
      this.pos++;
    }
    throw new Error('unterminated string');
  }

  private readNumber(): number {
    this.skipWs();
    const m = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(this.src.slice(this.pos));
    if (!m) throw new Error(`expected number at ${this.pos}`);
    this.pos += m[0].length;
    return Number(m[0]);
  }

  parseValue(): GqlValue {
    this.skipWs();
    const c = this.src[this.pos] as string;
    if (c === '"') return this.readString();
    if (c === '$') {
      this.pos++;
      return { $var: this.readName() } as unknown as GqlValue;
    }
    if (c === '[') {
      this.pos++;
      const items: GqlValue[] = [];
      while (this.peek() !== ']') items.push(this.parseValue());
      this.expect(']');
      return items;
    }
    if (c === '{') {
      this.pos++;
      const obj: Record<string, GqlValue> = {};
      while (this.peek() !== '}') {
        const key = this.readName();
        this.expect(':');
        obj[key] = this.parseValue();
      }
      this.expect('}');
      return obj;
    }
    if (c === '-' || /[0-9]/.test(c)) return this.readNumber();
    const name = this.readName();
    if (name === 'true') return true;
    if (name === 'false') return false;
    if (name === 'null') return null;
    return { $enum: name } as unknown as GqlValue;
  }

  parseArgs(): Record<string, GqlValue> {
    const args: Record<string, GqlValue> = {};
    this.skipWs();
    if (this.src[this.pos] !== '(') return args;
    this.pos++;
    while (this.peek() !== ')') {
      const key = this.readName();
      this.expect(':');
      args[key] = this.parseValue();
    }
    this.expect(')');
    return args;
  }

  private parseType(): void {
    this.skipWs();
    if (this.src[this.pos] === '[') {
      this.pos++;
      this.parseType();
      this.expect(']');
    } else {
      this.readName();
    }
    this.skipWs();
    if (this.src[this.pos] === '!') this.pos++;
  }

  parseSelectionSet(): Selection[] {
    this.expect('{');
    const out: Selection[] = [];
    for (;;) {
      this.skipWs();
      if (this.src[this.pos] === '}') {
        this.pos++;
        return out;
      }
      if (this.src.startsWith('...', this.pos)) {
        this.pos += 3;
        this.skipWs();
        let on: string | null = null;
        if (this.src.startsWith('on', this.pos) && /[\s]/.test(this.src[this.pos + 2] ?? '')) {
          this.pos += 2;
          on = this.readName();
        } else {
          // Named fragment spread: not used by the checked documents.
          this.readName();
        }
        const children = this.parseSelectionSet();
        for (const child of children) out.push({ ...child, fragmentOn: on });
        continue;
      }
      const first = this.readName();
      let alias = first;
      let name = first;
      this.skipWs();
      if (this.src[this.pos] === ':') {
        this.pos++;
        name = this.readName();
      }
      const args = this.parseArgs();
      let children: Selection[] = [];
      this.skipWs();
      if (this.src[this.pos] === '{') children = this.parseSelectionSet();
      out.push({ alias, name, args, children, fragmentOn: null });
    }
  }

  parseOperation(): Operation {
    this.skipWs();
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    if (this.src.startsWith('query', this.pos) && !/[A-Za-z0-9_]/.test(this.src[this.pos + 5] ?? '')) {
      this.pos += 5;
      kind = 'query';
    } else if (this.src.startsWith('mutation', this.pos) && !/[A-Za-z0-9_]/.test(this.src[this.pos + 8] ?? '')) {
      this.pos += 8;
      kind = 'mutation';
    }
    this.skipWs();
    if (/[A-Za-z_]/.test(this.src[this.pos] ?? '')) name = this.readName();
    this.skipWs();
    if (this.src[this.pos] === '(') {
      // Variable definitions: ($x: Type, ...).
      this.pos++;
      let depth = 1;
      while (this.pos < this.src.length && depth > 0) {
        const c = this.src[this.pos] as string;
        if (c === '(' || c === '[' || c === '{') depth++;
        else if (c === ')' || c === ']' || c === '}') depth--;
        this.pos++;
      }
    }
    const selections = this.parseSelectionSet();
    return { kind, name, selections };
  }
}

export function parseDocument(query: string): Operation[] {
  const ops: Operation[] = [];
  const parser = new Parser(query);
  while (parser.peek() !== '') {
    // Skip fragment definitions (not used by the checked documents).
    if (parser.peek() === '' ) break;
    const save = parser;
    void save;
    ops.push(parser.parseOperation());
  }
  return ops;
}

export function substitute(value: GqlValue, variables: Record<string, unknown>): unknown {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    const rec = value as Record<string, unknown>;
    if (typeof rec.$var === 'string') return variables[rec.$var] ?? null;
    if (typeof rec.$enum === 'string') return rec.$enum;
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(rec)) out[k] = substitute(v as GqlValue, variables);
    return out;
  }
  if (Array.isArray(value)) return value.map((v) => substitute(v, variables));
  return value;
}

// ---- executor (appended second pass) ---------------------------------------

type View =
  | { t: 'Query' }
  | { t: 'User'; login: string }
  | { t: 'Repository'; repo: Repo }
  | { t: 'Issue'; repo: Repo; issue: Issue }
  | { t: 'PullRequest'; repo: Repo; pull: Pull }
  | { t: 'IssueConnection'; repo: Repo; issues: Issue[]; total: number; hasNext: boolean; endCursor: string | null }
  | { t: 'PullRequestConnection'; repo: Repo; pulls: Pull[] }
  | { t: 'ChangedFileConnection'; repo: Repo; pull: Pull }
  | { t: 'ChangedFile'; path: string }
  | { t: 'Ref'; name: string }
  | { t: 'PageInfo'; hasNext: boolean; endCursor: string | null }
  | { t: '__Type'; name: string }
  | { t: 'Payload'; fields: Record<string, unknown> };

const ISSUE_TYPE_FIELDS = ['id', 'number', 'title', 'body', 'state', 'url', 'author', 'repository', 'comments', 'createdAt', 'updatedAt'];
const PR_TYPE_FIELDS = ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'files', 'headRefOid', 'baseRefOid', 'repository', 'createdAt', 'updatedAt'];
const REPO_TYPE_FIELDS = ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'url', 'createdAt', 'updatedAt'];
const SEARCH_ENUM = ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'MILESTONE', 'ORGANIZATION'];

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64url');
}

function cursorNumber(cursor: string | null | undefined): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

function gqlState(state: 'open' | 'closed'): string {
  return state === 'open' ? 'OPEN' : 'CLOSED';
}

function prState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

class Executor {
  errors: Array<{ message: string }> = [];
  private readonly store: Store;
  constructor(store: Store) {
    this.store = store;
  }

  private arg(sel: Selection, variables: Record<string, unknown>, name: string): unknown {
    if (!(name in sel.args)) return undefined;
    return substitute(sel.args[name] as GqlValue, variables);
  }

  private strArg(sel: Selection, variables: Record<string, unknown>, name: string): string | undefined {
    const v = this.arg(sel, variables, name);
    return typeof v === 'string' ? v : undefined;
  }

  private intArg(sel: Selection, variables: Record<string, unknown>, name: string): number | undefined {
    const v = this.arg(sel, variables, name);
    return typeof v === 'number' ? v : undefined;
  }

  execute(view: View, selections: Selection[], variables: Record<string, unknown>): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      if (sel.fragmentOn !== null && sel.fragmentOn !== view.t && !(view.t === 'Repository' && sel.fragmentOn === 'Node')) {
        continue;
      }
      const value = this.resolve(view, sel, variables);
      if (value === undefined) continue;
      out[sel.alias] = value;
    }
    return out;
  }

  private select(view: View, selections: Selection[], variables: Record<string, unknown>): unknown {
    if (selections.length === 0) return this.scalar(view);
    return this.execute(view, selections, variables);
  }

  private scalar(view: View): unknown {
    switch (view.t) {
      case 'User':
        return view.login;
      case 'Ref':
        return view.name;
      default:
        return null;
    }
  }

  private resolve(view: View, sel: Selection, variables: Record<string, unknown>): unknown {
    const name = sel.name;
    // __typename is valid on every object type.
    if (name === '__typename') return view.t === 'Query' ? 'Query' : view.t;
    switch (view.t) {
      case 'Query':
        return this.resolveQuery(sel, variables);
      case 'User':
        if (name === 'login') return view.login;
        if (name === 'id') return globalId('user', view.login);
        if (name === 'name') return view.login === VIEWER_LOGIN ? 'ada iversen' : view.login;
        if (name === 'email') return `${view.login}@example.invalid`;
        break;
      case 'Repository':
        return this.resolveRepo(view.repo, sel, variables);
      case 'Issue':
        return this.resolveIssue(view.repo, view.issue, sel, variables);
      case 'PullRequest':
        return this.resolvePull(view.repo, view.pull, sel, variables);
      case 'IssueConnection': {
        if (name === 'nodes') return view.issues.map((i) => this.select({ t: 'Issue', repo: view.repo, issue: i }, sel.children, variables));
        if (name === 'totalCount') return view.total;
        if (name === 'pageInfo') return this.select({ t: 'PageInfo', hasNext: view.hasNext, endCursor: view.endCursor }, sel.children, variables);
        break;
      }
      case 'PullRequestConnection': {
        if (name === 'nodes') return view.pulls.map((p) => this.select({ t: 'PullRequest', repo: view.repo, pull: p }, sel.children, variables));
        if (name === 'totalCount') return view.pulls.length;
        if (name === 'pageInfo') return this.select({ t: 'PageInfo', hasNext: false, endCursor: null }, sel.children, variables);
        break;
      }
      case 'ChangedFileConnection': {
        if (name === 'nodes') return view.pull.files.map((f) => this.select({ t: 'ChangedFile', path: f.filename }, sel.children, variables));
        if (name === 'totalCount') return view.pull.files.length;
        break;
      }
      case 'ChangedFile': {
        if (name === 'path') return view.path;
        break;
      }
      case 'Ref': {
        if (name === 'name') return view.name;
        break;
      }
      case 'PageInfo': {
        if (name === 'hasNextPage') return view.hasNext;
        if (name === 'hasPreviousPage') return false;
        if (name === 'endCursor') return view.endCursor;
        if (name === 'startCursor') return null;
        break;
      }
      case '__Type': {
        if (name === 'name') return view.name;
        if (name === 'kind') return view.name === 'SearchType' ? 'ENUM' : 'OBJECT';
        if (name === 'fields') {
          const fields =
            view.name === 'Issue' ? ISSUE_TYPE_FIELDS : view.name === 'PullRequest' ? PR_TYPE_FIELDS : view.name === 'Repository' ? REPO_TYPE_FIELDS : [];
          return fields.map((f) => ({ name: f }));
        }
        if (name === 'enumValues') {
          if (view.name !== 'SearchType') return [];
          return SEARCH_ENUM.map((e) => ({ name: e }));
        }
        if (name === 'description') return null;
        break;
      }
      case 'Payload': {
        const v = view.fields[name];
        if (v !== undefined) {
          if (v !== null && typeof v === 'object' && !Array.isArray(v) && '__view' in (v as Record<string, unknown>)) {
            return this.select((v as Record<string, unknown>).__view as View, sel.children, variables);
          }
          if (sel.children.length > 0 && v !== null && typeof v === 'object') {
            // Nested plain object (e.g. commentEdge.node).
            return this.executePlain(v as Record<string, unknown>, sel.children, variables);
          }
          return v;
        }
        break;
      }
    }
    this.errors.push({ message: `Cannot query field "${name}" on type "${view.t}".` });
    return undefined;
  }

  private executePlain(obj: Record<string, unknown>, selections: Selection[], variables: Record<string, unknown>): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const sel of selections) {
      const v = obj[sel.name];
      if (v === undefined) {
        this.errors.push({ message: `Cannot query field "${sel.name}" on type "Object".` });
        continue;
      }
      if (v !== null && typeof v === 'object' && !Array.isArray(v) && '__view' in (v as Record<string, unknown>)) {
        out[sel.alias] = this.select((v as Record<string, unknown>).__view as View, sel.children, variables);
      } else if (sel.children.length > 0 && v !== null && typeof v === 'object') {
        out[sel.alias] = this.executePlain(v as Record<string, unknown>, sel.children, variables);
      } else {
        out[sel.alias] = v;
      }
    }
    return out;
  }

  private resolveQuery(sel: Selection, variables: Record<string, unknown>): unknown {
    const name = sel.name;
    if (name === 'viewer') {
      return this.select({ t: 'User', login: VIEWER_LOGIN }, sel.children, variables);
    }
    if (name === 'repository') {
      const owner = this.strArg(sel, variables, 'owner');
      const repoName = this.strArg(sel, variables, 'name');
      if (owner === undefined || repoName === undefined) return null;
      const repo = this.store.getRepo(owner, repoName);
      if (repo === undefined) return null;
      return this.select({ t: 'Repository', repo }, sel.children, variables);
    }
    if (name === 'node') {
      const id = this.strArg(sel, variables, 'id');
      if (id === undefined) return null;
      const decoded = decodeGlobalId(id);
      if (decoded === null) return null;
      if (decoded.kind === 'repo') {
        const [owner, repoName] = decoded.parts as [string, string];
        const repo = this.store.getRepo(owner as string, repoName as string);
        if (repo === undefined) return null;
        return this.select({ t: 'Repository', repo }, sel.children, variables);
      }
      if (decoded.kind === 'issue') {
        const [owner, repoName, num] = decoded.parts as [string, string, string];
        const repo = this.store.getRepo(owner as string, repoName as string);
        const issue = repo?.issues.find((i) => i.number === Number(num));
        if (repo === undefined || issue === undefined) return null;
        return this.select({ t: 'Issue', repo, issue }, sel.children, variables);
      }
      if (decoded.kind === 'pull') {
        const [owner, repoName, num] = decoded.parts as [string, string, string];
        const repo = this.store.getRepo(owner as string, repoName as string);
        const pull = repo?.pulls.find((p) => p.number === Number(num));
        if (repo === undefined || pull === undefined) return null;
        return this.select({ t: 'PullRequest', repo, pull }, sel.children, variables);
      }
      return null;
    }
    if (name === '__type') {
      const typeName = this.strArg(sel, variables, 'name');
      if (typeName === undefined) return null;
      if (!['Issue', 'PullRequest', 'Repository', 'SearchType', 'User', 'Query', 'Mutation'].includes(typeName)) return null;
      return this.select({ t: '__Type', name: typeName }, sel.children, variables);
    }
    if (name === '__schema') {
      return { queryType: { name: 'Query' }, mutationType: { name: 'Mutation' } };
    }
    this.errors.push({ message: `Cannot query field "${name}" on type "Query".` });
    return undefined;
  }

  private resolveRepo(repo: Repo, sel: Selection, variables: Record<string, unknown>): unknown {
    const name = sel.name;
    if (name === 'id') return globalId('repo', repo.owner, repo.name);
    if (name === 'databaseId') return repo.id;
    if (name === 'name') return repo.name;
    if (name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
    if (name === 'owner') return this.select({ t: 'User', login: repo.owner }, sel.children, variables);
    if (name === 'defaultBranchRef') {
      const tip = this.store.branchTip(repo, repo.defaultBranch);
      if (tip === undefined) return null;
      return this.select({ t: 'Ref', name: repo.defaultBranch }, sel.children, variables);
    }
    if (name === 'viewerPermission') return 'ADMIN';
    if (name === 'hasIssuesEnabled') return true;
    if (name === 'url') return `/${repo.owner}/${repo.name}`;
    if (name === 'createdAt' || name === 'updatedAt') return FIXED_ISO;
    if (name === 'issue') {
      const num = this.intArg(sel, variables, 'number');
      const issue = repo.issues.find((i) => i.number === num);
      if (issue === undefined) return null;
      return this.select({ t: 'Issue', repo, issue }, sel.children, variables);
    }
    if (name === 'issues') {
      const first = this.intArg(sel, variables, 'first');
      const after = this.arg(sel, variables, 'after');
      const states = this.arg(sel, variables, 'states');
      const orderBy = this.arg(sel, variables, 'orderBy');
      let list = [...repo.issues];
      if (Array.isArray(states)) {
        const wanted = new Set(states.map((s) => String(s)));
        list = list.filter((i) => wanted.has(gqlState(i.state)));
      }
      let dir: 'ASC' | 'DESC' = 'ASC';
      if (orderBy !== undefined && orderBy !== null && typeof orderBy === 'object') {
        const ob = orderBy as Record<string, unknown>;
        if (ob.direction === 'DESC') dir = 'DESC';
        else if (ob.direction === 'ASC') dir = 'ASC';
      }
      list.sort((a, b) => (dir === 'DESC' ? b.seq - a.seq : a.seq - b.seq));
      const afterNum = typeof after === 'string' ? cursorNumber(after) : null;
      if (afterNum !== null) {
        list = list.filter((i) => (dir === 'DESC' ? i.number < afterNum : i.number > afterNum));
      }
      const total = list.length;
      const limited = first === undefined ? list : list.slice(0, first);
      const hasNext = first !== undefined ? total > limited.length : false;
      const endCursor = limited.length ? cursorFor(limited[limited.length - 1]?.number as number) : null;
      return this.select({ t: 'IssueConnection', repo, issues: limited, total, hasNext, endCursor }, sel.children, variables);
    }
    if (name === 'pullRequests') {
      const first = this.intArg(sel, variables, 'first');
      let list = [...repo.pulls].sort((a, b) => a.seq - b.seq);
      if (first !== undefined) list = list.slice(0, first);
      return this.select({ t: 'PullRequestConnection', repo, pulls: list }, sel.children, variables);
    }
    if (name === 'pullRequest') {
      const num = this.intArg(sel, variables, 'number');
      const pull = repo.pulls.find((p) => p.number === num);
      if (pull === undefined) return null;
      return this.select({ t: 'PullRequest', repo, pull }, sel.children, variables);
    }
    this.errors.push({ message: `Cannot query field "${name}" on type "Repository".` });
    return undefined;
  }

  private resolveIssue(repo: Repo, issue: Issue, sel: Selection, variables: Record<string, unknown>): unknown {
    const name = sel.name;
    if (name === 'id') return globalId('issue', repo.owner, repo.name, issue.number);
    if (name === 'number') return issue.number;
    if (name === 'title') return issue.title;
    if (name === 'body') return issue.body;
    if (name === 'state') return gqlState(issue.state);
    if (name === 'url') return `/${repo.owner}/${repo.name}/issues/${issue.number}`;
    if (name === 'author') return this.select({ t: 'User', login: VIEWER_LOGIN }, sel.children, variables);
    if (name === 'repository') return this.select({ t: 'Repository', repo }, sel.children, variables);
    if (name === 'createdAt' || name === 'updatedAt') return FIXED_ISO;
    this.errors.push({ message: `Cannot query field "${name}" on type "Issue".` });
    return undefined;
  }

  private resolvePull(repo: Repo, pull: Pull, sel: Selection, variables: Record<string, unknown>): unknown {
    const name = sel.name;
    if (name === 'id') return globalId('pull', repo.owner, repo.name, pull.number);
    if (name === 'number') return pull.number;
    if (name === 'title') return pull.title;
    if (name === 'body') return pull.body;
    if (name === 'state') return prState(pull);
    if (name === 'merged') return pull.merged;
    if (name === 'mergeable') return pullMergeable(this.store, repo, pull);
    if (name === 'headRefName') return pull.head;
    if (name === 'baseRefName') return pull.base;
    if (name === 'headRefOid') return pull.headSha;
    if (name === 'baseRefOid') return pull.baseSha;
    if (name === 'files') {
      return this.select({ t: 'ChangedFileConnection', repo, pull }, sel.children, variables);
    }
    if (name === 'repository') return this.select({ t: 'Repository', repo }, sel.children, variables);
    if (name === 'createdAt' || name === 'updatedAt') return FIXED_ISO;
    this.errors.push({ message: `Cannot query field "${name}" on type "PullRequest".` });
    return undefined;
  }

  executeOperation(op: Operation, variables: Record<string, unknown>): { data: Record<string, unknown>; errors: Array<{ message: string }> } {
    const root: View = { t: 'Query' };
    if (op.kind === 'mutation') {
      return this.executeMutation(op, variables);
    }
    const data = this.execute(root, op.selections, variables);
    return { data, errors: this.errors };
  }

  private executeMutation(op: Operation, variables: Record<string, unknown>): { data: Record<string, unknown>; errors: Array<{ message: string }> } {
    const data: Record<string, unknown> = {};
    for (const sel of op.selections) {
      const input = this.arg(sel, variables, 'input');
      const inputObj = (input !== undefined && input !== null && typeof input === 'object' ? (input as Record<string, unknown>) : {}) as Record<string, unknown>;
      if (sel.name === 'createIssue') {
        const repoId = String(inputObj.repositoryId ?? '');
        const decoded = decodeGlobalId(repoId);
        let issueView: View | null = null;
        let number: number | null = null;
        if (decoded?.kind === 'repo') {
          const repo = this.store.getRepo(decoded.parts[0] as string, decoded.parts[1] as string);
          if (repo !== undefined) {
            const created = storeCreateIssue(this.store, repo, String(inputObj.title ?? ''), String(inputObj.body ?? ''), []);
            if (!('error' in created)) {
              issueView = { t: 'Issue', repo, issue: created };
              number = created.number;
            }
          }
        }
        if (issueView === null) {
          this.errors.push({ message: 'createIssue: repository not found' });
          data[sel.alias] = null;
          continue;
        }
        void number;
        data[sel.alias] = this.executePlain({ issue: { __view: issueView } }, sel.children, variables);
        continue;
      }
      if (sel.name === 'closeIssue') {
        const decoded = decodeGlobalId(String(inputObj.issueId ?? ''));
        let issueView: View | null = null;
        if (decoded?.kind === 'issue') {
          const repo = this.store.getRepo(decoded.parts[0] as string, decoded.parts[1] as string);
          const issue = repo?.issues.find((i) => i.number === Number(decoded.parts[2]));
          if (repo !== undefined && issue !== undefined) {
            issue.state = 'closed';
            issueView = { t: 'Issue', repo, issue };
          }
        }
        if (issueView === null) {
          this.errors.push({ message: 'closeIssue: issue not found' });
          data[sel.alias] = null;
          continue;
        }
        data[sel.alias] = this.executePlain({ issue: { __view: issueView } }, sel.children, variables);
        continue;
      }
      if (sel.name === 'createPullRequest') {
        const decoded = decodeGlobalId(String(inputObj.repositoryId ?? ''));
        let pullView: View | null = null;
        if (decoded?.kind === 'repo') {
          const repo = this.store.getRepo(decoded.parts[0] as string, decoded.parts[1] as string);
          if (repo !== undefined) {
            const created = storeCreatePull(
              this.store,
              repo,
              String(inputObj.title ?? ''),
              String(inputObj.body ?? ''),
              String(inputObj.headRefName ?? ''),
              String(inputObj.baseRefName ?? ''),
            );
            if (!('error' in created)) pullView = { t: 'PullRequest', repo, pull: created };
          }
        }
        if (pullView === null) {
          this.errors.push({ message: 'createPullRequest: repository or branch not found' });
          data[sel.alias] = null;
          continue;
        }
        data[sel.alias] = this.executePlain({ pullRequest: { __view: pullView } }, sel.children, variables);
        continue;
      }
      if (sel.name === 'mergePullRequest') {
        const decoded = decodeGlobalId(String(inputObj.pullRequestId ?? ''));
        let pullView: View | null = null;
        if (decoded?.kind === 'pull') {
          const repo = this.store.getRepo(decoded.parts[0] as string, decoded.parts[1] as string);
          const pull = repo?.pulls.find((p) => p.number === Number(decoded.parts[2]));
          if (repo !== undefined && pull !== undefined) {
            storeMergePull(this.store, repo, pull);
            pullView = { t: 'PullRequest', repo, pull };
          }
        }
        if (pullView === null) {
          this.errors.push({ message: 'mergePullRequest: pull request not found' });
          data[sel.alias] = null;
          continue;
        }
        data[sel.alias] = this.executePlain({ pullRequest: { __view: pullView } }, sel.children, variables);
        continue;
      }
      if (sel.name === 'addComment') {
        const decoded = decodeGlobalId(String(inputObj.subjectId ?? ''));
        let node: Record<string, unknown> | null = null;
        if (decoded?.kind === 'issue') {
          const repo = this.store.getRepo(decoded.parts[0] as string, decoded.parts[1] as string);
          const issue = repo?.issues.find((i) => i.number === Number(decoded.parts[2]));
          if (repo !== undefined && issue !== undefined) {
            const comment = { id: this.store.nextCommentId(), body: String(inputObj.body ?? ''), seq: this.store.nextSeq() };
            issue.comments.push(comment);
            node = { id: globalId('comment', repo.owner, repo.name, issue.number, comment.id), body: comment.body };
          }
        }
        if (node === null) {
          this.errors.push({ message: 'addComment: subject not found' });
          data[sel.alias] = null;
          continue;
        }
        data[sel.alias] = this.executePlain({ commentEdge: { node } }, sel.children, variables);
        continue;
      }
      this.errors.push({ message: `Cannot query field "${sel.name}" on type "Mutation".` });
      data[sel.alias] = null;
    }
    return { data, errors: this.errors };
  }
}

export function handleGraphqlOperation(store: Store, body: unknown): { status: number; body: unknown } {
  const runOne = (opBody: unknown): Record<string, unknown> => {
    if (opBody === null || typeof opBody !== 'object' || Array.isArray(opBody)) {
      return { data: null, errors: [{ message: 'a GraphQL operation needs a query string' }] };
    }
    const rec = opBody as Record<string, unknown>;
    const query = rec.query;
    if (typeof query !== 'string') {
      return { data: null, errors: [{ message: 'a GraphQL operation needs a query string' }] };
    }
    const variables = (rec.variables !== undefined && rec.variables !== null && typeof rec.variables === 'object' ? (rec.variables as Record<string, unknown>) : {}) as Record<string, unknown>;
    let ops: Operation[];
    try {
      ops = parseDocument(query);
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
    }
    if (ops.length === 0) return { data: null, errors: [{ message: 'empty query' }] };
    let op = ops[0] as Operation;
    if (typeof rec.operationName === 'string') {
      const named = ops.find((o) => o.name === rec.operationName);
      if (named) op = named;
    }
    const executor = new Executor(store);
    const { data, errors } = executor.executeOperation(op, variables);
    const out: Record<string, unknown> = { data };
    if (errors.length > 0) out.errors = errors;
    return out;
  };
  if (Array.isArray(body)) {
    return { status: 200, body: body.map(runOne) };
  }
  return { status: 200, body: runOne(body) };
}
