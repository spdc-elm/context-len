package session

import (
	"strconv"
	"strings"
	"testing"

	"context-lens/backend/inspection"
)

func chatFacts(messages ...string) RequestFacts {
	digests := make([]string, len(messages))
	for i, message := range messages {
		digests[i] = nodeDigest(mustJSON(`{"content":` + strconv.Quote(message) + `}`))
	}
	return RequestFacts{Protocol: inspection.ProtocolChatCompletions, MessageDigests: digests, Model: "m", ToolsDigest: ""}
}

func mustJSON(text string) *inspection.JSONNode {
	return inspection.InspectJSON([]byte(text)).Root
}

// TestAssignChainsAndForks walks the canonical lifecycle: a root turn, an
// append-only continuation, a fork from a shared prefix, and a rollout
// (identical context resent).
func TestAssignChainsAndForks(t *testing.T) {
	ix := NewIndex(0)

	root := ix.Assign("r1", chatFacts("s", "u1"))
	if !root.Root || root.Depth != 1 || root.SessionID == "" {
		t.Fatalf("root assignment = %+v", root)
	}

	// Turn 2 extends the same history.
	turn2 := ix.Assign("r2", chatFacts("s", "u1", "a1", "t1"))
	if turn2.SessionID != root.SessionID || turn2.Depth != 2 {
		t.Fatalf("continuation = %+v", turn2)
	}
	if turn2.ParentExchangeID != "r1" || turn2.RepeatIndex != 0 || turn2.Fork {
		t.Fatalf("continuation parent/flags = %+v", turn2)
	}

	// Turn 3 extends turn 2.
	turn3 := ix.Assign("r3", chatFacts("s", "u1", "a1", "t1", "a2", "u2"))
	if turn3.Depth != 3 || turn3.ParentExchangeID != "r2" {
		t.Fatalf("turn3 = %+v", turn3)
	}

	// A fork from the turn-2 state with a different continuation.
	fork := ix.Assign("f1", chatFacts("s", "u1", "a1", "t1", "a2'", "u2'"))
	if !fork.Fork || fork.SessionID != root.SessionID || fork.Depth != 3 {
		t.Fatalf("fork = %+v", fork)
	}
	if fork.ParentExchangeID != "r2" {
		t.Fatalf("fork parent = %s, want r2 (owner of the fork point)", fork.ParentExchangeID)
	}

	// A rollout: the exact turn-2 context resent.
	rollout := ix.Assign("r2b", chatFacts("s", "u1", "a1", "t1"))
	if rollout.SessionID != root.SessionID || rollout.Depth != 2 || rollout.RepeatIndex != 1 {
		t.Fatalf("rollout = %+v", rollout)
	}
	if rollout.ParentExchangeID != turn2.ParentExchangeID || rollout.ParentPosition != turn2.ParentPosition {
		t.Fatalf("rollout parent = %+v, want turn2's parent", rollout)
	}

	// A second rollout forms a group of three at that position.
	if again := ix.Assign("r2c", chatFacts("s", "u1", "a1", "t1")); again.RepeatIndex != 2 {
		t.Fatalf("second rollout repeat index = %d", again.RepeatIndex)
	}
}

// TestAssignNewSessionOnPrefixBreak covers history rewrites: a request that
// no longer extends any known prefix becomes a fresh session.
func TestAssignNewSessionOnPrefixBreak(t *testing.T) {
	ix := NewIndex(0)
	root := ix.Assign("r1", chatFacts("s", "u1"))
	// Compaction: the first message is rewritten, so no position matches.
	compacted := ix.Assign("c1", chatFacts("s'", "u1", "a1"))
	if compacted.SessionID == root.SessionID || !compacted.Root {
		t.Fatalf("compaction = %+v, want a fresh session", compacted)
	}
}

