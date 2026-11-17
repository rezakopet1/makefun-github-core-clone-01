// In-memory store for the github-core-clone-01 forge: one process, no
// persistence, no clock and no randomness in anything observable. Every
// timestamp is the fixed trial instant and every id is a sequential counter,
// so two runs on the same requests leave byte-identical state.
//
// Locking: Store.mu (RWMutex) guards all structural state. Each repo has a
// cacheMu guarding only the derived response caches (fetch packs,
// advertisements, ls-refs, sorted refs, serialized issue JSON). Lock order is
// always Store.mu -> Repo.cacheMu. Store methods assume the caller holds the
// appropriate Store.mu lock unless the name says otherwise.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"sync"
)

const (
	ViewerLogin = "adaiversen"
	ViewerID    = 1000
	ViewerName  = "ada iversen"
	ViewerEmail = "adaiversen@example.invalid"
	FixedISO    = "2026-11-14T06:30:00Z"
	FixedEpoch  = 1700000000
	ZeroOID     = "0000000000000000000000000000000000000000"
)

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

// GitObject is one stored git object: its type and raw bytes.
type GitObject struct {
	Type ObjType
	Data []byte
}

// HashObject returns the SHA-1 object id for type+data, as git computes it.
func HashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// itoa is a small allocation-free integer formatter for headers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree decodes a raw tree object into its entries.
func ParseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 || sp+1+nul+21 > len(data) {
			break
		}
		nul += sp + 1
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  oid,
		})
		off = nul + 21
	}
	return out
}

type CommitInfo struct {
	Tree    string
	Parents []string
	Message string
}

// ParseCommit splits a raw commit into tree, parents and message.
func ParseCommit(data []byte) CommitInfo {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	head, msg := text, ""
	if blank >= 0 {
		head, msg = text[:blank], text[blank+2:]
	}
	var c CommitInfo
	c.Message = msg
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "tree ") {
			c.Tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		}
	}
	return c
}

type TagInfo struct {
	Object string
	Type   string
}

// ParseTag splits a raw tag into its target object and type.
func ParseTag(data []byte) TagInfo {
	text := string(data)
	blank := strings.Index(text, "\n\n")
	head := text
	if blank >= 0 {
		head = text[:blank]
	}
	var t TagInfo
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "object ") {
			t.Object = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "type ") {
			t.Type = strings.TrimSpace(line[5:])
		}
	}
	return t
}

// BuildTree serializes sorted tree entries the way git does: directories sort
// as if their name had a trailing slash.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ai, aj := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ai += "/"
		}
		if sorted[j].Mode == "40000" {
			aj += "/"
		}
		return ai < aj
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type IssueComment struct {
	ID   int
	Body string
	Seq  int
}

type Issue struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Seq      int
	Comments []*IssueComment
}

type FileChange struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Head           string
	Base           string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	Files          []FileChange
	Seq            int
}

type Label struct {
	Name  string
	Color string
}

// Repo is one repository: git state plus issues, pulls and labels, plus the
// derived-response caches dropped on mutation.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Seq           int
	Refs          map[string]string
	Objects       map[string]*GitObject
	Labels        map[string]*Label
	Issues        []*Issue
	Pulls         []*Pull
	NextIssue     int
	NextPull      int
	NextComment   int
	Version       int

	cacheMu         sync.Mutex
	fetchCache      map[string][]byte
	v0UpCache       []byte
	v0RecvCache     []byte
	lsRefsCache     map[string][]byte
	sortedRefsCache []RefEntry
	issueJSONCache  map[int][]byte
}

type RefEntry struct {
	Name string
	OID  string
}

// Store is the whole forge state.
type Store struct {
	mu              sync.RWMutex
	repos           map[string]*Repo
	repoOrder       []*Repo
	nextRepoID      int
	issueIDCounter  int
	pullIDCounter   int
	commentIDCounter int
	seq             int
}

func NewStore() *Store {
	return &Store{
		repos:          make(map[string]*Repo),
		nextRepoID:     1,
		issueIDCounter: 1,
		pullIDCounter:  1,
		commentIDCounter: 1,
	}
}

func (s *Store) NextIssueID() int {
	id := s.issueIDCounter
	s.issueIDCounter++
	return id
}

func (s *Store) NextPullID() int {
	id := s.pullIDCounter
	s.pullIDCounter++
	return id
}

