// Git smart-HTTP protocol (upload-pack v2, receive-pack v0) over the store.
// Plain (non-deltified) packfiles only on the way out; deltas on receipt are
// resolved by the store's pack reader.
package main

import (
	"bytes"
	"sort"
	"strings"
)

const hexDigits = "0123456789abcdef"

// hex4 renders a pkt-line length as 4 lowercase hex chars.
func hex4(n int) string {
	return string([]byte{
		hexDigits[(n>>12)&0xf],
		hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf],
		hexDigits[n&0xf],
	})
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// pktLine frames one payload as a pkt-line: a single allocation.
func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	n := len(payload) + 4
	out[0] = hexDigits[(n>>12)&0xf]
	out[1] = hexDigits[(n>>8)&0xf]
	out[2] = hexDigits[(n>>4)&0xf]
	out[3] = hexDigits[n&0xf]
	copy(out[4:], payload)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

// frameLines frames several ASCII lines as pkt-lines into one buffer.
func frameLines(lines []string) []byte {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(hex4(len(line) + 4))
		b.WriteString(line)
	}
	return []byte(b.String())
}

// frameLinesFlush frames lines plus a trailing flush (0000).
func frameLinesFlush(lines []string) []byte {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(hex4(len(line) + 4))
		b.WriteString(line)
	}
	b.WriteString("0000")
	return []byte(b.String())
}

var pktFlushBytes = []byte("0000")
var pktDelimBytes = []byte("0001")

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktEnd
)

type pkt struct {
	kind    pktKind
	payload []byte
}

// decodePktLines splits a buffer into pkt-line frames.
func decodePktLines(buf []byte) []pkt {
	var out []pkt
	off := 0
	for off+4 <= len(buf) {
		h0, h1, h2, h3 := hexVal(buf[off]), hexVal(buf[off+1]), hexVal(buf[off+2]), hexVal(buf[off+3])
		if h0 < 0 || h1 < 0 || h2 < 0 || h3 < 0 {
			break
		}
		n := (h0 << 12) | (h1 << 8) | (h2 << 4) | h3
		switch n {
		case 0:
			out = append(out, pkt{kind: pktFlush})
			off += 4
			continue
		case 1:
			out = append(out, pkt{kind: pktDelim})
			off += 4
			continue
		case 2:
			out = append(out, pkt{kind: pktEnd})
			off += 4
			continue
		}
		if off+n > len(buf) {
			break
		}
		out = append(out, pkt{kind: pktData, payload: buf[off+4 : off+n]})
		off += n
	}
	return out
}

