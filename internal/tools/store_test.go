package tools

import (
	"testing"
	"time"
)

func TestMemoryRoundTrip(t *testing.T) {
	store := New(time.Hour, nil)
	native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "c1"}
	store.RememberNativeCall("c1", native)
	got, ok := store.RememberedNativeCall("c1")
	if !ok || got["name"] != "run_officejs" {
		t.Fatalf("round trip failed: %v %v", got, ok)
	}
	if _, ok := store.RememberedNativeCall("missing"); ok {
		t.Fatal("missing key should not be found")
	}
	if _, ok := store.RememberedNativeCall(""); ok {
		t.Fatal("empty key should not be found")
	}
}

func TestMemoryEviction(t *testing.T) {
	store := New(time.Hour, nil)
	for i := 0; i < memoryCacheSize+50; i++ {
		store.RememberNativeCall(callN(i), map[string]any{"i": i})
	}
	store.mu.Lock()
	size := store.lru.Len()
	store.mu.Unlock()
	if size > memoryCacheSize {
		t.Fatalf("cache grew past limit: %d", size)
	}
	// oldest should be gone, newest present
	if _, ok := store.RememberedNativeCall(callN(0)); ok {
		t.Fatal("oldest entry should have been evicted")
	}
	if _, ok := store.RememberedNativeCall(callN(memoryCacheSize + 49)); !ok {
		t.Fatal("newest entry should be present")
	}
}

func TestKVKeyNormalization(t *testing.T) {
	if kvKey("call_ABC-1.2") != "call_ABC-1.2" {
		t.Fatal("safe key should be used verbatim")
	}
	weird := kvKey("call/with spaces")
	if len(weird) == 0 || weird == "call/with spaces" {
		t.Fatalf("unsafe key should be hashed: %s", weird)
	}
	if !safeKey.MatchString(weird) {
		t.Fatalf("hashed key must be safe: %s", weird)
	}
}

func callN(i int) string {
	return "c" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

func TestNilHostIsSafe(t *testing.T) {
	errs := 0
	store := New(time.Hour, func() { errs++ })
	store.RememberNativeCall("c1", map[string]any{"x": 1})
	if _, ok := store.RememberedNativeCall("c1"); !ok {
		t.Fatal("memory cache should work without host")
	}
	if errs != 0 {
		t.Fatalf("nil host must not trigger errors, got %d", errs)
	}
}
