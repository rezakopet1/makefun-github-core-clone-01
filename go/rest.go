// REST v3 under /api/v3: repos, branches, commits, contents, labels, issues,
// pull requests. All bodies are JSON; every reply carries X-GitHub-Request-Id
// (added by http.go) and application/json.
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// restResult is one REST answer: status, a body to marshal (with a trailing
// newline, as the Node server sent it) or pre-serialized bytes, plus headers.
type restResult struct {
	status    int
	body      any
	bodyBytes []byte
	headers   map[string]string
}

func notFoundResult(msg string) restResult {
	if msg == "" {
		msg = "Not Found"
	}
	return restResult{status: 404, body: map[string]any{"message": msg}}
}

func badRequestResult(msg string) restResult {
	return restResult{status: 400, body: map[string]any{"message": msg}}
}

// isoOfEpoch renders a Unix epoch as an ISO-8601 UTC string with millis.
func isoOfEpoch(epochSec int64) string {
	return time.Unix(epochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// ---- JSON shapes (field order mirrors the Node bodies) --------------------

type userObj struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Type      string `json:"type"`
	AvatarURL string `json:"avatar_url"`
}

func userJSON() userObj {
	return userObj{Login: ViewerLogin, ID: ViewerID, Name: ViewerName, Email: ViewerEmail, Type: "User", AvatarURL: ""}
}

type ownerObj struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoObj struct {
	ID            int      `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         ownerObj `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	HTMLURL       string   `json:"html_url"`
	URL           string   `json:"url"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	PushedAt      string   `json:"pushed_at"`
	HasIssues     bool     `json:"has_issues"`
	HasProjects   bool     `json:"has_projects"`
	HasWiki       bool     `json:"has_wiki"`
	Fork          bool     `json:"fork"`
	Archived      bool     `json:"archived"`
	Disabled      bool     `json:"disabled"`
	Visibility    string   `json:"visibility"`
}

func repoJSON(r *Repo) repoObj {
	vis := "public"
	if r.Private {
		vis = "private"
	}
	return repoObj{
		ID: r.ID, NodeID: "R_" + itoa(r.ID), Name: r.Name,
		FullName: r.Owner + "/" + r.Name,
		Owner:    ownerObj{Login: r.Owner, ID: ViewerID, Type: "User"},
		Private:  r.Private, Description: r.Description,
		DefaultBranch: r.DefaultBranch,
		HTMLURL:       "/" + r.Owner + "/" + r.Name,
		URL:           "/api/v3/repos/" + r.Owner + "/" + r.Name,
		CreatedAt: FixedISO, UpdatedAt: FixedISO, PushedAt: FixedISO,
		HasIssues: true, HasProjects: false, HasWiki: false,
		Fork: false, Archived: false, Disabled: false, Visibility: vis,
	}
}

type issueLabelObj struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issueObj struct {
	ID        int             `json:"id"`
	NodeID    string          `json:"node_id"`
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	State     string          `json:"state"`
	Labels    []issueLabelObj `json:"labels"`
	User      userObj         `json:"user"`
	Assignee  any             `json:"assignee"`
	Assignees []any           `json:"assignees"`
	Locked    bool            `json:"locked"`
	Comments  int             `json:"comments"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	ClosedAt  *string         `json:"closed_at"`
	HTMLURL   string          `json:"html_url"`
	URL       string          `json:"url"`
}

func issueJSON(r *Repo, issue *Issue) issueObj {
	labels := make([]issueLabelObj, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := r.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, issueLabelObj{Name: name, Color: color, Description: ""})
	}
	var closedAt *string
	if issue.State == "closed" {
		s := FixedISO
		closedAt = &s
	}
	return issueObj{
		ID: issue.ID, NodeID: "I_" + itoa(issue.ID), Number: issue.Number,
		Title: issue.Title, Body: issue.Body, State: issue.State,
		Labels: labels, User: userJSON(), Assignee: nil, Assignees: []any{},
		Locked: false, Comments: len(issue.Comments),
		CreatedAt: FixedISO, UpdatedAt: FixedISO, ClosedAt: closedAt,
		HTMLURL: "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + itoa(issue.Number),
		URL:     "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + itoa(issue.Number),
	}
}