// SortedRefs returns the repo's refs sorted by name, cached per version.
func SortedRefs(r *Repo) []RefEntry {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.sortedRefsCache != nil {
		return r.sortedRefsCache
	}
	refs := make([]RefEntry, 0, len(r.Refs))
	for name, oid := range r.Refs {
		refs = append(refs, RefEntry{Name: name, OID: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	r.sortedRefsCache = refs
	return refs
}

func headTarget(r *Repo) string { return "refs/heads/" + r.DefaultBranch }

// v2Advertisement is the constant upload-pack v2 capability advertisement.
var v2AdvertisementBytes = func() []byte {
	lines := []string{
		"version 2\n",
		"agent=github-core-clone/1.0\n",
		"ls-refs=unborn\n",
		"fetch=shallow filter\n",
		"server-option\n",
		"object-format=sha1\n",
	}
	parts := [][]byte{pktLineStr("# service=git-upload-pack\n"), pktFlushBytes, frameLinesFlush(lines)}
	return bytes.Join(parts, nil)
}()

// V0Advertisement returns the cached v0 ref advertisement for a service.
func V0Advertisement(s *Store, r *Repo, service string) []byte {
	r.cacheMu.Lock()
	if service == "git-upload-pack" && r.v0UpCache != nil {
		out := r.v0UpCache
		r.cacheMu.Unlock()
		return out
	}
	if service == "git-receive-pack" && r.v0RecvCache != nil {
		out := r.v0RecvCache
		r.cacheMu.Unlock()
		return out
	}
	r.cacheMu.Unlock()

	refs := SortedRefs(r)
	var caps string
	if service == "git-receive-pack" {
		caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=github-core-clone/1.0"
	} else {
		caps = "side-band-64k multi_ack_detailed thin-pack ofs-delta shallow agent=github-core-clone/1.0"
	}
	head := headTarget(r)
	headOID, hasHead := r.Refs[head]
	var lines []string
	if len(refs) == 0 {
		lines = append(lines, ZeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		oid := headOID
		if !hasHead {
			oid = ZeroOID
		}
		lines = append(lines, oid+" HEAD\x00"+caps+" symref=HEAD:"+head+"\n")
		for _, ref := range refs {
			lines = append(lines, ref.OID+" "+ref.Name+"\n")
			if obj, ok := r.Objects[ref.OID]; ok && obj.Type == ObjTag {
				lines = append(lines, s.Peel(r, ref.OID)+" "+ref.Name+"^{}\n")
			}
		}
	}
	out := bytes.Join([][]byte{
		pktLineStr("# service=" + service + "\n"),
		pktFlushBytes,
		frameLinesFlush(lines),
	}, nil)
	r.cacheMu.Lock()
	if service == "git-upload-pack" {
		r.v0UpCache = out
	} else {
		r.v0RecvCache = out
	}
	r.cacheMu.Unlock()
	return out
}

type fetchResult struct {
	objects   []*GitObject
	shallow   []string
	unshallow []string
}

// FetchObjects computes the objects to send for wants, minus what the client
// already holds, with depth/shallow handling.
func FetchObjects(s *Store, r *Repo, wants, haves []string, depth int, hasDepth bool, shallowBoundaries []string) fetchResult {
	shallowSet := map[string]bool{}
	for _, b := range shallowBoundaries {
		shallowSet[b] = true
	}
	// What the client already holds: reachable from its haves, never past a
	// shallow boundary (a shallow clone holds the boundary commit and its
	// tree, not the boundary's parents).
	haveReachable := map[string]bool{}
	queue := append([]string(nil), haves...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if haveReachable[oid] {
			continue
		}
		obj, ok := r.Objects[oid]
		if !ok {
			continue
		}
		haveReachable[oid] = true
		switch obj.Type {
		case ObjCommit:
			c := ParseCommit(obj.Data)
			for t := range s.Reachable(r, []string{c.Tree}) {
				haveReachable[t] = true
			}
			if !shallowSet[oid] {
				queue = append(queue, c.Parents...)
			}
		case ObjTag:
			queue = append(queue, ParseTag(obj.Data).Object)
		case ObjTree:
			for _, e := range ParseTree(obj.Data) {
				queue = append(queue, e.OID)
			}
		}
	}

	wanted := map[string]bool{}
	shallow := map[string]bool{}
	unshallow := map[string]bool{}
	if hasDepth {
		type item struct {
			oid string
			gen int
		}
		seenCommit := map[string]bool{}
		q := make([]item, 0, len(wants))
		for _, w := range wants {
			q = append(q, item{oid: w, gen: 0})
		}
		for len(q) > 0 {
			it := q[0]
			q = q[1:]
			if seenCommit[it.oid] {
				continue
			}
			seenCommit[it.oid] = true
			obj, ok := r.Objects[it.oid]
			if !ok {
				continue
			}
			if obj.Type == ObjTag {
				wanted[it.oid] = true
				target := ParseTag(obj.Data).Object
				if !seenCommit[target] {
					q = append(q, item{oid: target, gen: it.gen})
				}
				continue
			}
			if obj.Type != ObjCommit {
				wanted[it.oid] = true
				continue
			}
			c := ParseCommit(obj.Data)
			if it.gen >= depth {
				continue
			}
			wanted[it.oid] = true
			for t := range s.Reachable(r, []string{c.Tree}) {
				wanted[t] = true
			}
			if it.gen == depth-1 {
				shallow[it.oid] = true
			} else {
				for _, p := range c.Parents {
					q = append(q, item{oid: p, gen: it.gen + 1})
				}
			}
		}
		for _, b := range shallowBoundaries {
			if seenCommit[b] && !shallow[b] {
				unshallow[b] = true
			}
		}
	} else {
		for _, w := range wants {
			for oid := range s.Reachable(r, []string{w}) {
				wanted[oid] = true
			}
		}
	}
	type paired struct {
		obj *GitObject
		oid string
	}
	var list []paired
	for oid := range wanted {
		if haveReachable[oid] {
			continue
		}
		if obj, ok := r.Objects[oid]; ok {
			list = append(list, paired{obj, oid})
		}
	}
	// Deterministic pack order: commits, trees, blobs, tags, oid-sorted.
	rank := func(t ObjType) int {
		switch t {
		case ObjCommit:
			return 0
		case ObjTree:
			return 1
		case ObjBlob:
			return 2
		default:
			return 3
		}
	}
	sort.Slice(list, func(i, j int) bool {
		ri, rj := rank(list[i].obj.Type), rank(list[j].obj.Type)
		if ri != rj {
			return ri < rj
		}
		return list[i].oid < list[j].oid
	})
	objects := make([]*GitObject, len(list))
	for i, p := range list {
		objects[i] = p.obj
	}
	var shallowList, unshallowList []string
	for k := range shallow {
		shallowList = append(shallowList, k)
	}
	for k := range unshallow {
		unshallowList = append(unshallowList, k)
	}
	sort.Strings(shallowList)
	sort.Strings(unshallowList)
	return fetchResult{objects: objects, shallow: shallowList, unshallow: unshallowList}
}

// sidebandPack frames a pack in side-band-64k (band 1) pkt-lines.
func sidebandPack(pack []byte) []byte {
	if len(pack) == 0 {
		pack = WritePack(nil)
	}
	const chunk = 65515
	frames := (len(pack) + chunk - 1) / chunk
	if frames < 1 {
		frames = 1
	}
	parts := make([][]byte, frames)
	for i := 0; i < frames; i++ {
		off := i * chunk
		n := chunk
		if off+n > len(pack) {
			n = len(pack) - off
		}
		frame := make([]byte, 4+1+n)
		total := 5 + n
		frame[0] = hexDigits[(total>>12)&0xf]
		frame[1] = hexDigits[(total>>8)&0xf]
		frame[2] = hexDigits[(total>>4)&0xf]
		frame[3] = hexDigits[total&0xf]
		frame[4] = 1
		copy(frame[5:], pack[off:off+n])
		parts[i] = frame
	}
	return bytes.Join(parts, nil)
}


// ---- upload-pack v2 request parsing ---------------------------------------

type v2Request struct {
	command  string
	args     map[string][]string
	wants    []string
	haves    []string
	shallow  []string
	depth    int
	hasDepth bool
	prefixes []string
	peel     bool
	symrefs  bool
	unborn   bool
}

func stripNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s[:len(s)-1]
	}
	return s
}

// ParseV2Request decodes a v2 upload-pack request body.
func ParseV2Request(body []byte) *v2Request {
	req := &v2Request{args: map[string][]string{}}
	for _, p := range decodePktLines(body) {
		if p.kind != pktData {
			continue
		}
		line := stripNL(string(p.payload))
		if strings.HasPrefix(line, "command=") {
			req.command = line[len("command="):]
			continue
		}
		eq := strings.Index(line, "=")
		sp := strings.Index(line, " ")
		var key, val string
		if eq >= 0 && (sp < 0 || eq < sp) {
			key, val = line[:eq], line[eq+1:]
		} else if sp >= 0 {
			key, val = line[:sp], line[sp+1:]
		} else {
			key, val = line, ""
		}
		req.args[key] = append(req.args[key], val)
		switch key {
		case "want":
			req.wants = append(req.wants, val)
		case "have":
			req.haves = append(req.haves, val)
		case "shallow":
			req.shallow = append(req.shallow, val)
		case "deepen":
			req.depth = atoi(val)
			req.hasDepth = true
		case "ref-prefix":
			req.prefixes = append(req.prefixes, val)
		case "peel":
			req.peel = true
		case "symrefs":
			req.symrefs = true
		case "unborn":
			req.unborn = true
		}
	}
	if req.command == "" {
		return nil
	}
	return req
}

// atoi parses a decimal integer without fmt/strconv overhead in hot paths.
func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// LsRefsResponse answers a v2 ls-refs command, cached per repo version.
func LsRefsResponse(s *Store, r *Repo, req *v2Request) []byte {
	var kb strings.Builder
	for i, p := range req.prefixes {
		if i > 0 {
			kb.WriteByte(0)
		}
		kb.WriteString(p)
	}
	kb.WriteByte('|')
	if req.peel {
		kb.WriteByte('1')
	} else {
		kb.WriteByte('0')
	}
	if req.symrefs {
		kb.WriteByte('1')
	} else {
		kb.WriteByte('0')
	}
	if req.unborn {
		kb.WriteByte('1')
	} else {
		kb.WriteByte('0')
	}
	key := kb.String()
	r.cacheMu.Lock()
	if cached, ok := r.lsRefsCache[key]; ok {
		r.cacheMu.Unlock()
		return cached
	}
	r.cacheMu.Unlock()

	head := headTarget(r)
	headOID, hasHead := r.Refs[head]
	refs := SortedRefs(r)
	keep := func(name string) bool {
		if len(req.prefixes) == 0 {
			return true
		}
		for _, p := range req.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	keepHead := false
	if len(req.prefixes) == 0 {
		keepHead = true
	} else {
		for _, p := range req.prefixes {
			if strings.HasPrefix("HEAD", p) || strings.HasPrefix(p, "HEAD") {
				keepHead = true
				break
			}
		}
	}
	var lines []string
	if req.symrefs && keepHead {
		if hasHead {
			lines = append(lines, headOID+" HEAD symref-target:"+head+"\n")
		} else {
			lines = append(lines, "unborn HEAD symref-target:"+head+"\n")
		}
	}
	for _, ref := range refs {
		if !keep(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if req.peel {
			if obj, ok := r.Objects[ref.OID]; ok && obj.Type == ObjTag {
				line += " peeled:" + s.Peel(r, ref.OID)
			}
		}
		lines = append(lines, line+"\n")
	}
	if len(refs) == 0 && req.unborn && !req.symrefs {
		lines = append(lines, "unborn HEAD symref-target:"+head+"\n")
	}
	out := frameLinesFlush(lines)
	r.cacheMu.Lock()
	r.lsRefsCache[key] = out
	r.cacheMu.Unlock()
	return out
}

// fetchCacheKey builds the fetch cache key; fast path for the common
// single-want, no-haves clone/fetch.
func fetchCacheKey(prefix string, wants, haves []string, depth int, hasDepth bool, shallow []string) string {
	d := ""
	if hasDepth {
		d = itoa(depth)
	}
	if len(wants) == 1 && len(haves) == 0 && len(shallow) == 0 {
		return prefix + "|" + wants[0] + "|" + d
	}
	join := func(xs []string) string {
		if len(xs) <= 1 {
			return strings.Join(xs, ",")
		}
		cp := append([]string(nil), xs...)
		sort.Strings(cp)
		return strings.Join(cp, ",")
	}
	return prefix + "|" + join(wants) + "|" + join(haves) + "|" + d + "|" + join(shallow)
}

func cachedFetch(r *Repo, key string) ([]byte, bool) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	out, ok := r.fetchCache[key]
	return out, ok
}

func storeFetch(r *Repo, key string, out []byte) {
	r.cacheMu.Lock()
	r.fetchCache[key] = out
	r.cacheMu.Unlock()
}

// V2FetchResponse answers a v2 fetch command.
func V2FetchResponse(s *Store, r *Repo, req *v2Request) []byte {
	key := fetchCacheKey("v2", req.wants, req.haves, req.depth, req.hasDepth, req.shallow)
	if cached, ok := cachedFetch(r, key); ok {
		return cached
	}
	res := FetchObjects(s, r, req.wants, req.haves, req.depth, req.hasDepth, req.shallow)
	pack := WritePack(res.objects)
	parts := [][]byte{pktLineStr("acknowledgments\n")}
	if len(req.haves) > 0 {
		parts = append(parts, pktLineStr("ready\n"))
	} else {
		parts = append(parts, pktLineStr("NAK\n"))
	}
	parts = append(parts, pktDelimBytes)
	if len(res.shallow) > 0 || len(res.unshallow) > 0 || req.hasDepth {
		parts = append(parts, pktLineStr("shallow-info\n"))
		for _, x := range res.shallow {
			parts = append(parts, pktLineStr("shallow "+x+"\n"))
		}
		for _, x := range res.unshallow {
			parts = append(parts, pktLineStr("unshallow "+x+"\n"))
		}
		parts = append(parts, pktDelimBytes)
	}
	parts = append(parts, pktLineStr("packfile\n"), sidebandPack(pack), pktFlushBytes)
	out := bytes.Join(parts, nil)
	storeFetch(r, key, out)
	return out
}

// V0FetchResponse answers a v0 upload-pack fetch.
func V0FetchResponse(s *Store, r *Repo, wants, haves []string, depth int, hasDepth bool, shallow []string) []byte {
	key := fetchCacheKey("v0", wants, haves, depth, hasDepth, shallow)
	if cached, ok := cachedFetch(r, key); ok {
		return cached
	}
	res := FetchObjects(s, r, wants, haves, depth, hasDepth, shallow)
	pack := WritePack(res.objects)
	var parts [][]byte
	for _, x := range res.shallow {
		parts = append(parts, pktLineStr("shallow "+x+"\n"))
	}
	for _, x := range res.unshallow {
		parts = append(parts, pktLineStr("unshallow "+x+"\n"))
	}
	parts = append(parts, pktLineStr("NAK\n"), sidebandPack(pack), pktFlushBytes)
	out := bytes.Join(parts, nil)
	storeFetch(r, key, out)
	return out
}

// ---- receive-pack ---------------------------------------------------------

type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

type PushReport struct {
	UnpackOK bool
	Refs     []pushRefResult
}

type pushRefResult struct {
	Ref    string
	OK     bool
	Reason string
}

// ParseReceivePack splits a receive-pack body into commands and a packfile.
func ParseReceivePack(body []byte) ([]PushCommand, []byte) {
	var commands []PushCommand
	off := 0
	packStart := -1
	for off+4 <= len(body) {
		h0, h1, h2, h3 := hexVal(body[off]), hexVal(body[off+1]), hexVal(body[off+2]), hexVal(body[off+3])
		if h0 < 0 || h1 < 0 || h2 < 0 || h3 < 0 {
			break
		}
		n := (h0 << 12) | (h1 << 8) | (h2 << 4) | h3
		if n == 0 {
			packStart = off + 4
			break
		}
		if n < 4 || off+n > len(body) {
			break
		}
		line := stripNL(string(body[off+4 : off+n]))
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		bits := strings.Split(line, " ")
		if len(bits) >= 3 {
			commands = append(commands, PushCommand{
				OldOID: bits[0],
				NewOID: bits[1],
				Ref:    strings.Join(bits[2:], " "),
			})
		}
		off += n
	}
	var pack []byte
	if packStart >= 0 && packStart < len(body) {
		pack = body[packStart:]
	}
	return commands, pack
}

// ApplyPush ingests a received pack and applies the ref updates, refusing
// non-fast-forwards per ref while still applying the fast-forward ones.
func ApplyPush(s *Store, r *Repo, commands []PushCommand, pack []byte) PushReport {
	report := PushReport{UnpackOK: true}
	refChanged := false
	if len(pack) > 0 {
		objects, err := ReadPack(pack)
		if err != nil {
			report.UnpackOK = false
			for _, c := range commands {
				report.Refs = append(report.Refs, pushRefResult{Ref: c.Ref, Reason: "unpack failed"})
			}
			return report
		}
		inserted := false
		for oid, obj := range objects {
			if _, ok := r.Objects[oid]; !ok {
				r.Objects[oid] = obj
				inserted = true
			}
		}
		if inserted {
			TouchRepo(r)
		}
	}
	for _, cmd := range commands {
		current, hasCurrent := r.Refs[cmd.Ref]
		if cmd.NewOID == ZeroOID {
			// Delete.
			switch {
			case !hasCurrent:
				report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, Reason: "ref does not exist"})
			case cmd.OldOID != ZeroOID && cmd.OldOID != current:
				report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, Reason: "old value does not match"})
			default:
				delete(r.Refs, cmd.Ref)
				refChanged = true
				report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, OK: true})
			}
			continue
		}
		if _, ok := r.Objects[cmd.NewOID]; !ok {
			report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, Reason: "object not found"})
			continue
		}
		if !hasCurrent {
			if cmd.OldOID != ZeroOID {
				report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, Reason: "old value does not match"})
				continue
			}
			r.Refs[cmd.Ref] = cmd.NewOID
			refChanged = true
			report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, OK: true})
			continue
		}
		if cmd.OldOID != current {
			report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, Reason: "old value does not match"})
			continue
		}
		if !s.IsAncestor(r, current, cmd.NewOID) {
			report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
			continue
		}
		r.Refs[cmd.Ref] = cmd.NewOID
		refChanged = true
		report.Refs = append(report.Refs, pushRefResult{Ref: cmd.Ref, OK: true})
	}
	if refChanged {
		TouchRepo(r)
	}
	return report
}

