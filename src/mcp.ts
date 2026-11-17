// MCP over Streamable HTTP at /mcp: modern server/discover era plus the
// legacy initialize handshake. Six tools over the same store as REST.
import {
  VIEWER_LOGIN,
  storeCreateIssue,
  storeCreatePull,
  storeMergePull,
  type Repo,
  type Store,
} from './store.ts';
import { issueJson, pullJson, repoJson } from './rest.ts';

const SERVER_INFO = { name: 'github-core-clone', version: '1.0.0' };

interface ToolDef {
  name: string;
  description: string;
  properties: Record<string, { type: string; description: string }>;
  required: string[];
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    properties: {
      owner: { type: 'string', description: 'The owner login.' },
      name: { type: 'string', description: 'The repository name.' },
      description: { type: 'string', description: 'A description of the repository.' },
      private: { type: 'boolean', description: 'Whether the repository is private.' },
    },
    required: ['name'],
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    properties: {
      owner: { type: 'string', description: 'The owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    properties: {
      owner: { type: 'string', description: 'The owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The issue title.' },
      body: { type: 'string', description: 'The issue body.' },
      labels: { type: 'array', description: 'Labels to apply to the issue, by name.' },
    },
    required: ['owner', 'repo', 'title'],
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    properties: {
      owner: { type: 'string', description: 'The owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'Issue state: open, closed or all.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    properties: {
      owner: { type: 'string', description: 'The owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The pull request title.' },
      body: { type: 'string', description: 'The pull request body.' },
      head: { type: 'string', description: 'The head branch name.' },
      base: { type: 'string', description: 'The base branch name.' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request.',
    properties: {
      owner: { type: 'string', description: 'The owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      pull_number: { type: 'integer', description: 'The pull request number.' },
    },
    required: ['owner', 'repo', 'pull_number'],
  },
];

function toolsList(): Array<Record<string, unknown>> {
  return TOOLS.map((t) => ({
    name: t.name,
    description: t.description,
    inputSchema: {
      type: 'object',
      properties: t.properties,
      required: t.required,
    },
  }));
}

function toolResult(obj: unknown): Record<string, unknown> {
  const text = JSON.stringify(obj);
  return { content: [{ type: 'text', text }], structuredContent: obj, isError: false };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function missingRequired(tool: ToolDef, args: Record<string, unknown>): string[] {
  return tool.required.filter((r) => args[r] === undefined || args[r] === null || args[r] === '');
}

function callTool(store: Store, name: string, args: Record<string, unknown>): { rpcError?: { code: number; message: string }; result?: Record<string, unknown> } {
  const tool = TOOLS.find((t) => t.name === name);
  if (tool === undefined) return { rpcError: { code: -32602, message: `unknown tool: ${name}` } };
  const missing = missingRequired(tool, args);
  if (missing.length > 0) {
    return { rpcError: { code: -32602, message: `missing required arguments: ${missing.join(', ')}` } };
  }
  const repoName = typeof args.repo === 'string' ? (args.repo as string) : '';
  const owner = typeof args.owner === 'string' ? (args.owner as string) : VIEWER_LOGIN;
  if (name === 'create_repo') {
    const repo = store.createRepo(
      typeof args.name === 'string' ? (args.name as string) : '',
      args.private === true,
      typeof args.description === 'string' ? (args.description as string) : '',
    );
    if (repo === null) return { result: toolError('repository already exists') };
    return { result: toolResult(repoJson(repo)) };
  }
  const repo: Repo | undefined = store.getRepo(owner, repoName);
  if (repo === undefined) return { result: toolError(`repository ${owner}/${repoName} not found`) };
  if (name === 'get_repo') return { result: toolResult(repoJson(repo)) };
  if (name === 'create_issue') {
    const labels = Array.isArray(args.labels) ? (args.labels as unknown[]).map((l) => String(l)) : [];
    const created = storeCreateIssue(store, repo, String(args.title ?? ''), String(args.body ?? ''), labels);
    if ('error' in created) return { result: toolError(created.error) };
    return { result: toolResult(issueJson(repo, created)) };
  }
  if (name === 'list_issues') {
    const state = typeof args.state === 'string' ? (args.state as string) : 'open';
    const list = repo.issues.filter((i) => state === 'all' || i.state === state);
    const items = list.map((i) => issueJson(repo, i));
    return { result: toolResult({ issues: items }) };
  }
  if (name === 'create_pull_request') {
    const created = storeCreatePull(
      store,
      repo,
      String(args.title ?? ''),
      String(args.body ?? ''),
      String(args.head ?? ''),
      String(args.base ?? ''),
    );
    if ('error' in created) return { result: toolError(created.error) };
    return { result: toolResult(pullJson(store, repo, created)) };
  }
  if (name === 'merge_pull_request') {
    const num = Number(args.pull_number);
    const pull = repo.pulls.find((p) => p.number === num);
    if (pull === undefined) return { result: toolError(`pull request #${num} not found`) };
    const merged = storeMergePull(store, repo, pull);
    if (merged.status !== 200) return { result: toolError(merged.message ?? 'merge conflict') };
    return { result: toolResult(pullJson(store, repo, pull)) };
  }
  return { rpcError: { code: -32602, message: `unknown tool: ${name}` } };
}

export function handleMcp(store: Store, body: unknown): { status: number; body: unknown } {
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } } };
  }
  const rec = body as Record<string, unknown>;
  const id = rec.id === undefined ? null : rec.id;
  const method = typeof rec.method === 'string' ? rec.method : '';
  const params = (rec.params !== undefined && rec.params !== null && typeof rec.params === 'object' ? (rec.params as Record<string, unknown>) : {}) as Record<string, unknown>;
  const ok = (result: unknown): { status: number; body: unknown } => ({ status: 200, body: { jsonrpc: '2.0', id, result } });
  const err = (code: number, message: string): { status: number; body: unknown } => ({
    status: 200,
    body: { jsonrpc: '2.0', id, error: { code, message } },
  });
  // Notifications carry no id: acknowledge with 202 and no body.
  if (rec.id === undefined) {
    return { status: 202, body: null };
  }
  if (method === 'server/discover') {
    return ok({
      supportedVersions: ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26'],
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
      _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
    });
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? (params.protocolVersion as string) : '2025-11-25';
    return ok({
      protocolVersion: requested,
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
    });
  }
  if (method === 'ping') return ok({});
  if (method === 'tools/list') {
    return ok({ tools: toolsList() });
  }
  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? (params.name as string) : '';
    const args = (params.arguments !== undefined && params.arguments !== null && typeof params.arguments === 'object' ? (params.arguments as Record<string, unknown>) : {}) as Record<string, unknown>;
    const outcome = callTool(store, toolName, args);
    if (outcome.rpcError !== undefined) return err(outcome.rpcError.code, outcome.rpcError.message);
    return ok(outcome.result ?? {});
  }
  return err(-32601, `method not found: ${method}`);
}
