package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"context-lens/backend/inspection"
)

// DefaultMaxPositions bounds the in-memory position table. Positions are
// shared message-prefix states; a long append-only conversation contributes
// roughly one entry per unique message prefix, so this cap is generous for a
// local workbench while keeping memory bounded.
const DefaultMaxPositions = 100_000

const positionDomain = "ctxlens-pos/v1"

// Assignment is the additive session placement of one exchange, carried on
// its snapshot. Structure comes from the original inbound request (harness
// behaviour); it never changes after capture.
type Assignment struct {
	SessionID        string `json:"session_id"`
	Depth            int    `json:"depth"`
	Position         string `json:"position,omitempty"`
	ParentPosition   string `json:"parent_position,omitempty"`
	ParentExchangeID string `json:"parent_exchange_id,omitempty"`
	RepeatIndex      int    `json:"repeat_index,omitempty"`
	Fork             bool   `json:"fork,omitempty"`
	ModelChanged     bool   `json:"model_changed,omitempty"`
	ToolsChanged     bool   `json:"tools_changed,omitempty"`
	Root             bool   `json:"root,omitempty"`
}

// Clone returns an independent copy for snapshot value semantics.
func (a *Assignment) Clone() *Assignment {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

// position is one registered request tip: the chain hash of a request's
// complete message history. Only tips are registered (intermediate prefixes
// are not): the next turn of an append-only conversation extends the previous
// turn's tip, a fork extends it with different content, and a rollout repeats
// it exactly. A position therefore knows its session and turn, the rollout
// group of exchanges whose tip it is, and the distinct successor tips observed
// so far (fork detection).
type position struct {
	sessionID       string
	depth           int
	ownerExchangeID string
	tipExchangeIDs  []string
	model           string
	toolsDigest     string
	successors      map[string]struct{}
}

// sessionRecord tracks LRU activity for whole-tree eviction.
type sessionRecord struct {
	protocol       inspection.Protocol
	rootExchangeID string
	createdAt      time.Time
	lastAccess     time.Time
}

// responseLink lets a Responses request that carries previous_response_id
// continue the conversation of the exchange that produced that response.
type responseLink struct {
	exchangeID  string
	sessionID   string
	depth       int
	position    string
	model       string
	toolsDigest string
}

// exchangeRecord retains an exchange's placement and request facts so a later
// response observation can register a response link for it.
type exchangeRecord struct {
	assignment  Assignment
	sessionID   string
	model       string
	toolsDigest string
}

// Index assigns exchanges to sessions using the append-only position chain
// defined in docs/session-spec.md. It is a pure observation structure: the
// gateway feeds it facts derived from original inbound bytes, and it returns
// placements that ride on snapshots. Nothing it computes is transport input.
type Index struct {
	mu           sync.Mutex
	maxPositions int

	positions map[string]*position
	sessions  map[string]*sessionRecord
	responses map[string]*responseLink
	exchanges map[string]*exchangeRecord

	sessionSeq int
}

// NewIndex constructs a session index. maxPositions <= 0 means the default.
func NewIndex(maxPositions int) *Index {
	if maxPositions <= 0 {
		maxPositions = DefaultMaxPositions
	}
	return &Index{
		maxPositions: maxPositions,
		positions:    make(map[string]*position),
		sessions:     make(map[string]*sessionRecord),
		responses:    make(map[string]*responseLink),
		exchanges:    make(map[string]*exchangeRecord),
	}
}

// Assign places one request in a session tree and returns its placement.
// The exchange id must be unique for the lifetime of the index.
func (ix *Index) Assign(exchangeID string, facts RequestFacts) Assignment {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := time.Now().UTC()
	if facts.PreviousResponseID != "" && facts.Protocol == inspection.ProtocolResponses {
		return ix.assignByResponseLocked(exchangeID, facts, now)
	}
	hashes := chainHashes(facts.Protocol, facts.MessageDigests)
	deepest := -1
	var parent *position
	for i, hash := range hashes {
		if p, ok := ix.positions[hash]; ok {
			deepest = i
			parent = p
		}
	}
	switch {
	case deepest == len(hashes)-1:
		// The request's complete history is already a registered tip: a
		// rollout sibling (identical context resent).
		return ix.assignRepeatLocked(exchangeID, facts, hashes[len(hashes)-1], now)
	case deepest >= 0:
		return ix.assignContinueLocked(exchangeID, facts, hashes, deepest, parent, now)
	default:
		return ix.assignRootLocked(exchangeID, facts, hashes, now)
	}
}

// NoteResponseID registers a response identifier so a later request that
// carries it as previous_response_id continues this exchange's session.
func (ix *Index) NoteResponseID(exchangeID, responseID string) {
	if responseID == "" {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	record, ok := ix.exchanges[exchangeID]
	if !ok {
		return
	}
	ix.responses[responseID] = &responseLink{
		exchangeID:  exchangeID,
		sessionID:   record.sessionID,
		depth:       record.assignment.Depth,
		position:    record.assignment.Position,
		model:       record.model,
		toolsDigest: record.toolsDigest,
	}
}

// Stats is a metadata-only diagnostic for tests and operators.
func (ix *Index) Stats() (positions, sessions int) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.positions), len(ix.sessions)
}

