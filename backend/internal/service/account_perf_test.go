package service

import "testing"

func TestGenerationTPS(t *testing.T) {
	t.Parallel()
	// 100 tokens in 2s generation after 500ms TTFB → 50 tok/s
	if got := GenerationTPS(100, 2500, 500); got < 49.9 || got > 50.1 {
		t.Fatalf("got %v want 50", got)
	}
	// no generation segment → fall back to whole duration
	if got := GenerationTPS(10, 1000, 1000); got < 9.9 || got > 10.1 {
		t.Fatalf("fallback got %v want 10", got)
	}
	if GenerationTPS(0, 2000, 200) != 0 {
		t.Fatal("zero output")
	}
	if GenerationTPS(10, 0, 0) != 0 {
		t.Fatal("zero duration")
	}
}
