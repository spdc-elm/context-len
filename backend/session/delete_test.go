package session

import (
	"testing"

	"context-lens/backend/inspection"
)

func TestDeleteSessionRemovesTreePositionsAndResponseLinks(t *testing.T) {
	ix := NewIndex(0)
	first := ix.Assign("e1", RequestFacts{Protocol: inspection.ProtocolResponses, MessageDigests: []string{"a"}})
	second := ix.Assign("e2", RequestFacts{Protocol: inspection.ProtocolResponses, MessageDigests: []string{"a", "b"}})
	if first.SessionID != second.SessionID {
		t.Fatalf("tree split: %q %q", first.SessionID, second.SessionID)
	}
	ix.NoteResponseID("e2", "resp-old")
	ix.DeleteSession(first.SessionID)
	positions, sessions := ix.Stats()
	if positions != 0 || sessions != 0 {
		t.Fatalf("stats after delete = %d,%d", positions, sessions)
	}
	fresh := ix.Assign("e3", RequestFacts{Protocol: inspection.ProtocolResponses, PreviousResponseID: "resp-old"})
	if !fresh.Root || fresh.SessionID == first.SessionID {
		t.Fatalf("stale response link survived: %+v", fresh)
	}
	ix.DeleteSession(first.SessionID) // idempotent
}