func (ix *Index) assignRootLocked(exchangeID string, facts RequestFacts, hashes []string, now time.Time) Assignment {
	sessionID := ix.newSessionIDLocked(facts.Protocol, exchangeID, now)
	assignment := Assignment{SessionID: sessionID, Depth: 1, Root: true}
	if len(hashes) > 0 {
		assignment.Position = hashes[len(hashes)-1]
		ix.registerTipLocked(sessionID, 1, exchangeID, facts, hashes[len(hashes)-1])
	}
	ix.recordExchangeLocked(exchangeID, assignment, facts)
	ix.evictIfNeededLocked()
	return assignment
}

func (ix *Index) assignContinueLocked(exchangeID string, facts RequestFacts, hashes []string, deepest int, parent *position, now time.Time) Assignment {
	ix.touchSessionLocked(parent.sessionID, now)
	depth := parent.depth + 1
	// The parent tip already observed a different successor: this turn opens
	// a fork branch.
	fork := len(parent.successors) > 0
	tipHash := hashes[len(hashes)-1]
	parent.successors[tipHash] = struct{}{}
	assignment := Assignment{
		SessionID:        parent.sessionID,
		Depth:            depth,
		Position:         tipHash,
		ParentPosition:   hashes[deepest],
		ParentExchangeID: parent.ownerExchangeID,
		Fork:             fork,
		ModelChanged:     facts.Model != parent.model,
		ToolsChanged:     facts.ToolsDigest != parent.toolsDigest,
	}
	ix.registerTipLocked(parent.sessionID, depth, exchangeID, facts, tipHash)
	ix.recordExchangeLocked(exchangeID, assignment, facts)
	ix.evictIfNeededLocked()
	return assignment
}

func (ix *Index) assignRepeatLocked(exchangeID string, facts RequestFacts, tipHash string, now time.Time) Assignment {
	p := ix.positions[tipHash]
	ix.touchSessionLocked(p.sessionID, now)
	assignment := Assignment{
		SessionID:    p.sessionID,
		Depth:        p.depth,
		Position:     tipHash,
		RepeatIndex:  len(p.tipExchangeIDs),
		ModelChanged: facts.Model != p.model,
		ToolsChanged: facts.ToolsDigest != p.toolsDigest,
	}
	// A repeat of a known request is a rollout sibling: it shares the tree
	// parent of the exchange that first occupied this position.
	if len(p.tipExchangeIDs) > 0 {
		if original, ok := ix.exchanges[p.tipExchangeIDs[0]]; ok {
			assignment.ParentPosition = original.assignment.ParentPosition
			assignment.ParentExchangeID = original.assignment.ParentExchangeID
		}
	}
	p.tipExchangeIDs = append(p.tipExchangeIDs, exchangeID)
	ix.recordExchangeLocked(exchangeID, assignment, facts)
	return assignment
}

func (ix *Index) assignByResponseLocked(exchangeID string, facts RequestFacts, now time.Time) Assignment {
	tipHash := responsePositionHash(facts.Protocol, facts.PreviousResponseID)
	if link, ok := ix.responses[facts.PreviousResponseID]; ok {
		if _, exists := ix.positions[tipHash]; exists {
			// A sibling already continued this response: a rollout of it.
			return ix.assignRepeatLocked(exchangeID, facts, tipHash, now)
		}
		ix.touchSessionLocked(link.sessionID, now)
		assignment := Assignment{
			SessionID:        link.sessionID,
			Depth:            link.depth + 1,
			Position:         tipHash,
			ParentPosition:   link.position,
			ParentExchangeID: link.exchangeID,
			ModelChanged:     facts.Model != link.model,
			ToolsChanged:     facts.ToolsDigest != link.toolsDigest,
		}
		ix.registerTipLocked(link.sessionID, link.depth+1, exchangeID, facts, tipHash)
		ix.recordExchangeLocked(exchangeID, assignment, facts)
		ix.evictIfNeededLocked()
		return assignment
	}
	// Unknown previous_response_id: the conversation is foreign or was
	// evicted; this request becomes a fresh session root at a stable position
	// derived from the response id.
	assignment := Assignment{SessionID: ix.newSessionIDLocked(facts.Protocol, exchangeID, now), Depth: 1, Root: true, Position: tipHash}
	ix.registerTipLocked(assignment.SessionID, 1, exchangeID, facts, tipHash)
	ix.recordExchangeLocked(exchangeID, assignment, facts)
	ix.evictIfNeededLocked()
	return assignment
}

