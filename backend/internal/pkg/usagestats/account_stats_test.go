package usagestats

import "testing"

func TestComputeCacheHitRate(t *testing.T) {
	t.Parallel()
	if got := ComputeCacheHitRate(0, 0, 0); got != nil {
		t.Fatalf("empty prompt must be nil, got %v", *got)
	}
	got := ComputeCacheHitRate(60, 10, 30)
	if got == nil {
		t.Fatal("expected rate")
	}
	// 30 / (60+10+30) = 30%
	if *got < 29.999 || *got > 30.001 {
		t.Fatalf("got %v want 30", *got)
	}
	full := ComputeCacheHitRate(0, 0, 100)
	if full == nil || *full != 100 {
		t.Fatalf("all-cache should be 100, got %v", full)
	}
}
