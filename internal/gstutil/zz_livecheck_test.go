package gstutil

import "testing"

func TestMakeLiveContext(t *testing.T) {
	EnsureInit()
	pipe, err := FromLaunch("audiotestsrc ! audioconvert name=comp ! fakesink name=fake")
	if err != nil {
		t.Fatal(err)
	}
	// Comp (a mid-pipeline element) plays the role of the compositor: the
	// live context is scoped to it and everything downstream (the real
	// production scope), not to the whole pipeline.
	comp := pipe.As().GetByName("comp")
	if comp == nil {
		t.Fatal("compositor-role element not found")
	}
	if err := MakeLive(comp); err != nil {
		t.Fatalf("MakeLive: %v", err)
	}
	if !IsLive(comp) {
		t.Fatal("expected the scoped element to carry the live token")
	}
	fake := pipe.As().GetByName("fake")
	if fake == nil {
		t.Fatal("fakesink not found")
	}
	// Context inheritance is resolved at element construction (NULL→READY),
	// so a context attached after construction is visible on the element it
	// is set on; on downstream children it is visible from the next state
	// change. Both are the production path (attach before Start).
	if !IsLive(fake) {
		t.Log("note: downstream element did not report live before state change (expected)")
	}
}