// registerTipLocked registers one request tip as a position. Intermediate
// message prefixes are deliberately not registered: the next turn of an
// append-only conversation always extends the previous turn's tip, so
// tip-to-tip edges are both sufficient for matching and correct for turn
// depth and fork attribution.
func (ix *Index) registerTipLocked(sessionID string, depth int, exchangeID string, facts RequestFacts, tipHash string) {
	if existing, ok := ix.positions[tipHash]; ok {
		// The tip is already registered; the first owner stays authoritative.
		if !containsString(existing.tipExchangeIDs, exchangeID) {
			existing.tipExchangeIDs = append(existing.tipExchangeIDs, exchangeID)
		}
		return
	}
	ix.positions[tipHash] = &position{
		sessionID:       sessionID,
		depth:           depth,
		ownerExchangeID: exchangeID,
		tipExchangeIDs:  []string{exchangeID},
		model:           facts.Model,
		toolsDigest:     facts.ToolsDigest,
		successors:      make(map[string]struct{}),
	}
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (ix *Index) recordExchangeLocked(exchangeID string, assignment Assignment, facts RequestFacts) {
	ix.exchanges[exchangeID] = &exchangeRecord{
		assignment:  assignment,
		sessionID:   assignment.SessionID,
		model:       facts.Model,
		toolsDigest: facts.ToolsDigest,
	}
}

func (ix *Index) newSessionIDLocked(protocol inspection.Protocol, exchangeID string, now time.Time) string {
	ix.sessionSeq++
	sessionID := fmt.Sprintf("sess-%d-%d", now.UnixNano(), ix.sessionSeq)
	ix.sessions[sessionID] = &sessionRecord{
		protocol:       protocol,
		rootExchangeID: exchangeID,
		createdAt:      now,
		lastAccess:     now,
	}
	return sessionID
}

func (ix *Index) touchSessionLocked(sessionID string, now time.Time) {
	if record, ok := ix.sessions[sessionID]; ok {
		record.lastAccess = now
	}
}

// evictIfNeededLocked removes least-recently-active sessions wholesale until
// the position table is back under its bound. Subsequent requests for an
// evicted session become fresh roots by design.
func (ix *Index) evictIfNeededLocked() {
	if len(ix.positions) <= ix.maxPositions {
		return
	}
	type candidate struct {
		id         string
		lastAccess time.Time
	}
	candidates := make([]candidate, 0, len(ix.sessions))
	for id, record := range ix.sessions {
		candidates = append(candidates, candidate{id: id, lastAccess: record.lastAccess})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].lastAccess.Equal(candidates[j].lastAccess) {
			return candidates[i].id < candidates[j].id
		}
		return candidates[i].lastAccess.Before(candidates[j].lastAccess)
	})
	target := ix.maxPositions * 9 / 10
	for _, c := range candidates {
		if len(ix.positions) <= target {
			break
		}
		ix.evictSessionLocked(c.id)
	}
}

func (ix *Index) evictSessionLocked(sessionID string) {
	for hash, p := range ix.positions {
		if p.sessionID == sessionID {
			delete(ix.positions, hash)
		}
	}
	for responseID, link := range ix.responses {
		if link.sessionID == sessionID {
			delete(ix.responses, responseID)
		}
	}
	for exchangeID, record := range ix.exchanges {
		if record.sessionID == sessionID {
			delete(ix.exchanges, exchangeID)
		}
	}
	delete(ix.sessions, sessionID)
}

// chainHashes walks the message digests into position hashes. The first hash
// domains the protocol, so positions of different protocols can never match.
func chainHashes(protocol inspection.Protocol, digests []string) []string {
	if len(digests) == 0 {
		return []string{hashJoin(positionDomain, string(protocol), "")}
	}
	hashes := make([]string, len(digests))
	hashes[0] = hashJoin(positionDomain, string(protocol), digests[0])
	for i := 1; i < len(digests); i++ {
		hashes[i] = hashJoin(positionDomain, "chain", hashes[i-1], digests[i])
	}
	return hashes
}

// responsePositionHash derives a stable position for a previous_response_id
// continuation.
func responsePositionHash(protocol inspection.Protocol, responseID string) string {
	return hashJoin(positionDomain, string(protocol), "resp", responseID)
}

// hashJoin hashes length-prefixed parts so no concatenation ambiguity exists.
func hashJoin(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