type pullRefObj struct {
	Ref   string  `json:"ref"`
	Sha   string  `json:"sha"`
	Label string  `json:"label"`
	User  userObj `json:"user"`
	Repo  repoObj `json:"repo"`
}

type pullObj struct {
	ID             int         `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	Mergeable      bool        `json:"mergeable"`
	Head           pullRefObj  `json:"head"`
	Base           pullRefObj  `json:"base"`
	User           userObj     `json:"user"`
	Assignee       any         `json:"assignee"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	ClosedAt       *string     `json:"closed_at"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
}

func pullJSON(s *Store, r *Repo, pull *Pull) pullObj {
	var mergedAt, mergeSha, closedAt *string
	if pull.MergedAt != "" {
		v := pull.MergedAt
		mergedAt = &v
	}
	if pull.MergeCommitSha != "" {
		v := pull.MergeCommitSha
		mergeSha = &v
	}
	if pull.State == "closed" {
		v := FixedISO
		closedAt = &v
	}
	return pullObj{
		ID: pull.ID, NodeID: "PR_" + itoa(pull.ID), Number: pull.Number,
		Title: pull.Title, Body: pull.Body, State: pull.State,
		Merged: pull.Merged, MergedAt: mergedAt, MergeCommitSha: mergeSha,
		Mergeable: s.PullMergeable(r, pull) == "MERGEABLE",
		Head: pullRefObj{Ref: pull.Head, Sha: pull.HeadSha, Label: r.Owner + ":" + pull.Head, User: userJSON(), Repo: repoJSON(r)},
		Base: pullRefObj{Ref: pull.Base, Sha: pull.BaseSha, Label: r.Owner + ":" + pull.Base, User: userJSON(), Repo: repoJSON(r)},
		User: userJSON(), Assignee: nil,
		CreatedAt: FixedISO, UpdatedAt: FixedISO, ClosedAt: closedAt,
		HTMLURL: "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + itoa(pull.Number),
		URL:     "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + itoa(pull.Number),
	}
}

type commitAuthorObj struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitInnerObj struct {
	Message   string          `json:"message"`
	Author    commitAuthorObj `json:"author"`
	Committer commitAuthorObj `json:"committer"`
}

type commitParentObj struct {
	Sha string `json:"sha"`
}

type commitObj struct {
	Sha       string          `json:"sha"`
	NodeID    string          `json:"node_id"`
	Commit    commitInnerObj  `json:"commit"`
	Author    ownerObj        `json:"author"`
	Committer ownerObj        `json:"committer"`
	Parents   []commitParentObj `json:"parents"`
	URL       string          `json:"url"`
}

func parseAuthor(line string) commitAuthorObj {
	// "Name <email> epoch +0000"
	lt := strings.Index(line, "<")
	gt := strings.Index(line, ">")
	if lt < 0 || gt < 0 || gt < lt {
		return commitAuthorObj{Name: ViewerName, Email: ViewerEmail, Date: FixedISO}
	}
	name := strings.TrimSpace(line[:lt])
	email := line[lt+1 : gt]
	rest := strings.Fields(strings.TrimSpace(line[gt+1:]))
	if len(rest) < 1 {
		return commitAuthorObj{Name: ViewerName, Email: ViewerEmail, Date: FixedISO}
	}
	epoch, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return commitAuthorObj{Name: ViewerName, Email: ViewerEmail, Date: FixedISO}
	}
	return commitAuthorObj{Name: name, Email: email, Date: isoOfEpoch(epoch)}
}

func commitJSON(s *Store, r *Repo, sha string) *commitObj {
	obj, ok := r.Objects[sha]
	if !ok || obj.Type != ObjCommit {
		return nil
	}
	text := string(obj.Data)
	blank := strings.Index(text, "\n\n")
	head, message := text, ""
	if blank >= 0 {
		head, message = text[:blank], text[blank+2:]
	}
	var authorLine, committerLine string
	var parents []string
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "author "):
			authorLine = line[7:]
		case strings.HasPrefix(line, "committer "):
			committerLine = line[10:]
		case strings.HasPrefix(line, "parent "):
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
	}
	author := parseAuthor(authorLine)
	committer := parseAuthor(committerLine)
	cleanMessage := strings.TrimRight(message, "\n")
	parentObjs := make([]commitParentObj, 0, len(parents))
	for _, p := range parents {
		parentObjs = append(parentObjs, commitParentObj{Sha: p})
	}
	return &commitObj{
		Sha: sha, NodeID: "C_" + sha,
		Commit: commitInnerObj{Message: cleanMessage, Author: author, Committer: committer},
		Author: ownerObj{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		Committer: ownerObj{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		Parents: parentObjs,
		URL:     "/api/v3/repos/" + r.Owner + "/" + r.Name + "/commits/" + sha,
	}
}

// commitList walks a tip breadth-first, as the Node version did.
func commitList(s *Store, r *Repo, tip string) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		out = append(out, oid)
		queue = append(queue, ParseCommit(obj.Data).Parents...)
	}
	return out
}

// paginate slices items per ?per_page/&page with the Node clamps.
func paginate[T any](items []T, query url.Values) []T {
	perPage := numOr(query.Get("per_page"), 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := numOr(query.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// numOr parses a query number the way Number(x) || fallback does: unparsable
// or zero falls back.
func numOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || f == 0 {
		return fallback
	}
	return int(f)
}

// linkHeader builds a GitHub-style Link header for multi-page listings.
func linkHeader(origin, path string, query url.Values, page, perPage, total int) string {
	pages := (total + perPage - 1) / perPage
	if pages <= 1 {
		return ""
	}
	mk := func(p int, rel string) string {
		q := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("page", itoa(p))
		q.Set("per_page", itoa(perPage))
		return "<" + origin + path + "?" + q.Encode() + ">; rel=\"" + rel + "\""
	}
	var links []string
	if page < pages {
		links = append(links, mk(page+1, "next"), mk(pages, "last"))
	}
	if page > 1 {
		links = append(links, mk(1, "first"), mk(page-1, "prev"))
	}
	return strings.Join(links, ", ")
}

func bodyStr(body map[string]any, key string) string {
	if body == nil {
		return ""
	}
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}

func bodyBool(body map[string]any, key string) bool {
	if body == nil {
		return false
	}
	if v, ok := body[key].(bool); ok {
		return v
	}
	return false
}

func bodyStrs(body map[string]any, key string) []string {
	if body == nil {
		return nil
	}
	arr, ok := body[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}


// ---- routing ---------------------------------------------------------------

// HandleRest serves one REST v3 request. segs are the raw (still %-encoded)
// path parts after /api/v3; body is the parsed JSON object or nil.
func HandleRest(s *Store, method string, segs []string, query url.Values, body map[string]any, origin string) restResult {
	if len(segs) == 0 {
		if method == "GET" {
			return restResult{status: 200, body: map[string]any{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			}, headers: map[string]string{"x-oauth-scopes": "repo, read:org"}}
		}
		return notFoundResult("")
	}
	first := segs[0]
	if first == "meta" && method == "GET" {
		return restResult{status: 200, body: map[string]any{"installed_version": "1.0.0"}}
	}
	if first == "rate_limit" && method == "GET" {
		return restResult{status: 200, body: map[string]any{
			"resources": map[string]any{"core": map[string]any{"limit": 5000, "remaining": 4999, "reset": 1700000000}},
			"rate":      map[string]any{"limit": 5000, "remaining": 4999, "reset": 1700000000},
		}}
	}
	if first == "user" {
		if len(segs) == 1 && method == "GET" {
			return restResult{status: 200, body: userJSON()}
		}
		if len(segs) == 2 && segs[1] == "repos" {
			if method == "GET" {
				repos := s.ViewerRepos()
				perPage := numOr(query.Get("per_page"), 30)
				if perPage < 1 {
					perPage = 1
				}
				if perPage > 100 {
					perPage = 100
				}
				pageNum := numOr(query.Get("page"), 1)
				if pageNum < 1 {
					pageNum = 1
				}
				pageItems := paginate(repos, query)
				items := make([]repoObj, len(pageItems))
				for i, r := range pageItems {
					items[i] = repoJSON(r)
				}
				link := linkHeader(origin, "/api/v3/user/repos", query, pageNum, perPage, len(repos))
				var headers map[string]string
				if link != "" {
					headers = map[string]string{"link": link}
				}
				return restResult{status: 200, body: items, headers: headers}
			}
			if method == "POST" {
				name := bodyStr(body, "name")
				if name == "" {
					return badRequestResult("name is required")
				}
				created := s.CreateRepo(name, bodyBool(body, "private"), bodyStr(body, "description"))
				if created == nil {
					return restResult{status: 422, body: map[string]any{"message": "Repository already exists"}}
				}
				return restResult{status: 201, body: repoJSON(created)}
			}
		}
		return notFoundResult("")
	}
	if first != "repos" {
		return notFoundResult("")
	}
	if len(segs) < 3 || segs[1] == "" || segs[2] == "" {
		return notFoundResult("")
	}
	repo := s.GetRepo(segs[1], segs[2])
	if repo == nil {
		return notFoundResult("")
	}
	rest := segs[3:]
	if len(rest) == 0 {
		switch method {
		case "GET":
			return restResult{status: 200, body: repoJSON(repo)}
		case "PATCH":
			if v, ok := body["description"].(string); ok {
				repo.Description = v
			}
			if v, ok := body["private"].(bool); ok {
				repo.Private = v
			}
			if v, ok := body["default_branch"].(string); ok {
				repo.DefaultBranch = v
			}
			return restResult{status: 200, body: repoJSON(repo)}
		case "DELETE":
			s.DeleteRepo(repo)
			return restResult{status: 204, body: nil}
		}
		return notFoundResult("")
	}
	switch rest[0] {
	case "branches":
		return handleBranches(s, repo, method, rest[1:])
	case "commits":
		return handleCommits(s, repo, method, rest[1:], query)
	case "git":
		return handleGitData(s, repo, method, rest[1:], body)
	case "contents":
		return handleContents(s, repo, method, rest[1:], query, body)
	case "labels":
		return handleLabels(repo, method, body)
	case "issues":
		return handleIssues(s, repo, method, rest[1:], query, body)
	case "pulls":
		return handlePulls(s, repo, method, rest[1:], query, body)
	}
	return notFoundResult("")
}

func handleBranches(s *Store, repo *Repo, method string, rest []string) restResult {
	type branchObj struct {
		Name      string         `json:"name"`
		Commit    map[string]any `json:"commit"`
		Protected bool           `json:"protected"`
	}
	if len(rest) == 0 && method == "GET" {
		var names []string
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref[len("refs/heads/"):])
			}
		}
		sort.Strings(names)
		list := make([]branchObj, 0, len(names))
		for _, name := range names {
			sha, _ := s.BranchTip(repo, name)
			list = append(list, branchObj{Name: name, Commit: map[string]any{"sha": sha, "url": ""}, Protected: false})
		}
		return restResult{status: 200, body: list}
	}
	if len(rest) == 1 && method == "GET" {
		sha, ok := s.BranchTip(repo, rest[0])
		if !ok {
			return notFoundResult("")
		}
		return restResult{status: 200, body: branchObj{Name: rest[0], Commit: map[string]any{"sha": sha, "url": ""}, Protected: false}}
	}
	return notFoundResult("")
}

func handleCommits(s *Store, repo *Repo, method string, rest []string, query url.Values) restResult {
	if len(rest) == 0 && method == "GET" {
		refName := query.Get("sha")
		if refName == "" {
			refName = repo.DefaultBranch
		}
		tip, ok := s.BranchTip(repo, refName)
		if !ok && oidRe(refName) {
			tip, ok = refName, true
		}
		if !ok {
			return restResult{status: 200, body: []any{}}
		}
		shas := commitList(s, repo, tip)
		page := paginate(shas, query)
		items := make([]*commitObj, 0, len(page))
		for _, sha := range page {
			if c := commitJSON(s, repo, sha); c != nil {
				items = append(items, c)
			}
		}
		return restResult{status: 200, body: items}
	}
	if len(rest) == 1 && method == "GET" {
		item := commitJSON(s, repo, rest[0])
		if item == nil {
			return notFoundResult("")
		}
		return restResult{status: 200, body: item}
	}
	return notFoundResult("")
}

func handleGitData(s *Store, repo *Repo, method string, rest []string, body map[string]any) restResult {
	if len(rest) == 0 || rest[0] != "refs" {
		return notFoundResult("")
	}
	type refObj struct {
		Ref    string         `json:"ref"`
		Object map[string]any `json:"object"`
		NodeID string         `json:"node_id,omitempty"`
	}
	if method == "POST" && len(rest) == 1 {
		ref := bodyStr(body, "ref")
		sha := bodyStr(body, "sha")
		if !strings.HasPrefix(ref, "refs/") || !oidRe(sha) {
			return restResult{status: 422, body: map[string]any{"message": "invalid ref or sha"}}
		}
		if _, ok := repo.Refs[ref]; ok {
			return restResult{status: 422, body: map[string]any{"message": "Reference already exists"}}
		}
		if _, ok := repo.Objects[sha]; !ok {
			return restResult{status: 422, body: map[string]any{"message": "object does not exist"}}
		}
		repo.Refs[ref] = sha
		TouchRepo(repo)
		typ := ObjCommit
		if o, ok := repo.Objects[sha]; ok {
			typ = o.Type
		}
		return restResult{status: 201, body: refObj{
			Ref: ref, Object: map[string]any{"sha": sha, "type": string(typ)}, NodeID: "R_" + sha,
		}}
	}
	if method == "GET" && len(rest) == 1 {
		var names []string
		for ref := range repo.Refs {
			names = append(names, ref)
		}
		sort.Strings(names)
		list := make([]refObj, 0, len(names))
		for _, ref := range names {
			sha := repo.Refs[ref]
			typ := "commit"
			if o, ok := repo.Objects[sha]; ok {
				typ = string(o.Type)
			}
			list = append(list, refObj{Ref: ref, Object: map[string]any{"sha": sha, "type": typ}})
		}
		return restResult{status: 200, body: list}
	}
	if method == "GET" && len(rest) >= 3 && rest[1] == "heads" {
		branch := strings.Join(rest[2:], "/")
		sha, ok := s.BranchTip(repo, branch)
		if !ok {
			return notFoundResult("")
		}
		return restResult{status: 200, body: refObj{Ref: "refs/heads/" + branch, Object: map[string]any{"sha": sha, "type": "commit"}}}
	}
	if method == "GET" && len(rest) >= 3 && rest[1] == "tags" {
		tag := strings.Join(rest[2:], "/")
		sha, ok := repo.Refs["refs/tags/"+tag]
		if !ok {
			return notFoundResult("")
		}
		typ := "tag"
		if o, ok2 := repo.Objects[sha]; ok2 {
			typ = string(o.Type)
		}
		return restResult{status: 200, body: refObj{Ref: "refs/tags/" + tag, Object: map[string]any{"sha": sha, "type": typ}}}
	}
	return notFoundResult("")
}


// ---- contents / labels / issues / pulls -----------------------------------

func resolveRefTip(s *Store, repo *Repo, ref string, hasRef bool) (string, bool) {
	if !hasRef {
		return s.BranchTip(repo, repo.DefaultBranch)
	}
	if tip, ok := s.BranchTip(repo, ref); ok {
		return tip, true
	}
	if oidRe(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref, true
		}
	}
	return "", false
}

func handleContents(s *Store, repo *Repo, method string, rest []string, query url.Values, body map[string]any) restResult {
	decoded := make([]string, len(rest))
	for i, seg := range rest {
		if u, err := url.PathUnescape(seg); err == nil {
			decoded[i] = u
		} else {
			decoded[i] = seg
		}
	}
	path := strings.Join(decoded, "/")
	refVals, hasRef := query["ref"]
	var ref string
	if hasRef && len(refVals) > 0 {
		ref = refVals[0]
	}
	if method == "GET" {
		tip, ok := resolveRefTip(s, repo, ref, hasRef)
		if !ok {
			return notFoundResult("")
		}
		files := s.TreeMap(repo, tip)
		if path == "" {
			return restResult{status: 200, body: dirEntries(s, repo, files, "")}
		}
		if blobOID, ok := files[path]; ok {
			data := []byte{}
			if o, ok2 := repo.Objects[blobOID]; ok2 {
				data = o.Data
			}
			name := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
			return restResult{status: 200, body: map[string]any{
				"type": "file", "encoding": "base64", "size": len(data),
				"name": name, "path": path,
				"content": base64.StdEncoding.EncodeToString(data),
				"sha":       blobOID,
				"url":       "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
				"html_url": "", "download_url": "",
			}}
		}
		prefix := path + "/"
		for p := range files {
			if strings.HasPrefix(p, prefix) {
				return restResult{status: 200, body: dirEntries(s, repo, files, path)}
			}
		}
		return notFoundResult("")
	}
	if method == "PUT" {
		branch := bodyStr(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		message := bodyStr(body, "message")
		if message == "" {
			message = "Update " + path
		}
		contentB64 := bodyStr(body, "content")
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, contentB64)
		content, _ := base64.StdEncoding.DecodeString(clean)
		sha, hasSha := body["sha"].(string)
		result := s.StoreCommitFile(repo, branch, path, content, message, sha, hasSha, false)
		switch result.Status {
		case 409:
			return restResult{status: 409, body: map[string]any{"message": result.Message}}
		case 404:
			return notFoundResult("")
		}
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		return restResult{status: result.Status, body: map[string]any{
			"content": map[string]any{"name": name, "path": path, "sha": result.ContentSha, "size": len(content), "type": "file", "encoding": "base64"},
			"commit":  map[string]any{"sha": result.CommitSha, "message": message},
		}}
	}
	if method == "DELETE" {
		branch := bodyStr(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		message := bodyStr(body, "message")
		if message == "" {
			message = "Delete " + path
		}
		sha, hasSha := body["sha"].(string)
		result := s.StoreCommitFile(repo, branch, path, []byte{}, message, sha, hasSha, true)
		switch result.Status {
		case 404:
			return notFoundResult("")
		case 409:
			return restResult{status: 409, body: map[string]any{"message": result.Message}}
		}
		return restResult{status: 200, body: map[string]any{
			"commit": map[string]any{"sha": result.CommitSha, "message": message},
		}}
	}
	return notFoundResult("")
}

func dirEntries(s *Store, repo *Repo, files map[string]string, dir string) []map[string]any {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	type child struct {
		dir bool
		oid string
	}
	children := map[string]*child{}
	for path, oid := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		restPath := path[len(prefix):]
		first := restPath
		if i := strings.Index(restPath, "/"); i >= 0 {
			first = restPath[:i]
		}
		if strings.Contains(restPath, "/") {
			if _, ok := children[first]; !ok {
				children[first] = &child{dir: true}
			}
		} else {
			children[first] = &child{dir: false, oid: oid}
		}
	}
	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		c := children[name]
		childPath := name
		if dir != "" {
			childPath = dir + "/" + name
		}
		if c.dir {
			sub := map[string]string{}
			for p, o := range files {
				if strings.HasPrefix(p, childPath+"/") {
					sub[p[len(childPath)+1:]] = o
				}
			}
			treeOID := s.TreeFromMap(repo, sub)
			out = append(out, map[string]any{"type": "dir", "name": name, "path": childPath, "sha": treeOID, "size": 0, "url": "", "html_url": ""})
		} else {
			size := 0
			if o, ok := repo.Objects[c.oid]; ok {
				size = len(o.Data)
			}
			out = append(out, map[string]any{"type": "file", "name": name, "path": childPath, "sha": c.oid, "size": size, "url": "", "html_url": "", "download_url": ""})
		}
	}
	return out
}

func handleLabels(repo *Repo, method string, body map[string]any) restResult {
	if method == "POST" {
		name := bodyStr(body, "name")
		if name == "" {
			return badRequestResult("name is required")
		}
		color := bodyStr(body, "color")
		if color == "" {
			color = "ededed"
		}
		repo.Labels[name] = &Label{Name: name, Color: color}
		InvalidateAllIssueJSON(repo)
		return restResult{status: 201, body: map[string]any{"name": name, "color": color, "description": ""}}
	}
	if method == "GET" {
		list := make([]map[string]any, 0, len(repo.Labels))
		for _, l := range repo.Labels {
			list = append(list, map[string]any{"name": l.Name, "color": l.Color, "description": ""})
		}
		return restResult{status: 200, body: list}
	}
	return notFoundResult("")
}

func handleIssues(s *Store, repo *Repo, method string, rest []string, query url.Values, body map[string]any) restResult {
	if len(rest) == 0 {
		if method == "POST" {
			title := bodyStr(body, "title")
			issueBody := bodyStr(body, "body")
			labels := bodyStrs(body, "labels")
			created, errMsg := s.StoreCreateIssue(repo, title, issueBody, labels)
			if errMsg != "" {
				return restResult{status: 422, body: map[string]any{"message": errMsg}}
			}
			return restResult{status: 201, body: issueJSON(repo, created)}
		}
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			var list []*Issue
			for _, i := range repo.Issues {
				if state == "all" || i.State == state {
					list = append(list, i)
				}
			}
			sortStr := query.Get("sort")
			if sortStr == "" {
				sortStr = "created"
			}
			direction := query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			if sortStr == "created" {
				cp := append([]*Issue(nil), list...)
				if direction == "asc" {
					sort.Slice(cp, func(a, b int) bool { return cp[a].Seq < cp[b].Seq })
				} else {
					sort.Slice(cp, func(a, b int) bool { return cp[a].Seq > cp[b].Seq })
				}
				list = cp
			}
			page := paginate(list, query)
			items := make([]issueObj, len(page))
			for i, iss := range page {
				items[i] = issueJSON(repo, iss)
			}
			return restResult{status: 200, body: items}
		}
		return notFoundResult("")
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		return notFoundResult("")
	}
	var issue *Issue
	for _, i := range repo.Issues {
		if i.Number == num {
			issue = i
			break
		}
	}
	if issue == nil {
		return notFoundResult("")
	}
	if len(rest) == 1 {
		if method == "GET" {
			repo.cacheMu.Lock()
			cached, ok := repo.issueJSONCache[num]
			repo.cacheMu.Unlock()
			if ok {
				return restResult{status: 200, bodyBytes: cached}
			}
			raw, _ := json.Marshal(issueJSON(repo, issue))
			text := append(raw, '\n')
			repo.cacheMu.Lock()
			repo.issueJSONCache[num] = text
			repo.cacheMu.Unlock()
			return restResult{status: 200, bodyBytes: text}
		}
		if method == "PATCH" {
			InvalidateIssueJSON(repo, num)
			if v, ok := body["state"].(string); ok && (v == "open" || v == "closed") {
				issue.State = v
			}
			if v, ok := body["title"].(string); ok {
				issue.Title = v
			}
			if v, ok := body["body"].(string); ok {
				issue.Body = v
			}
			if arr, ok := body["labels"].([]any); ok {
				var labels []string
				for _, x := range arr {
					if str, ok := x.(string); ok {
						labels = append(labels, str)
					}
				}
				for _, l := range labels {
					if _, ok := repo.Labels[l]; !ok {
						return restResult{status: 422, body: map[string]any{"message": "label " + l + " does not exist"}}
					}
				}
				issue.Labels = labels
			}
			return restResult{status: 200, body: issueJSON(repo, issue)}
		}
		return notFoundResult("")
	}
	if rest[1] == "comments" {
		if method == "POST" {
			text := bodyStr(body, "body")
			comment := &IssueComment{ID: s.NextCommentID(), Body: text, Seq: s.NextSeq()}
			issue.Comments = append(issue.Comments, comment)
			InvalidateIssueJSON(repo, num)
			return restResult{status: 201, body: map[string]any{
				"id": comment.ID, "body": text, "user": userJSON(),
				"created_at": FixedISO, "updated_at": FixedISO,
			}}
		}
		if method == "GET" {
			items := make([]map[string]any, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				items = append(items, map[string]any{
					"id": c.ID, "body": c.Body, "user": userJSON(),
					"created_at": FixedISO, "updated_at": FixedISO,
				})
			}
			return restResult{status: 200, body: items}
		}
	}
	return notFoundResult("")
}

func handlePulls(s *Store, repo *Repo, method string, rest []string, query url.Values, body map[string]any) restResult {
	if len(rest) == 0 {
		if method == "POST" {
			title := bodyStr(body, "title")
			pullBody := bodyStr(body, "body")
			head := bodyStr(body, "head")
			base := bodyStr(body, "base")
			if base == "" {
				base = repo.DefaultBranch
			}
			if i := strings.LastIndex(head, ":"); i >= 0 {
				head = head[i+1:]
			}
			if i := strings.LastIndex(base, ":"); i >= 0 {
				base = base[i+1:]
			}
			created, errMsg := s.StoreCreatePull(repo, title, pullBody, head, base)
			if errMsg != "" {
				return restResult{status: 422, body: map[string]any{"message": errMsg}}
			}
			return restResult{status: 201, body: pullJSON(s, repo, created)}
		}
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			var list []*Pull
			for _, p := range repo.Pulls {
				if state == "all" || p.State == state {
					list = append(list, p)
				}
			}
			cp := append([]*Pull(nil), list...)
			sort.Slice(cp, func(a, b int) bool { return cp[a].Seq > cp[b].Seq })
			page := paginate(cp, query)
			items := make([]pullObj, len(page))
			for i, p := range page {
				items[i] = pullJSON(s, repo, p)
			}
			return restResult{status: 200, body: items}
		}
		return notFoundResult("")
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		return notFoundResult("")
	}
	var pull *Pull
	for _, p := range repo.Pulls {
		if p.Number == num {
			pull = p
			break
		}
	}
	if pull == nil {
		return notFoundResult("")
	}
	if len(rest) == 1 {
		if method == "GET" {
			return restResult{status: 200, body: pullJSON(s, repo, pull)}
		}
		return notFoundResult("")
	}
	if rest[1] == "files" && method == "GET" {
		headMap := s.TreeMap(repo, pull.HeadSha)
		items := make([]map[string]any, 0, len(pull.Files))
		for _, f := range pull.Files {
			sha := headMap[f.Filename]
			items = append(items, map[string]any{
				"filename": f.Filename, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
				"sha": sha, "patch": "",
			})
		}
		return restResult{status: 200, body: items}
	}
	if rest[1] == "merge" && method == "PUT" {
		status, sha, message := s.StoreMergePull(repo, pull)
		if status == 405 {
			return restResult{status: 405, body: map[string]any{"message": message}}
		}
		return restResult{status: 200, body: map[string]any{"merged": true, "sha": sha, "message": message}}
	}
	return notFoundResult("")
}