// TestAssignProtocolIsolation checks that identical message digests under
// different protocols never join one session.
func TestAssignProtocolIsolation(t *testing.T) {
	ix := NewIndex(0)
	chat := chatFacts("s", "u1")
	anthropic := chatFacts("s", "u1")
	anthropic.Protocol = inspection.ProtocolAnthropicMessages
	first := ix.Assign("chat-1", chat)
	second := ix.Assign("anthropic-1", anthropic)
	if first.SessionID == second.SessionID {
		t.Fatalf("cross-protocol match: %+v vs %+v", first, second)
	}
}

// TestAssignSoftSignals verifies model and tool changes are turn-level
// boundary markers, not chain breaks.
func TestAssignSoftSignals(t *testing.T) {
	ix := NewIndex(0)
	base := chatFacts("s", "u1")
	base.Model = "model-a"
	base.ToolsDigest = "tools-1"
	ix.Assign("r1", base)

	switched := chatFacts("s", "u1", "a1")
	switched.Model = "model-b"
	switched.ToolsDigest = "tools-2"
	assignment := ix.Assign("r2", switched)
	if !assignment.ModelChanged || !assignment.ToolsChanged {
		t.Fatalf("soft signals missing: %+v", assignment)
	}
	if assignment.SessionID != ix.Assign("r3", func() RequestFacts {
		f := chatFacts("s", "u1", "a1")
		f.Model = "model-b"
		f.ToolsDigest = "tools-2"
		return f
	}()).SessionID {
		t.Fatalf("model/tool switch must not break the session")
	}
}

// TestAssignByResponseID covers the Responses server-side state chain:
// a request with previous_response_id continues the exchange that produced
// that response, unknown ids start fresh sessions, and repeats roll out.
func TestAssignByResponseID(t *testing.T) {
	ix := NewIndex(0)
	first := RequestFacts{Protocol: inspection.ProtocolResponses, Model: "m"}
	firstAssign := ix.Assign("resp-1", first)
	ix.NoteResponseID("resp-1", "resp_abc")

	follow := RequestFacts{Protocol: inspection.ProtocolResponses, Model: "m", PreviousResponseID: "resp_abc"}
	second := ix.Assign("resp-2", follow)
	if second.SessionID != firstAssign.SessionID || second.Depth != 2 {
		t.Fatalf("response continuation = %+v", second)
	}
	if second.ParentExchangeID != "resp-1" || second.ParentPosition != firstAssign.Position {
		t.Fatalf("response continuation parent = %+v", second)
	}
	ix.NoteResponseID("resp-2", "resp_def")

	third := ix.Assign("resp-3", RequestFacts{Protocol: inspection.ProtocolResponses, Model: "m", PreviousResponseID: "resp_def"})
	if third.Depth != 3 || third.SessionID != firstAssign.SessionID {
		t.Fatalf("second continuation = %+v", third)
	}

	// The same continuation point resent is a rollout sibling.
	repeat := ix.Assign("resp-3b", RequestFacts{Protocol: inspection.ProtocolResponses, Model: "m", PreviousResponseID: "resp_def"})
	if repeat.RepeatIndex != 1 || repeat.Depth != 3 || repeat.SessionID != firstAssign.SessionID {
		t.Fatalf("response rollout = %+v", repeat)
	}

	// Unknown response ids become fresh roots.
	foreign := ix.Assign("resp-x", RequestFacts{Protocol: inspection.ProtocolResponses, Model: "m", PreviousResponseID: "resp_unknown"})
	if !foreign.Root || foreign.SessionID == firstAssign.SessionID {
		t.Fatalf("foreign continuation = %+v", foreign)
	}
}

// TestIndexEviction exercises whole-session LRU eviction: evicted sessions
// disappear from the table and their follow-ups become fresh roots.
func TestIndexEviction(t *testing.T) {
	ix := NewIndex(12)
	a := ix.Assign("a1", chatFacts("s", "u1"))
	ix.Assign("a2", chatFacts("s", "u1", "a1"))
	ix.Assign("a3", chatFacts("s", "u1", "a1", "t1"))
	// Later sessions with fresh histories push the idle first session past
	// the position cap, evicting it wholesale. Tips-only registration means
	// one position per turn, so ten extra single-turn sessions are needed.
	for i := 0; i < 10; i++ {
		prefix := string(rune('a' + i))
		ix.Assign(prefix, chatFacts(prefix+"-1", prefix+"-2", prefix+"-3"))
	}
	positions, _ := ix.Stats()
	if positions > 12 {
		t.Fatalf("positions = %d, want <= 12", positions)
	}
	after := ix.Assign("a4", chatFacts("s", "u1", "a1", "t1", "a2"))
	if after.SessionID == a.SessionID {
		t.Fatalf("evicted session still matched: %+v", after)
	}
}

