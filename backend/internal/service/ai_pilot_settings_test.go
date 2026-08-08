package service

import (
	"encoding/json"
	"testing"
)

func TestDefaultOpPointersAreDistinct(t *testing.T) {
	t.Parallel()
	d := DefaultAIAutopilotSettings()
	if d.OpDisable == d.OpEnable || d.OpDisable == d.OpSetWeight {
		t.Fatal("default op pointers must not alias")
	}
	*d.OpDisable = false
	if !*d.OpEnable {
		t.Fatal("toggling OpDisable must not flip OpEnable")
	}
}

func TestNormalizePreservesFalseOpSwitches(t *testing.T) {
	t.Parallel()
	off := false
	on := true
	s := AIAutopilotSettings{
		OpDisable: &off,
		OpEnable:  &on,
	}.Normalize()
	if s.OpDisable == nil || *s.OpDisable {
		t.Fatalf("op_disable should stay false, got %v", s.OpDisable)
	}
	if s.OpEnable == nil || !*s.OpEnable {
		t.Fatalf("op_enable should stay true")
	}
	// Normalize must re-allocate (no alias with input)
	if s.OpDisable == &off {
		t.Fatal("Normalize should return fresh pointers")
	}
}

func TestJSONRoundTripOpSwitchesIndependent(t *testing.T) {
	t.Parallel()
	// Simulate the old bug path: start from Default (now fixed) and from zero value.
	in := DefaultAIAutopilotSettings()
	*in.OpDisable = false
	*in.OpSetWeight = false
	*in.OpEnable = true
	raw, err := json.Marshal(in.Normalize())
	if err != nil {
		t.Fatal(err)
	}

	// Zero-value start (Get path)
	var loaded AIAutopilotSettings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	loaded = loaded.Normalize()
	if *loaded.OpDisable {
		t.Fatal("loaded op_disable should be false")
	}
	if *loaded.OpSetWeight {
		t.Fatal("loaded op_set_weight should be false")
	}
	if !*loaded.OpEnable {
		t.Fatal("loaded op_enable should be true")
	}

	// Default-value start (old buggy Get path — must still work after distinct pointers)
	loaded2 := DefaultAIAutopilotSettings()
	if err := json.Unmarshal(raw, &loaded2); err != nil {
		t.Fatal(err)
	}
	loaded2 = loaded2.Normalize()
	if *loaded2.OpDisable {
		t.Fatal("default-start load: op_disable should be false")
	}
	if *loaded2.OpSetWeight {
		t.Fatal("default-start load: op_set_weight should be false")
	}
	if !*loaded2.OpEnable {
		t.Fatal("default-start load: op_enable should be true")
	}
}

func TestOpAllowedRespectsFalse(t *testing.T) {
	t.Parallel()
	cfg := DefaultAIAutopilotSettings()
	*cfg.OpDisable = false
	cfg = cfg.Normalize()
	if cfg.OpAllowed(AIOpDisable) {
		t.Fatal("disable should be denied")
	}
	if !cfg.OpAllowed(AIOpSetPriority) {
		t.Fatal("priority should still be allowed")
	}
}
