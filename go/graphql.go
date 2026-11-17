// GraphQL v4 for the documents gh and github-mcp-server send: a small
// recursive-descent parser plus a typed executor over the store. No library.
package main

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

// gqlValue is a parsed GraphQL literal; $var and $enum markers are kept as
// single-key maps so substitute() can resolve them against variables.
type gqlValue any

type selection struct {
	alias      string
	name       string
	args       map[string]gqlValue
	children   []*selection
	fragmentOn string
	hasFrag    bool
}

type operation struct {
	kind       string // "query" | "mutation"
	name       string
	selections []*selection
}

type gqlParser struct {
	src string
	pos int
}

func (p *gqlParser) skipWs() {
	for {
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
				p.pos++
			} else {
				break
			}
		}
		if p.pos < len(p.src) && p.src[p.pos] == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *gqlParser) peek() byte {
	p.skipWs()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *gqlParser) expect(ch byte) error {
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != ch {
		return errGQLParse
	}
	p.pos++
	return nil
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func (p *gqlParser) readName() (string, error) {
	p.skipWs()
	start := p.pos
	if p.pos >= len(p.src) || !isNameStart(p.src[p.pos]) {
		return "", errGQLParse
	}
	p.pos++
	for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos], nil
}

func (p *gqlParser) readString() (string, error) {
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", errGQLParse
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				return "", errGQLParse
			}
			e := p.src[p.pos]
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(e)
			}
			p.pos++
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", errGQLParse
}

func (p *gqlParser) readNumber() (float64, error) {
	p.skipWs()
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= '0' && c <= '9' || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.pos++
		} else {
			break
		}
	}
	if p.pos == start {
		return 0, errGQLParse
	}
	n, err := strconv.ParseFloat(p.src[start:p.pos], 64)
	if err != nil {
		return 0, errGQLParse
	}
	return n, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	p.skipWs()
	if p.pos >= len(p.src) {
		return nil, errGQLParse
	}
	c := p.src[p.pos]
	switch {
	case c == '"':
		s, err := p.readString()
		return s, err
	case c == '$':
		p.pos++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		return map[string]any{"$var": name}, nil
	case c == '[':
		p.pos++
		var items []gqlValue
		for p.peek() != ']' {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		if err := p.expect(']'); err != nil {
			return nil, err
		}
		return items, nil
	case c == '{':
		p.pos++
		obj := map[string]gqlValue{}
		for p.peek() != '}' {
			key, err := p.readName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[key] = v
		}
		if err := p.expect('}'); err != nil {
			return nil, err
		}
		return obj, nil
	case c == '-' || c >= '0' && c <= '9':
		return p.readNumber()
	default:
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return map[string]any{"$enum": name}, nil
	}
}