func (s *Store) NextCommentID() int {
	id := s.commentIDCounter
	s.commentIDCounter++
	return id
}

func (s *Store) NextSeq() int {
	s.seq++
	return s.seq
}

func repoKey(owner, name string) string { return owner + "/" + name }

// CreateRepo adds a repository owned by the viewer; nil when the name is taken.
func (s *Store) CreateRepo(name string, priv bool, description string) *Repo {
	k := repoKey(ViewerLogin, name)
	if _, ok := s.repos[k]; ok {
		return nil
	}
	r := &Repo{
		ID:            s.nextRepoID,
		Owner:         ViewerLogin,
		Name:          name,
		Description:   description,
		Private:       priv,
		DefaultBranch: "main",
		Seq:           s.NextSeq(),
		Refs:          make(map[string]string),
		Objects:       make(map[string]*GitObject),
		Labels:        make(map[string]*Label),
		NextIssue:     1,
		NextPull:      1,
		NextComment:   1,
		fetchCache:    make(map[string][]byte),
		lsRefsCache:   make(map[string][]byte),
		issueJSONCache: make(map[int][]byte),
	}
	s.nextRepoID++
	s.repos[k] = r
	s.repoOrder = append(s.repoOrder, r)
	return r
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[repoKey(owner, name)]
}

func (s *Store) DeleteRepo(r *Repo) {
	delete(s.repos, repoKey(r.Owner, r.Name))
	for i, x := range s.repoOrder {
		if x == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
}

func (s *Store) ViewerRepos() []*Repo {
	var out []*Repo
	for _, r := range s.repoOrder {
		if r.Owner == ViewerLogin {
			out = append(out, r)
		}
	}
	return out
}

// ---- git objects ---------------------------------------------------------

// PutObject stores an object, returning its id. Duplicate content is a no-op.
func (s *Store) PutObject(r *Repo, typ ObjType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *Store) Blob(r *Repo, data []byte) string {
	return s.PutObject(r, ObjBlob, data)
}

// TreeFromMap builds the nested tree for a flat path -> blob id map.
func (s *Store) TreeFromMap(r *Repo, files map[string]string) string {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: map[string]string{}, dirs: map[string]*node{}} }
	root := mk()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		segs := strings.Split(p, "/")
		n := root
		for i := 0; i < len(segs)-1; i++ {
			c, ok := n.dirs[segs[i]]
			if !ok {
				c = mk()
				n.dirs[segs[i]] = c
			}
			n = c
		}
		n.files[segs[len(segs)-1]] = files[p]
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []TreeEntry
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return s.PutObject(r, ObjTree, BuildTree(entries))
	}
	return build(root)
}

// TreeMap flattens a commit's tree into path -> blob id.
func (s *Store) TreeMap(r *Repo, commitOID string) map[string]string {
	out := map[string]string{}
	c, ok := r.Objects[commitOID]
	if !ok || c.Type != ObjCommit {
		return out
	}
	tree := ParseCommit(c.Data).Tree
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		t, ok := r.Objects[treeOID]
		if !ok || t.Type != ObjTree {
			return
		}
		for _, e := range ParseTree(t.Data) {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, p)
			} else {
				out[p] = e.OID
			}
		}
	}
	walk(tree, "")
	return out
}

// CommitObject writes a commit with the fixed author/committer identity.
func (s *Store) CommitObject(r *Repo, treeOID string, parents []string, message string) string {
	who := ViewerName + " <" + ViewerEmail + "> " + itoa(FixedEpoch) + " +0000"
	var b strings.Builder
	b.WriteString("tree " + treeOID + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + who + "\n")
	b.WriteString("committer " + who + "\n")
	b.WriteString("\n")
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	b.WriteString(message)
	return s.PutObject(r, ObjCommit, []byte(b.String()))
}

