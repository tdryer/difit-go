package collate

import (
	"slices"
	"testing"
)

func TestCompareSortsLikeLocaleCompare(t *testing.T) {
	got := []string{"feature/b", "Zeta", "alpha", "a", "A", "1.0", "_x", "-y", "feature-a", "feature_a"}
	slices.SortStableFunc(got, Compare)
	want := []string{"_x", "-y", "1.0", "a", "A", "alpha", "feature_a", "feature-a", "feature/b", "Zeta"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCompareISOTimestamps(t *testing.T) {
	if Compare("2024-01-01T00:00:00.000Z", "2024-01-02T00:00:00.000Z") >= 0 {
		t.Fatal("expected earlier timestamp first")
	}
	if Compare("x", "x") != 0 {
		t.Fatal("expected equal")
	}
}