func (p *gqlParser) parseArgs() (map[string]gqlValue, error) {
	args := map[string]gqlValue{}
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return args, nil
	}
	p.pos++
	for p.peek() != ')' {
		key, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[key] = v
	}
	if err := p.expect(')'); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseType() error {
	p.skipWs()
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		p.pos++
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expect(']'); err != nil {
			return err
		}
	} else {
		if _, err := p.readName(); err != nil {
			return err
		}
	}
	p.skipWs()
	if p.pos < len(p.src) && p.src[p.pos] == '!' {
		p.pos++
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*selection, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var out []*selection
	for {
		p.skipWs()
		if p.pos < len(p.src) && p.src[p.pos] == '}' {
			p.pos++
			return out, nil
		}
		if strings.HasPrefix(p.src[p.pos:], "...") {
			p.pos += 3
			p.skipWs()
			var on string
			hasOn := false
			if strings.HasPrefix(p.src[p.pos:], "on") && p.pos+2 < len(p.src) &&
				(p.src[p.pos+2] == ' ' || p.src[p.pos+2] == '\t' || p.src[p.pos+2] == '\n' || p.src[p.pos+2] == '\r') {
				p.pos += 2
				name, err := p.readName()
				if err != nil {
					return nil, err
				}
				on, hasOn = name, true
			} else {
				if _, err := p.readName(); err != nil {
					return nil, err
				}
			}
			children, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			for _, child := range children {
				cp := *child
				if hasOn {
					cp.fragmentOn = on
					cp.hasFrag = true
				}
				out = append(out, &cp)
			}
			continue
		}
		first, err := p.readName()
		if err != nil {
			return nil, err
		}
		alias, name := first, first
		p.skipWs()
		if p.pos < len(p.src) && p.src[p.pos] == ':' {
			p.pos++
			name, err = p.readName()
			if err != nil {
				return nil, err
			}
		}
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		var children []*selection
		p.skipWs()
		if p.pos < len(p.src) && p.src[p.pos] == '{' {
			children, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		out = append(out, &selection{alias: alias, name: name, args: args, children: children})
	}
}

func (p *gqlParser) parseOperation() (*operation, error) {
	p.skipWs()
	kind := "query"
	if strings.HasPrefix(p.src[p.pos:], "query") {
		next := byte(0)
		if p.pos+5 < len(p.src) {
			next = p.src[p.pos+5]
		}
		if !isNameChar(next) {
			p.pos += 5
			kind = "query"
		}
	} else if strings.HasPrefix(p.src[p.pos:], "mutation") {
		next := byte(0)
		if p.pos+8 < len(p.src) {
			next = p.src[p.pos+8]
		}
		if !isNameChar(next) {
			p.pos += 8
			kind = "mutation"
		}
	}
	p.skipWs()
	var name string
	if p.pos < len(p.src) && isNameStart(p.src[p.pos]) {
		n, err := p.readName()
		if err != nil {
			return nil, err
		}
		name = n
	}
	p.skipWs()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		p.pos++
		depth := 1
		for p.pos < len(p.src) && depth > 0 {
			c := p.src[p.pos]
			if c == '(' || c == '[' || c == '{' {
				depth++
			} else if c == ')' || c == ']' || c == '}' {
				depth--
			}
			p.pos++
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &operation{kind: kind, name: name, selections: sels}, nil
}

func parseDocument(query string) ([]*operation, error) {
	p := &gqlParser{src: query}
	var ops []*operation
	for p.peek() != 0 {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

var errGQLParse = &gqlError{"parse error"}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

// substitute resolves $var / $enum markers against variables.
func substitute(v gqlValue, vars map[string]any) any {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["$var"].(string); ok {
			if val, ok := vars[name]; ok {
				return val
			}
			return nil
		}
		if name, ok := t["$enum"].(string); ok {
			return name
		}
		out := map[string]any{}
		for k, x := range t {
			out[k] = substitute(x, vars)
		}
		return out
	case map[string]gqlValue:
		out := map[string]any{}
		for k, x := range t {
			out[k] = substitute(x, vars)
		}
		return out
	case []gqlValue:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = substitute(x, vars)
		}
		return out
	default:
		return v
	}
}


// ---- executor ---------------------------------------------------------------

type viewKind int

const (
	vQuery viewKind = iota
	vUser
	vRepository
	vIssue
	vPullRequest
	vIssueConnection
	vPullRequestConnection
	vChangedFileConnection
	vChangedFile
	vRef
	vPageInfo
	vType
	vPayload
)

type view struct {
	kind   viewKind
	login  string
	repo   *Repo
	issue  *Issue
	pull   *Pull
	issues []*Issue
	pulls  []*Pull
	total  int
	hasNext bool
	endCursor string
	path   string
	refName string
	typeName string
	fields map[string]any
}

var issueTypeFields = []string{"id", "number", "title", "body", "state", "url", "author", "repository", "comments", "createdAt", "updatedAt"}
var prTypeFields = []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "headRefOid", "baseRefOid", "repository", "createdAt", "updatedAt"}
var repoTypeFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "createdAt", "updatedAt"}
var searchEnum = []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "ORGANIZATION"}

func viewTypeName(v view) string {
	switch v.kind {
	case vQuery:
		return "Query"
	case vUser:
		return "User"
	case vRepository:
		return "Repository"
	case vIssue:
		return "Issue"
	case vPullRequest:
		return "PullRequest"
	case vIssueConnection:
		return "IssueConnection"
	case vPullRequestConnection:
		return "PullRequestConnection"
	case vChangedFileConnection:
		return "ChangedFileConnection"
	case vChangedFile:
		return "ChangedFile"
	case vRef:
		return "Ref"
	case vPageInfo:
		return "PageInfo"
	case vType:
		return "__Type"
	default:
		return "Object"
	}
}

func cursorFor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + itoa(number)))
}