// TestChainHashesDomainSeparation guards against accidental position
// collisions between chains of different protocols or lengths.
func TestChainHashesDomainSeparation(t *testing.T) {
	chat := chainHashes(inspection.ProtocolChatCompletions, []string{"d1", "d2"})
	anthropic := chainHashes(inspection.ProtocolAnthropicMessages, []string{"d1", "d2"})
	short := chainHashes(inspection.ProtocolChatCompletions, []string{"d1"})
	// A prefix is itself a position: the first hash of a longer chain equals
	// the hash of the truncated chain.
	if chat[0] != short[0] {
		t.Fatalf("prefix identity broken")
	}
	seen := map[string]bool{}
	for _, hash := range append([]string{chat[1], short[0]}, anthropic...) {
		if seen[hash] {
			t.Fatalf("unexpected position hash collision: %s", hash)
		}
		seen[hash] = true
	}
	if chainHashes(inspection.ProtocolChatCompletions, nil)[0] == "" {
		t.Fatalf("empty digests must still hash")
	}
}

// TestCanonicalDigestStability pins the canonical-serialization contract:
// key order and escape spelling drift must not change a message digest,
// while numeric spelling stays significant.
func TestCanonicalDigestStability(t *testing.T) {
	same := []string{
		`{"role":"user","content":"hi"}`,
		`{"content":"hi","role":"user"}`,
		`{ "role" : "user" , "content" : "hi" }`,
	}
	base := nodeDigest(mustJSON(same[0]))
	for _, variant := range same[1:] {
		if got := nodeDigest(mustJSON(variant)); got != base {
			t.Fatalf("digest drifted for %s", variant)
		}
	}
	// Escape-spelling drift normalizes to the same digest.
	if got := nodeDigest(mustJSON(`{"role":"user","content":"é"}`)); got != nodeDigest(mustJSON(`{"role":"user","content":"\u00e9"}`)) {
		t.Fatalf("escape spelling changed the digest")
	}
	// Numeric spelling stays significant by design.
	if nodeDigest(mustJSON(`{"n":1}`)) == nodeDigest(mustJSON(`{"n":1.0}`)) {
		t.Fatalf("numeric spelling must stay significant")
	}
	// Control characters are fixed-width escaped.
	if !strings.Contains(quoteJSON("a\x01b"), `\u0001`) {
		t.Fatalf("control escape = %q", quoteJSON("a\x01b"))
	}
}

// TestAnalyzeRequestFacts checks the analysis seam: digests, tools digest,
// and previous_response_id extraction from realistic bodies.
func TestAnalyzeRequestFacts(t *testing.T) {
	body := []byte(`{"model":"m","previous_response_id":"resp_1","instructions":"be brief","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"lookup"}]}`)
	analysis := AnalyzeRequest(inspection.ProtocolResponses, body)
	if analysis.PreviousResponseID != "resp_1" {
		t.Fatalf("previous_response_id = %q", analysis.PreviousResponseID)
	}
	if len(analysis.MessageDigests) != 2 {
		t.Fatalf("digests = %d, want 2 (instructions + input item)", len(analysis.MessageDigests))
	}
	if analysis.ToolsDigest == "" || analysis.ToolsDigest == nodeDigest(nil) {
		t.Fatalf("tools digest missing")
	}
	if analysis.Summary.Model != "m" || analysis.Summary.MessageCount != 2 {
		t.Fatalf("summary = %+v", analysis.Summary)
	}
}
