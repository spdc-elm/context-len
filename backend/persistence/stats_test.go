package persistence

import "testing"

func TestStatsExposeConfiguredMetadataLimits(t *testing.T) {
	s, err := NewStore(Config{MaxArtifactBytes: 11, MaxTotalBytes: 22, MaxMemoryBytes: 7, MaxArtifacts: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := s.Stats()
	if got.MaxArtifactBytes != 11 || got.MaxTotalBytes != 22 || got.MaxMemoryBytes != 7 || got.MaxArtifacts != 3 {
		t.Fatalf("stats limits = %+v", got)
	}
}