func cursorNumber(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func gqlState(state string) string {
	if state == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func prState(pull *Pull) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

// GlobalId helpers (base64url of "kind:part:part...").
func globalID(kind string, parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + strings.Join(parts, ":")))
}

func decodeGlobalID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	bits := strings.Split(string(raw), ":")
	if len(bits) < 2 {
		return "", nil, false
	}
	return bits[0], bits[1:], true
}

type executor struct {
	store  *Store
	errors []map[string]any
}

func (e *executor) arg(sel *selection, vars map[string]any, name string) (any, bool) {
	v, ok := sel.args[name]
	if !ok {
		return nil, false
	}
	return substitute(v, vars), true
}

func (e *executor) strArg(sel *selection, vars map[string]any, name string) (string, bool) {
	v, ok := e.arg(sel, vars, name)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (e *executor) intArg(sel *selection, vars map[string]any, name string) (int, bool) {
	v, ok := e.arg(sel, vars, name)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func (e *executor) execute(v view, sels []*selection, vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, sel := range sels {
		if sel.hasFrag && sel.fragmentOn != viewTypeName(v) &&
			!(v.kind == vRepository && sel.fragmentOn == "Node") {
			continue
		}
		value, present := e.resolve(v, sel, vars)
		if !present {
			continue
		}
		out[sel.alias] = value
	}
	return out
}

// select resolves a view with children, or its scalar when there are none.
func (e *executor) selectView(v view, sels []*selection, vars map[string]any) any {
	if len(sels) == 0 {
		switch v.kind {
		case vUser:
			return v.login
		case vRef:
			return v.refName
		}
		return nil
	}
	return e.execute(v, sels, vars)
}

func (e *executor) resolve(v view, sel *selection, vars map[string]any) (any, bool) {
	name := sel.name
	if name == "__typename" {
		return viewTypeName(v), true
	}
	switch v.kind {
	case vQuery:
		return e.resolveQuery(sel, vars)
	case vUser:
		switch name {
		case "login":
			return v.login, true
		case "id":
			return globalID("user", v.login), true
		case "name":
			if v.login == ViewerLogin {
				return "ada iversen", true
			}
			return v.login, true
		case "email":
			return v.login + "@example.invalid", true
		}
	case vRepository:
		return e.resolveRepo(v.repo, sel, vars)
	case vIssue:
		return e.resolveIssue(v.repo, v.issue, sel, vars)
	case vPullRequest:
		return e.resolvePull(v.repo, v.pull, sel, vars)
	case vIssueConnection:
		switch name {
		case "nodes":
			nodes := make([]any, len(v.issues))
			for i, iss := range v.issues {
				nodes[i] = e.selectView(view{kind: vIssue, repo: v.repo, issue: iss}, sel.children, vars)
			}
			return nodes, true
		case "totalCount":
			return v.total, true
		case "pageInfo":
			return e.selectView(view{kind: vPageInfo, hasNext: v.hasNext, endCursor: v.endCursor}, sel.children, vars), true
		}
	case vPullRequestConnection:
		switch name {
		case "nodes":
			nodes := make([]any, len(v.pulls))
			for i, p := range v.pulls {
				nodes[i] = e.selectView(view{kind: vPullRequest, repo: v.repo, pull: p}, sel.children, vars)
			}
			return nodes, true
		case "totalCount":
			return len(v.pulls), true
		case "pageInfo":
			return e.selectView(view{kind: vPageInfo, hasNext: false, endCursor: ""}, sel.children, vars), true
		}
	case vChangedFileConnection:
		switch name {
		case "nodes":
			nodes := make([]any, len(v.pull.Files))
			for i, f := range v.pull.Files {
				nodes[i] = e.selectView(view{kind: vChangedFile, path: f.Filename}, sel.children, vars)
			}
			return nodes, true
		case "totalCount":
			return len(v.pull.Files), true
		}
	case vChangedFile:
		if name == "path" {
			return v.path, true
		}
	case vRef:
		if name == "name" {
			return v.refName, true
		}
	case vPageInfo:
		switch name {
		case "hasNextPage":
			return v.hasNext, true
		case "hasPreviousPage":
			return false, true
		case "endCursor":
			if v.endCursor == "" {
				return nil, true
			}
			return v.endCursor, true
		case "startCursor":
			return nil, true
		}
	case vType:
		switch name {
		case "name":
			return v.typeName, true
		case "kind":
			if v.typeName == "SearchType" {
				return "ENUM", true
			}
			return "OBJECT", true
		case "fields":
			var fields []string
			switch v.typeName {
			case "Issue":
				fields = issueTypeFields
			case "PullRequest":
				fields = prTypeFields
			case "Repository":
				fields = repoTypeFields
			}
			out := make([]any, len(fields))
			for i, f := range fields {
				out[i] = map[string]any{"name": f}
			}
			return out, true
		case "enumValues":
			if v.typeName != "SearchType" {
				return []any{}, true
			}
			out := make([]any, len(searchEnum))
			for i, x := range searchEnum {
				out[i] = map[string]any{"name": x}
			}
			return out, true
		case "description":
			return nil, true
		}
	case vPayload:
		if val, ok := v.fields[name]; ok {
			if m, isMap := val.(map[string]any); isMap {
				if vw, hasView := m["__view"]; hasView {
					return e.selectView(vw.(view), sel.children, vars), true
				}
			}
			if len(sel.children) > 0 {
				if m, isMap := val.(map[string]any); isMap {
					return e.executePlain(m, sel.children, vars), true
				}
			}
			return val, true
		}
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + name + "\" on type \"" + viewTypeName(v) + "\"."})
	return nil, false
}

func (e *executor) executePlain(obj map[string]any, sels []*selection, vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, sel := range sels {
		v, ok := obj[sel.name]
		if !ok {
			e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + sel.name + "\" on type \"Object\"."})
			continue
		}
		if m, isMap := v.(map[string]any); isMap {
			if vw, hasView := m["__view"]; hasView {
				out[sel.alias] = e.selectView(vw.(view), sel.children, vars)
				continue
			}
		}
		if len(sel.children) > 0 {
			if m, isMap := v.(map[string]any); isMap {
				out[sel.alias] = e.executePlain(m, sel.children, vars)
				continue
			}
		}
		out[sel.alias] = v
	}
	return out
}