// PushReportResponse frames a receive-pack report as one side-band frame.
func PushReportResponse(report PushReport) []byte {
	var lines []string
	if report.UnpackOK {
		lines = append(lines, "unpack ok\n")
	} else {
		lines = append(lines, "unpack error\n")
	}
	for _, x := range report.Refs {
		if x.OK {
			lines = append(lines, "ok "+x.Ref+"\n")
		} else {
			lines = append(lines, "ng "+x.Ref+" "+x.Reason+"\n")
		}
	}
	inner := frameLines(lines)
	framed := make([]byte, 4+1+len(inner))
	total := 5 + len(inner)
	framed[0] = hexDigits[(total>>12)&0xf]
	framed[1] = hexDigits[(total>>8)&0xf]
	framed[2] = hexDigits[(total>>4)&0xf]
	framed[3] = hexDigits[total&0xf]
	framed[4] = 1
	copy(framed[5:], inner)
	return bytes.Join([][]byte{framed, pktFlushBytes}, nil)
}

// ParseV0Fetch decodes a v0 upload-pack request body.
func ParseV0Fetch(body []byte) (wants, haves, shallow []string, depth int, hasDepth bool) {
	for _, p := range decodePktLines(body) {
		if p.kind != pktData {
			continue
		}
		line := stripNL(string(p.payload))
		sp := strings.Index(line, " ")
		key, rest := line, ""
		if sp >= 0 {
			key, rest = line[:sp], line[sp+1:]
		}
		switch key {
		case "want":
			if i := strings.Index(rest, " "); i >= 0 {
				rest = rest[:i]
			}
			wants = append(wants, rest)
		case "have":
			haves = append(haves, rest)
		case "deepen":
			depth = atoi(rest)
			hasDepth = true
		case "shallow":
			shallow = append(shallow, rest)
		}
	}
	return wants, haves, shallow, depth, hasDepth
}