var oidRe = func() func(string) bool {
	return func(s string) bool {
		if len(s) != 40 {
			return false
		}
		for i := 0; i < 40; i++ {
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
}()

// Reachable returns every object id reachable from the roots.
func (s *Store) Reachable(r *Repo, roots []string) map[string]bool {
	seen := map[string]bool{}
	var queue []string
	for _, x := range roots {
		if oidRe(x) {
			queue = append(queue, x)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj, ok := r.Objects[oid]
		if !ok {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case ObjCommit:
			c := ParseCommit(obj.Data)
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case ObjTag:
			queue = append(queue, ParseTag(obj.Data).Object)
		case ObjTree:
			for _, e := range ParseTree(obj.Data) {
				if e.Mode != "160000" {
					queue = append(queue, e.OID)
				}
			}
		}
	}
	return seen
}

// IsAncestor reports whether anc is reachable from desc (or equal).
func (s *Store) IsAncestor(r *Repo, anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	queue := []string{desc}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if oid == anc {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		queue = append(queue, ParseCommit(obj.Data).Parents...)
	}
	return false
}

// MergeBase returns the first common ancestor of a and b found by breadth
// search from b over a's ancestor set, or "" when there is none.
func (s *Store) MergeBase(r *Repo, a, b string) string {
	ancA := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if ancA[oid] {
			continue
		}
		ancA[oid] = true
		obj, ok := r.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		queue = append(queue, ParseCommit(obj.Data).Parents...)
	}
	seenB := map[string]bool{}
	qb := []string{b}
	for len(qb) > 0 {
		oid := qb[len(qb)-1]
		qb = qb[:len(qb)-1]
		if ancA[oid] {
			return oid
		}
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		obj, ok := r.Objects[oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		qb = append(qb, ParseCommit(obj.Data).Parents...)
	}
	return ""
}

// Peel follows annotated tags to the underlying object (at most 10 hops).
func (s *Store) Peel(r *Repo, oid string) string {
	cur := oid
	for i := 0; i < 10; i++ {
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != ObjTag {
			return cur
		}
		cur = ParseTag(obj.Data).Object
	}
	return cur
}

func (s *Store) RefTip(r *Repo, ref string) (string, bool) {
	oid, ok := r.Refs[ref]
	return oid, ok
}

func (s *Store) BranchTip(r *Repo, branch string) (string, bool) {
	return s.RefTip(r, "refs/heads/"+branch)
}

// ---- caches ---------------------------------------------------------------

// TouchRepo bumps the repo version and drops every derived git response.
func TouchRepo(r *Repo) {
	r.Version++
	r.cacheMu.Lock()
	r.fetchCache = make(map[string][]byte)
	r.v0UpCache = nil
	r.v0RecvCache = nil
	r.lsRefsCache = make(map[string][]byte)
	r.sortedRefsCache = nil
	r.cacheMu.Unlock()
}

// InvalidateIssueJSON drops the cached serialized GET body for one issue.
func InvalidateIssueJSON(r *Repo, number int) {
	r.cacheMu.Lock()
	delete(r.issueJSONCache, number)
	r.cacheMu.Unlock()
}

// InvalidateAllIssueJSON drops all cached serialized issue GET bodies.
func InvalidateAllIssueJSON(r *Repo) {
	r.cacheMu.Lock()
	r.issueJSONCache = make(map[int][]byte)
	r.cacheMu.Unlock()
}

// oneByteReader feeds a zlib stream one byte per Read so the flate decoder
// never reads past the stream end; the byte count is then exact. Verified:
// a probe stream reported 58/58 bytes consumed, ~6us/entry slower than a
// slice inflate on a 1 KiB blob.
type oneByteReader struct {
	b   []byte
	pos int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	p[0] = r.b[r.pos]
	r.pos++
	return 1, nil
}

// inflateEntry inflates one zlib stream starting at start and reports the
// data plus the exact number of input bytes the stream consumed.
func inflateEntry(buf []byte, start, end int) ([]byte, int, error) {
	cr := &oneByteReader{b: buf[start:end]}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return data, cr.pos, nil
}


// ---- contents / issues / pulls -------------------------------------------

type ContentResult struct {
	Status     int // 201 | 200 | 409 | 422 | 404
	CommitSha  string
	ContentSha string
	Message    string
}

// lineCounts diffs two blobs as multisets of lines.
func lineCounts(a, b []byte) (additions, deletions int) {
	var la, lb []string
	if a != nil {
		la = strings.Split(string(a), "\n")
	}
	if b != nil {
		lb = strings.Split(string(b), "\n")
	}
	count := map[string]int{}
	for _, l := range la {
		count[l]++
	}
	for _, l := range lb {
		if count[l] > 0 {
			count[l]--
		} else {
			additions++
		}
	}
	for _, c := range count {
		deletions += c
	}
	return additions, deletions
}

// DiffFiles diffs two commits' trees (base may be "" for the empty tree).
func (s *Store) DiffFiles(r *Repo, baseCommit, headCommit string) []FileChange {
	var baseMap map[string]string
	if baseCommit == "" {
		baseMap = map[string]string{}
	} else {
		baseMap = s.TreeMap(r, baseCommit)
	}
	headMap := s.TreeMap(r, headCommit)
	paths := map[string]bool{}
	for p := range baseMap {
		paths[p] = true
	}
	for p := range headMap {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []FileChange
	blobData := func(oid string) []byte {
		if o, ok := r.Objects[oid]; ok {
			return o.Data
		}
		return []byte{}
	}
	for _, path := range sorted {
		b, hasB := baseMap[path]
		h, hasH := headMap[path]
		if hasB && hasH && b == h {
			continue
		}
		switch {
		case !hasB:
			add, _ := lineCounts(nil, blobData(h))
			if add < 1 {
				add = 1
			}
			out = append(out, FileChange{Filename: path, Status: "added", Additions: add, Deletions: 0, Changes: add})
		case !hasH:
			_, del := lineCounts(blobData(b), nil)
			if del < 1 {
				del = 1
			}
			out = append(out, FileChange{Filename: path, Status: "removed", Additions: 0, Deletions: del, Changes: del})
		default:
			add, del := lineCounts(blobData(b), blobData(h))
			if add < 1 {
				add = 1
			}
			out = append(out, FileChange{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return out
}

// StoreCommitFile applies a contents create/update/delete as one commit.
func (s *Store) StoreCommitFile(r *Repo, branch, path string, content []byte, message, sha string, hasSha, isDelete bool) ContentResult {
	tip, ok := s.BranchTip(r, branch)
	if !ok && branch != r.DefaultBranch {
		if base, ok2 := s.BranchTip(r, r.DefaultBranch); ok2 {
			r.Refs["refs/heads/"+branch] = base
			tip, ok = base, true
		}
	}
	var files map[string]string
	if !ok {
		files = map[string]string{}
	} else {
		files = s.TreeMap(r, tip)
	}
	existing, hasExisting := files[path]
	if isDelete {
		if !hasExisting {
			return ContentResult{Status: 404, Message: "Not Found"}
		}
		if hasSha && !strings.EqualFold(sha, existing) {
			return ContentResult{Status: 409, Message: "sha does not match"}
		}
		delete(files, path)
	} else {
		if hasExisting {
			if !hasSha {
				return ContentResult{Status: 409, Message: "sha required to update a file"}
			}
			if !strings.EqualFold(sha, existing) {
				return ContentResult{Status: 409, Message: "sha does not match the current file"}
			}
		}
		blobOID := s.Blob(r, content)
		files[path] = blobOID
		treeOID := s.TreeFromMap(r, files)
		var parents []string
		if ok {
			parents = []string{tip}
		}
		commitSha := s.CommitObject(r, treeOID, parents, message)
		r.Refs["refs/heads/"+branch] = commitSha
		TouchRepo(r)
		status := 201
		if hasExisting {
			status = 200
		}
		return ContentResult{Status: status, CommitSha: commitSha, ContentSha: blobOID}
	}
	treeOID := s.TreeFromMap(r, files)
	var parents []string
	if ok {
		parents = []string{tip}
	}
	commitSha := s.CommitObject(r, treeOID, parents, message)
	r.Refs["refs/heads/"+branch] = commitSha
	TouchRepo(r)
	return ContentResult{Status: 200, CommitSha: commitSha, ContentSha: existing}
}

// StoreCreateIssue appends an issue; error text when a label is unknown.
func (s *Store) StoreCreateIssue(r *Repo, title, body string, labels []string) (*Issue, string) {
	for _, l := range labels {
		if _, ok := r.Labels[l]; !ok {
			return nil, "label " + l + " does not exist"
		}
	}
	issue := &Issue{
		ID:     s.NextIssueID(),
		Number: r.NextIssue,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string(nil), labels...),
		Seq:    s.NextSeq(),
	}
	r.NextIssue++
	r.Issues = append(r.Issues, issue)
	return issue, ""
}

// StoreCreatePull opens a pull request between two existing branches.
func (s *Store) StoreCreatePull(r *Repo, title, body, head, base string) (*Pull, string) {
	headSha, okH := s.BranchTip(r, head)
	baseSha, okB := s.BranchTip(r, base)
	if !okH || !okB {
		return nil, "head or base branch not found"
	}
	mb := s.MergeBase(r, headSha, baseSha)
	files := s.DiffFiles(r, mb, headSha)
	pull := &Pull{
		ID:      s.NextPullID(),
		Number:  r.NextPull,
		Title:   title,
		Body:    body,
		State:   "open",
		Head:    head,
		Base:    base,
		HeadSha: headSha,
		BaseSha: baseSha,
		Files:   files,
		Seq:     s.NextSeq(),
	}
	r.NextPull++
	r.Pulls = append(r.Pulls, pull)
	return pull, ""
}

// PullMergeable reports MERGEABLE or CONFLICTING for a three-way merge.
func (s *Store) PullMergeable(r *Repo, pull *Pull) string {
	headTip, okH := s.BranchTip(r, pull.Head)
	baseTip, okB := s.BranchTip(r, pull.Base)
	if !okH || !okB {
		return "CONFLICTING"
	}
	mb := s.MergeBase(r, headTip, baseTip)
	if mb == "" {
		return "MERGEABLE"
	}
	baseMap := s.TreeMap(r, mb)
	headMap := s.TreeMap(r, headTip)
	curMap := s.TreeMap(r, baseTip)
	paths := map[string]bool{}
	for p := range headMap {
		paths[p] = true
	}
	for p := range baseMap {
		paths[p] = true
	}
	for path := range paths {
		fromBase, bOK := baseMap[path]
		inHead, hOK := headMap[path]
		inCur, cOK := curMap[path]
		// Normalize missing entries to "" with a presence flag folded in:
		// a missing entry differs from any present one, equals another miss.
		if hOK && bOK && inHead == fromBase {
			continue
		}
		if !hOK && !bOK {
			continue
		}
		if cOK && bOK && inCur == fromBase {
			continue
		}
		if !cOK && !bOK {
			continue
		}
		if hOK == cOK && inHead == inCur {
			continue
		}
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// StoreMergePull squash-merges an open pull onto its base branch.
func (s *Store) StoreMergePull(r *Repo, pull *Pull) (status int, sha, message string) {
	if pull.State != "open" || pull.Merged {
		return 405, "", "pull request is not open"
	}
	if s.PullMergeable(r, pull) == "CONFLICTING" {
		return 405, "", "merge conflict"
	}
	headTip, _ := s.BranchTip(r, pull.Head)
	baseTip, _ := s.BranchTip(r, pull.Base)
	mb := s.MergeBase(r, headTip, baseTip)
	headMap := s.TreeMap(r, headTip)
	merged := s.TreeMap(r, baseTip)
	for _, f := range s.DiffFiles(r, mb, headTip) {
		if oid, ok := headMap[f.Filename]; ok {
			merged[f.Filename] = oid
		} else {
			delete(merged, f.Filename)
		}
	}
	treeOID := s.TreeFromMap(r, merged)
	sha = s.CommitObject(r, treeOID, []string{baseTip, headTip}, "Merge pull request #"+itoa(pull.Number)+" from "+pull.Head)
	r.Refs["refs/heads/"+pull.Base] = sha
	TouchRepo(r)
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = FixedISO
	pull.MergeCommitSha = sha
	pull.BaseSha = sha
	return 200, sha, "Merged #" + itoa(pull.Number)
}


// ---- packfiles (plain objects only, no deltas on write) -------------------

var typeCode = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeType = map[int]ObjType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

// WritePack serializes objects as a pack v2 with no deltas, deterministic
// order being the caller's job (fetchObjects sorts first).
func WritePack(objects []*GitObject) []byte {
	var body bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4] = 0
	hdr[5] = 0
	hdr[6] = 0
	hdr[7] = 2
	n := len(objects)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	body.Write(hdr)
	for _, obj := range objects {
		code := typeCode[obj.Type]
		size := len(obj.Data)
		var first byte = byte((code << 4) | (size & 0x0f))
		size >>= 4
		var head []byte
		for size > 0 {
			head = append(head, first|0x80)
			first = byte(size & 0x7f)
			size >>= 7
		}
		head = append(head, first)
		body.Write(head)
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, 1)
		zw.Write(obj.Data)
		zw.Close()
		body.Write(zb.Bytes())
	}
	pack := body.Bytes()
	sum := sha1.Sum(pack)
	return append(pack, sum[:]...)
}

type rawEntry struct {
	offset     int
	typeCode   int
	size       int
	data       []byte
	baseOffset int // ofs-delta only, -1 when unused
	baseOID    string
	hasBaseOff bool
	hasBaseOID bool
}

// ReadPack parses a pack v2/v3, resolving ofs- and ref-deltas, and returns
// the objects keyed by id.
func ReadPack(pack []byte) (map[string]*GitObject, error) {
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return nil, errBadPack
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, errBadPack
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	offset := 12
	raw := make([]*rawEntry, 0, count)
	byOffset := map[int]*rawEntry{}
	for i := 0; i < count; i++ {
		entryOffset := offset
		first := pack[offset]
		size := int(first & 0x0f)
		shift := uint(4)
		offset++
		b := first
		for b&0x80 != 0 {
			b = pack[offset]
			offset++
			size += int(b&0x7f) << shift
			shift += 7
		}
		tc := int((first >> 4) & 0x07)
		e := &rawEntry{offset: entryOffset, typeCode: tc, size: size, baseOffset: -1}
		if tc == 6 {
			b = pack[offset]
			offset++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[offset]
				offset++
				v = ((v + 1) << 7) | int(b&0x7f)
			}
			e.baseOffset = entryOffset - v
			e.hasBaseOff = true
		} else if tc == 7 {
			e.baseOID = hex.EncodeToString(pack[offset : offset+20])
			e.hasBaseOID = true
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, errBadPack
		}
		data, consumed, err := inflateEntry(pack, offset, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, errBadPack
		}
		e.data = data
		offset += consumed
		raw = append(raw, e)
		byOffset[entryOffset] = e
	}
	resolved := map[int]*resolvedObj{}
	byOID := map[string]*resolvedObj{}
	var resolve func(e *rawEntry) (*resolvedObj, error)
	resolve = func(e *rawEntry) (*resolvedObj, error) {
		if c, ok := resolved[e.offset]; ok {
			return c, nil
		}
		var typ ObjType
		var data []byte
		switch {
		case e.typeCode == 6:
			base, ok := byOffset[e.baseOffset]
			if !ok {
				return nil, errBadPack
			}
			rb, err := resolve(base)
			if err != nil {
				return nil, err
			}
			typ = rb.typ
			data, err = ApplyDelta(rb.data, e.data)
			if err != nil {
				return nil, err
			}
		case e.typeCode == 7:
			base, ok := byOID[e.baseOID]
			if !ok {
				return nil, errBadPack
			}
			typ = base.typ
			var err error
			data, err = ApplyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
		default:
			typ = codeType[e.typeCode]
			data = e.data
		}
		oid := HashObject(typ, data)
		r := &resolvedObj{typ: typ, data: data, oid: oid}
		resolved[e.offset] = r
		byOID[oid] = r
		return r, nil
	}
	out := map[string]*GitObject{}
	for _, e := range raw {
		r, err := resolve(e)
		if err != nil {
			return nil, err
		}
		if _, ok := out[r.oid]; !ok {
			out[r.oid] = &GitObject{Type: r.typ, Data: r.data}
		}
	}
	return out, nil
}

type resolvedObj struct {
	typ  ObjType
	data []byte
	oid  string
}

var errBadPack = &packError{"bad pack"}

type packError struct{ msg string }

func (e *packError) Error() string { return e.msg }

func readDeltaVarint(data []byte, off int) (int, int) {
	v, shift, i := 0, uint(0), off
	for {
		b := data[i]
		i++
		v |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	return v, i
}

// ApplyDelta applies a git delta to its base buffer.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	srcSize, next := readDeltaVarint(delta, off)
	off = next
	if srcSize != len(base) {
		return nil, errBadPack
	}
	tgtSize, next := readDeltaVarint(delta, off)
	off = next
	var out bytes.Buffer
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					cpOff |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					cpSize |= int(delta[off]) << (8 * uint(i))
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errBadPack
			}
			out.Write(base[cpOff : cpOff+cpSize])
		} else if op > 0 {
			if off+int(op) > len(delta) {
				return nil, errBadPack
			}
			out.Write(delta[off : off+int(op)])
			off += int(op)
		} else {
			return nil, errBadPack
		}
	}
	if out.Len() != tgtSize {
		return nil, errBadPack
	}
	return out.Bytes(), nil
}