func (e *executor) resolveQuery(sel *selection, vars map[string]any) (any, bool) {
	name := sel.name
	switch name {
	case "viewer":
		return e.selectView(view{kind: vUser, login: ViewerLogin}, sel.children, vars), true
	case "repository":
		owner, ok1 := e.strArg(sel, vars, "owner")
		repoName, ok2 := e.strArg(sel, vars, "name")
		if !ok1 || !ok2 {
			return nil, true
		}
		repo := e.store.GetRepo(owner, repoName)
		if repo == nil {
			return nil, true
		}
		return e.selectView(view{kind: vRepository, repo: repo}, sel.children, vars), true
	case "node":
		id, ok := e.strArg(sel, vars, "id")
		if !ok {
			return nil, true
		}
		kind, parts, good := decodeGlobalID(id)
		if !good {
			return nil, true
		}
		switch kind {
		case "repo":
			if len(parts) < 2 {
				return nil, true
			}
			repo := e.store.GetRepo(parts[0], parts[1])
			if repo == nil {
				return nil, true
			}
			return e.selectView(view{kind: vRepository, repo: repo}, sel.children, vars), true
		case "issue":
			if len(parts) < 3 {
				return nil, true
			}
			repo := e.store.GetRepo(parts[0], parts[1])
			num, err := strconv.Atoi(parts[2])
			if err != nil || repo == nil {
				return nil, true
			}
			var issue *Issue
			for _, i := range repo.Issues {
				if i.Number == num {
					issue = i
					break
				}
			}
			if issue == nil {
				return nil, true
			}
			return e.selectView(view{kind: vIssue, repo: repo, issue: issue}, sel.children, vars), true
		case "pull":
			if len(parts) < 3 {
				return nil, true
			}
			repo := e.store.GetRepo(parts[0], parts[1])
			num, err := strconv.Atoi(parts[2])
			if err != nil || repo == nil {
				return nil, true
			}
			var pull *Pull
			for _, p := range repo.Pulls {
				if p.Number == num {
					pull = p
					break
				}
			}
			if pull == nil {
				return nil, true
			}
			return e.selectView(view{kind: vPullRequest, repo: repo, pull: pull}, sel.children, vars), true
		}
		return nil, true
	case "__type":
		typeName, ok := e.strArg(sel, vars, "name")
		if !ok {
			return nil, true
		}
		switch typeName {
		case "Issue", "PullRequest", "Repository", "SearchType", "User", "Query", "Mutation":
			return e.selectView(view{kind: vType, typeName: typeName}, sel.children, vars), true
		}
		return nil, true
	case "__schema":
		return map[string]any{"queryType": map[string]any{"name": "Query"}, "mutationType": map[string]any{"name": "Mutation"}}, true
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + name + "\" on type \"Query\"."})
	return nil, false
}

