// MCP over Streamable HTTP at /mcp: modern server/discover era plus the
// legacy initialize handshake. Six tools over the same store as REST.
package main

import (
	"encoding/json"
	"strings"
)

var mcpServerInfo = map[string]any{"name": "github-core-clone", "version": "1.0.0"}

type toolDef struct {
	name        string
	description string
	properties  map[string]toolProp
	required    []string
}

type toolProp struct {
	typ         string
	description string
}

var mcpTools = []toolDef{
	{
		name: "create_repo", description: "Create a repository owned by the authenticated user.",
		properties: map[string]toolProp{
			"owner":       {"string", "The owner login."},
			"name":        {"string", "The repository name."},
			"description": {"string", "A description of the repository."},
			"private":     {"boolean", "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name: "get_repo", description: "Get a repository by owner and name.",
		properties: map[string]toolProp{
			"owner": {"string", "The owner login."},
			"repo":  {"string", "The repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_issue", description: "Create an issue in a repository.",
		properties: map[string]toolProp{
			"owner":  {"string", "The owner login."},
			"repo":   {"string", "The repository name."},
			"title":  {"string", "The issue title."},
			"body":   {"string", "The issue body."},
			"labels": {"array", "Labels to apply to the issue, by name."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name: "list_issues", description: "List issues in a repository.",
		properties: map[string]toolProp{
			"owner": {"string", "The owner login."},
			"repo":  {"string", "The repository name."},
			"state": {"string", "Issue state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_pull_request", description: "Create a pull request from a head branch into a base branch.",
		properties: map[string]toolProp{
			"owner": {"string", "The owner login."},
			"repo":  {"string", "The repository name."},
			"title": {"string", "The pull request title."},
			"body":  {"string", "The pull request body."},
			"head":  {"string", "The head branch name."},
			"base":  {"string", "The base branch name."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name: "merge_pull_request", description: "Merge a pull request.",
		properties: map[string]toolProp{
			"owner":       {"string", "The owner login."},
			"repo":        {"string", "The repository name."},
			"pull_number": {"integer", "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

// toolsListJSON is the pre-serialized tools/list payload: two calls answer
// the same list, byte for byte. Property order is fixed by construction.
var toolsListJSON = func() []byte {
	var b strings.Builder
	b.WriteString(`{"tools":[`)
	for i, t := range mcpTools {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":`)
		writeJSONString(&b, t.name)
		b.WriteString(`,"description":`)
		writeJSONString(&b, t.description)
		b.WriteString(`,"inputSchema":{"type":"object","properties":{`)
		// Fixed property order per tool, matching the definition order above.
		props := toolPropOrder(t.name)
		for j, key := range props {
			if j > 0 {
				b.WriteByte(',')
			}
			prop := t.properties[key]
			writeJSONString(&b, key)
			b.WriteString(`:{"type":`)
			writeJSONString(&b, prop.typ)
			b.WriteString(`,"description":`)
			writeJSONString(&b, prop.description)
			b.WriteByte('}')
		}
		b.WriteString(`},"required":[`)
		for j, r := range t.required {
			if j > 0 {
				b.WriteByte(',')
			}
			writeJSONString(&b, r)
		}
		b.WriteString(`]}}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}()

// toolPropOrder fixes each tool's property order (Go maps do not order).
func toolPropOrder(name string) []string {
	switch name {
	case "create_repo":
		return []string{"owner", "name", "description", "private"}
	case "get_repo":
		return []string{"owner", "repo"}
	case "create_issue":
		return []string{"owner", "repo", "title", "body", "labels"}
	case "list_issues":
		return []string{"owner", "repo", "state"}
	case "create_pull_request":
		return []string{"owner", "repo", "title", "body", "head", "base"}
	case "merge_pull_request":
		return []string{"owner", "repo", "pull_number"}
	}
	return nil
}

func writeJSONString(b *strings.Builder, s string) {
	raw, _ := json.Marshal(s)
	b.Write(raw)
}

func toolResult(obj any) map[string]any {
	raw, _ := json.Marshal(obj)
	return map[string]any{
		"content":          []any{map[string]any{"type": "text", "text": string(raw)}},
		"structuredContent": obj,
		"isError":          false,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	}
}

type mcpOutcome struct {
	rpcCode    int
	rpcMessage string
	result     map[string]any
	isError    bool
}

func callTool(s *Store, name string, args map[string]any) mcpOutcome {
	var tool *toolDef
	for i := range mcpTools {
		if mcpTools[i].name == name {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return mcpOutcome{rpcCode: -32602, rpcMessage: "unknown tool: " + name, isError: true}
	}
	var missing []string
	for _, r := range tool.required {
		v, ok := args[r]
		if !ok || v == nil || v == "" {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return mcpOutcome{rpcCode: -32602, rpcMessage: "missing required arguments: " + strings.Join(missing, ", "), isError: true}
	}
	argStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	repoName := argStr("repo")
	owner := argStr("owner")
	if owner == "" {
		owner = ViewerLogin
	}
	if name == "create_repo" {
		repo := s.CreateRepo(argStr("name"), args["private"] == true, argStr("description"))
		if repo == nil {
			return mcpOutcome{result: toolError("repository already exists")}
		}
		return mcpOutcome{result: toolResult(repoJSON(repo))}
	}
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		return mcpOutcome{result: toolError("repository " + owner + "/" + repoName + " not found")}
	}
	switch name {
	case "get_repo":
		return mcpOutcome{result: toolResult(repoJSON(repo))}
	case "create_issue":
		var labels []string
		if arr, ok := args["labels"].([]any); ok {
			for _, l := range arr {
				labels = append(labels, jsonStringify(l))
			}
		}
		created, errMsg := s.StoreCreateIssue(repo, argStr("title"), argStr("body"), labels)
		if errMsg != "" {
			return mcpOutcome{result: toolError(errMsg)}
		}
		return mcpOutcome{result: toolResult(issueJSON(repo, created))}
	case "list_issues":
		state := argStr("state")
		if state == "" {
			state = "open"
		}
		items := make([]issueObj, 0)
		for _, i := range repo.Issues {
			if state == "all" || i.State == state {
				items = append(items, issueJSON(repo, i))
			}
		}
		return mcpOutcome{result: toolResult(map[string]any{"issues": items})}
	case "create_pull_request":
		created, errMsg := s.StoreCreatePull(repo, argStr("title"), argStr("body"), argStr("head"), argStr("base"))
		if errMsg != "" {
			return mcpOutcome{result: toolError(errMsg)}
		}
		return mcpOutcome{result: toolResult(pullJSON(s, repo, created))}
	case "merge_pull_request":
		num := 0
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case int:
			num = v
		}
		var pull *Pull
		for _, p := range repo.Pulls {
			if p.Number == num {
				pull = p
				break
			}
		}
		if pull == nil {
			return mcpOutcome{result: toolError("pull request #" + itoa(num) + " not found")}
		}
		status, _, message := s.StoreMergePull(repo, pull)
		if status != 200 {
			return mcpOutcome{result: toolError(message)}
		}
		return mcpOutcome{result: toolResult(pullJSON(s, repo, pull))}
	}
	return mcpOutcome{rpcCode: -32602, rpcMessage: "unknown tool: " + name, isError: true}
}

// jsonStringify renders a JSON value as its shortest string form.
func jsonStringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

// HandleMcp serves one MCP request body. The id is carried as raw JSON so
// numeric and string ids round-trip exactly.
func HandleMcp(s *Store, body map[string]any, hasID bool, idRaw json.RawMessage) (int, any) {
	if body == nil {
		return 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}}
	}
	method, _ := body["method"].(string)
	params, _ := body["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	var id any
	if hasID {
		id = json.RawMessage(append([]byte(nil), idRaw...))
	}
	// Notifications carry no id: acknowledge with 202 and no body.
	if !hasID {
		return 202, nil
	}
	ok := func(result any) (int, any) {
		return 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	}
	errResp := func(code int, message string) (int, any) {
		return 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
	}
	switch method {
	case "server/discover":
		return ok(map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})
	case "initialize":
		requested := "2025-11-25"
		if v, ok := params["protocolVersion"].(string); ok {
			requested = v
		}
		return ok(map[string]any{
			"protocolVersion": requested,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo,
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		// Pre-serialized payload: byte-identical across calls.
		return 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(toolsListJSON)}
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		outcome := callTool(s, name, args)
		if outcome.isError {
			return errResp(outcome.rpcCode, outcome.rpcMessage)
		}
		return ok(outcome.result)
	default:
		return errResp(-32601, "method not found: "+method)
	}
}
