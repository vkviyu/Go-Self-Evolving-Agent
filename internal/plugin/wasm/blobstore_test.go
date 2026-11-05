package wasm

import (
	"errors"
	"testing"
)

func TestBlobHandlesAreCallScopedAndHostOwnedInputCannotBeDropped(t *testing.T) {
	t.Parallel()

	store := newBlobStore(16, 32)
	handle, err := store.begin("call-1", []byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.dropGuest("call-1", handle); !errors.Is(err, errHostOwned) {
		t.Fatalf("drop host blob error = %v, want errHostOwned", err)
	}
	if _, err := store.getGuest("call-1", handle); !errors.Is(err, errNotGuestOwned) {
		t.Fatalf("use host blob as response error = %v, want errNotGuestOwned", err)
	}
	if _, err := store.get("call-2", handle); !errors.Is(err, errCallNotFound) {
		t.Fatalf("cross-call get error = %v, want errCallNotFound", err)
	}
	store.end("call-1")
	if _, err := store.get("call-1", handle); !errors.Is(err, errCallNotFound) {
		t.Fatalf("get after end error = %v, want errCallNotFound", err)
	}
}

func TestBlobStoreEnforcesPerBlobAndPerCallLimits(t *testing.T) {
	t.Parallel()

	store := newBlobStore(8, 10)
	if _, err := store.begin("oversized", make([]byte, 9)); !errors.Is(err, errBlobLimit) {
		t.Fatalf("begin error = %v, want errBlobLimit", err)
	}
	if _, err := store.begin("call", make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.putGuest("call", make([]byte, 5)); !errors.Is(err, errBlobLimit) {
		t.Fatalf("put error = %v, want errBlobLimit", err)
	}
}