func (e *executor) resolveRepo(repo *Repo, sel *selection, vars map[string]any) (any, bool) {
	name := sel.name
	switch name {
	case "id":
		return globalID("repo", repo.Owner, repo.Name), true
	case "databaseId":
		return repo.ID, true
	case "name":
		return repo.Name, true
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name, true
	case "owner":
		return e.selectView(view{kind: vUser, login: repo.Owner}, sel.children, vars), true
	case "defaultBranchRef":
		tip, ok := e.store.BranchTip(repo, repo.DefaultBranch)
		if !ok {
			return nil, true
		}
		_ = tip
		return e.selectView(view{kind: vRef, refName: repo.DefaultBranch}, sel.children, vars), true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "url":
		return "/" + repo.Owner + "/" + repo.Name, true
	case "createdAt", "updatedAt":
		return FixedISO, true
	case "issue":
		num, ok := e.intArg(sel, vars, "number")
		if !ok {
			return nil, true
		}
		var issue *Issue
		for _, i := range repo.Issues {
			if i.Number == num {
				issue = i
				break
			}
		}
		if issue == nil {
			return nil, true
		}
		return e.selectView(view{kind: vIssue, repo: repo, issue: issue}, sel.children, vars), true
	case "issues":
		first, hasFirst := e.intArg(sel, vars, "first")
		after, _ := e.arg(sel, vars, "after")
		states, _ := e.arg(sel, vars, "states")
		orderBy, _ := e.arg(sel, vars, "orderBy")
		list := append([]*Issue(nil), repo.Issues...)
		if arr, ok := states.([]any); ok {
			wanted := map[string]bool{}
			for _, x := range arr {
				if s, ok := x.(string); ok {
					wanted[s] = true
				}
			}
			filtered := list[:0]
			for _, i := range list {
				if wanted[gqlState(i.State)] {
					filtered = append(filtered, i)
				}
			}
			list = filtered
		}
		dir := "ASC"
		if ob, ok := orderBy.(map[string]any); ok {
			if ob["direction"] == "DESC" {
				dir = "DESC"
			} else if ob["direction"] == "ASC" {
				dir = "ASC"
			}
		}
		if dir == "DESC" {
			sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
		} else {
			sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
		}
		if afterStr, ok := after.(string); ok {
			if afterNum, good := cursorNumber(afterStr); good {
				filtered := list[:0]
				for _, i := range list {
					if dir == "DESC" {
						if i.Number < afterNum {
							filtered = append(filtered, i)
						}
					} else if i.Number > afterNum {
						filtered = append(filtered, i)
					}
				}
				list = filtered
			}
		}
		total := len(list)
		limited := list
		if hasFirst && len(list) > first {
			limited = list[:first]
		}
		hasNext := false
		if hasFirst {
			hasNext = total > len(limited)
		}
		endCursor := ""
		if len(limited) > 0 {
			endCursor = cursorFor(limited[len(limited)-1].Number)
		}
		return e.selectView(view{kind: vIssueConnection, repo: repo, issues: limited, total: total, hasNext: hasNext, endCursor: endCursor}, sel.children, vars), true
	case "pullRequests":
		first, hasFirst := e.intArg(sel, vars, "first")
		list := append([]*Pull(nil), repo.Pulls...)
		sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
		if hasFirst && len(list) > first {
			list = list[:first]
		}
		return e.selectView(view{kind: vPullRequestConnection, repo: repo, pulls: list}, sel.children, vars), true
	case "pullRequest":
		num, ok := e.intArg(sel, vars, "number")
		if !ok {
			return nil, true
		}
		var pull *Pull
		for _, p := range repo.Pulls {
			if p.Number == num {
				pull = p
				break
			}
		}
		if pull == nil {
			return nil, true
		}
		return e.selectView(view{kind: vPullRequest, repo: repo, pull: pull}, sel.children, vars), true
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + name + "\" on type \"Repository\"."})
	return nil, false
}

func (e *executor) resolveIssue(repo *Repo, issue *Issue, sel *selection, vars map[string]any) (any, bool) {
	name := sel.name
	switch name {
	case "id":
		return globalID("issue", repo.Owner, repo.Name, itoa(issue.Number)), true
	case "number":
		return issue.Number, true
	case "title":
		return issue.Title, true
	case "body":
		return issue.Body, true
	case "state":
		return gqlState(issue.State), true
	case "url":
		return "/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number), true
	case "author":
		return e.selectView(view{kind: vUser, login: ViewerLogin}, sel.children, vars), true
	case "repository":
		return e.selectView(view{kind: vRepository, repo: repo}, sel.children, vars), true
	case "createdAt", "updatedAt":
		return FixedISO, true
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + name + "\" on type \"Issue\"."})
	return nil, false
}

func (e *executor) resolvePull(repo *Repo, pull *Pull, sel *selection, vars map[string]any) (any, bool) {
	name := sel.name
	switch name {
	case "id":
		return globalID("pull", repo.Owner, repo.Name, itoa(pull.Number)), true
	case "number":
		return pull.Number, true
	case "title":
		return pull.Title, true
	case "body":
		return pull.Body, true
	case "state":
		return prState(pull), true
	case "merged":
		return pull.Merged, true
	case "mergeable":
		return e.store.PullMergeable(repo, pull), true
	case "headRefName":
		return pull.Head, true
	case "baseRefName":
		return pull.Base, true
	case "headRefOid":
		return pull.HeadSha, true
	case "baseRefOid":
		return pull.BaseSha, true
	case "files":
		return e.selectView(view{kind: vChangedFileConnection, repo: repo, pull: pull}, sel.children, vars), true
	case "repository":
		return e.selectView(view{kind: vRepository, repo: repo}, sel.children, vars), true
	case "createdAt", "updatedAt":
		return FixedISO, true
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + name + "\" on type \"PullRequest\"."})
	return nil, false
}

func (e *executor) executeOperation(op *operation, vars map[string]any) (map[string]any, []map[string]any) {
	if op.kind == "mutation" {
		return e.executeMutation(op, vars)
	}
	data := e.execute(view{kind: vQuery}, op.selections, vars)
	return data, e.errors
}

func (e *executor) executeMutation(op *operation, vars map[string]any) (map[string]any, []map[string]any) {
	data := map[string]any{}
	for _, sel := range op.selections {
		input, _ := e.arg(sel, vars, "input")
		inputObj, _ := input.(map[string]any)
		if inputObj == nil {
			inputObj = map[string]any{}
		}
		strInput := func(k string) string {
			if s, ok := inputObj[k].(string); ok {
				return s
			}
			return ""
		}
		switch sel.name {
		case "createIssue":
			kind, parts, good := decodeGlobalID(strInput("repositoryId"))
			var issueView *view
			if good && kind == "repo" && len(parts) >= 2 {
				if repo := e.store.GetRepo(parts[0], parts[1]); repo != nil {
					created, errMsg := e.store.StoreCreateIssue(repo, strInput("title"), strInput("body"), nil)
					if errMsg == "" {
						v := view{kind: vIssue, repo: repo, issue: created}
						issueView = &v
					}
				}
			}
			if issueView == nil {
				e.errors = append(e.errors, map[string]any{"message": "createIssue: repository not found"})
				data[sel.alias] = nil
				continue
			}
			data[sel.alias] = e.executePlain(map[string]any{"issue": map[string]any{"__view": *issueView}}, sel.children, vars)
		case "closeIssue":
			kind, parts, good := decodeGlobalID(strInput("issueId"))
			var issueView *view
			if good && kind == "issue" && len(parts) >= 3 {
				if repo := e.store.GetRepo(parts[0], parts[1]); repo != nil {
					if num, err := strconv.Atoi(parts[2]); err == nil {
						for _, iss := range repo.Issues {
							if iss.Number == num {
								iss.State = "closed"
								v := view{kind: vIssue, repo: repo, issue: iss}
								issueView = &v
								break
							}
						}
					}
				}
			}
			if issueView == nil {
				e.errors = append(e.errors, map[string]any{"message": "closeIssue: issue not found"})
				data[sel.alias] = nil
				continue
			}
			data[sel.alias] = e.executePlain(map[string]any{"issue": map[string]any{"__view": *issueView}}, sel.children, vars)
		case "createPullRequest":
			kind, parts, good := decodeGlobalID(strInput("repositoryId"))
			var pullView *view
			if good && kind == "repo" && len(parts) >= 2 {
				if repo := e.store.GetRepo(parts[0], parts[1]); repo != nil {
					created, errMsg := e.store.StoreCreatePull(repo, strInput("title"), strInput("body"), strInput("headRefName"), strInput("baseRefName"))
					if errMsg == "" {
						v := view{kind: vPullRequest, repo: repo, pull: created}
						pullView = &v
					}
				}
			}
			if pullView == nil {
				e.errors = append(e.errors, map[string]any{"message": "createPullRequest: repository or branch not found"})
				data[sel.alias] = nil
				continue
			}
			data[sel.alias] = e.executePlain(map[string]any{"pullRequest": map[string]any{"__view": *pullView}}, sel.children, vars)
		case "mergePullRequest":
			kind, parts, good := decodeGlobalID(strInput("pullRequestId"))
			var pullView *view
			if good && kind == "pull" && len(parts) >= 3 {
				if repo := e.store.GetRepo(parts[0], parts[1]); repo != nil {
					if num, err := strconv.Atoi(parts[2]); err == nil {
						for _, p := range repo.Pulls {
							if p.Number == num {
								e.store.StoreMergePull(repo, p)
								v := view{kind: vPullRequest, repo: repo, pull: p}
								pullView = &v
								break
							}
						}
					}
				}
			}
			if pullView == nil {
				e.errors = append(e.errors, map[string]any{"message": "mergePullRequest: pull request not found"})
				data[sel.alias] = nil
				continue
			}
			data[sel.alias] = e.executePlain(map[string]any{"pullRequest": map[string]any{"__view": *pullView}}, sel.children, vars)
		case "addComment":
			kind, parts, good := decodeGlobalID(strInput("subjectId"))
			var node map[string]any
			if good && kind == "issue" && len(parts) >= 3 {
				if repo := e.store.GetRepo(parts[0], parts[1]); repo != nil {
					if num, err := strconv.Atoi(parts[2]); err == nil {
						for _, iss := range repo.Issues {
							if iss.Number == num {
								comment := &IssueComment{ID: e.store.NextCommentID(), Body: strInput("body"), Seq: e.store.NextSeq()}
								iss.Comments = append(iss.Comments, comment)
								node = map[string]any{
									"id":   globalID("comment", repo.Owner, repo.Name, itoa(iss.Number), itoa(comment.ID)),
									"body": comment.Body,
								}
								break
							}
						}
					}
				}
			}
			if node == nil {
				e.errors = append(e.errors, map[string]any{"message": "addComment: subject not found"})
				data[sel.alias] = nil
				continue
			}
			data[sel.alias] = e.executePlain(map[string]any{"commentEdge": map[string]any{"node": node}}, sel.children, vars)
		default:
			e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + sel.name + "\" on type \"Mutation\"."})
			data[sel.alias] = nil
		}
	}
	return data, e.errors
}

// HandleGraphqlOperation runs one GraphQL request body (or an array of them).
func HandleGraphqlOperation(store *Store, body any) (int, any) {
	runOne := func(opBody any) map[string]any {
		rec, ok := opBody.(map[string]any)
		if !ok {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": "a GraphQL operation needs a query string"}}}
		}
		query, ok := rec["query"].(string)
		if !ok {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": "a GraphQL operation needs a query string"}}}
		}
		vars := map[string]any{}
		if v, ok := rec["variables"].(map[string]any); ok {
			vars = v
		}
		ops, err := parseDocument(query)
		if err != nil {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": err.Error()}}}
		}
		if len(ops) == 0 {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": "empty query"}}}
		}
		op := ops[0]
		if name, ok := rec["operationName"].(string); ok {
			for _, o := range ops {
				if o.name == name {
					op = o
					break
				}
			}
		}
		ex := &executor{store: store}
		data, errs := ex.executeOperation(op, vars)
		out := map[string]any{"data": data}
		if len(errs) > 0 {
			out["errors"] = errs
		}
		return out
	}
	if arr, ok := body.([]any); ok {
		results := make([]map[string]any, len(arr))
		for i, item := range arr {
			results[i] = runOne(item)
		}
		return 200, results
	}
	return 200, runOne(body)
}
