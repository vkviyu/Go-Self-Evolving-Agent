package wasm

import (
	"errors"
	"fmt"
	"sync"
)

var (
	errCallNotFound  = errors.New("WASM call is not active")
	errBlobNotFound  = errors.New("blob handle is not owned by this call")
	errHostOwned     = errors.New("guest cannot drop a host-owned blob")
	errNotGuestOwned = errors.New("WASM response handle is not guest-owned")
	errBlobLimit     = errors.New("blob quota exceeded")
)

type blobOwner uint8

const (
	ownerHost blobOwner = iota + 1
	ownerGuest
)

type blob struct {
	owner blobOwner
	data  []byte
}

type callBlobs struct {
	nextHandle uint64
	totalBytes uint64
	blobs      map[uint64]blob
}

// blobStore scopes opaque handles to one module instance/call. Handles from a
// previous call or a different plugin instance are never accepted.
type blobStore struct {
	mu           sync.RWMutex
	calls        map[string]*callBlobs
	maxBlobBytes uint64
	maxCallBytes uint64
}

func newBlobStore(maxBlobBytes, maxCallBytes uint64) *blobStore {
	return &blobStore{
		calls:        make(map[string]*callBlobs),
		maxBlobBytes: maxBlobBytes,
		maxCallBytes: maxCallBytes,
	}
}

func (s *blobStore) begin(callKey string, request []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.calls[callKey]; exists {
		return 0, fmt.Errorf("duplicate WASM call key %q", callKey)
	}
	call := &callBlobs{nextHandle: 1, blobs: make(map[uint64]blob)}
	s.calls[callKey] = call
	handle, err := s.putLocked(call, ownerHost, request)
	if err != nil {
		delete(s.calls, callKey)
		return 0, err
	}
	return handle, nil
}

func (s *blobStore) end(callKey string) {
	s.mu.Lock()
	delete(s.calls, callKey)
	s.mu.Unlock()
}

func (s *blobStore) length(callKey string, handle uint64) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call, ok := s.calls[callKey]
	if !ok {
		return 0, errCallNotFound
	}
	value, ok := call.blobs[handle]
	if !ok {
		return 0, errBlobNotFound
	}
	return uint64(len(value.data)), nil
}

func (s *blobStore) get(callKey string, handle uint64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call, ok := s.calls[callKey]
	if !ok {
		return nil, errCallNotFound
	}
	value, ok := call.blobs[handle]
	if !ok {
		return nil, errBlobNotFound
	}
	return append([]byte(nil), value.data...), nil
}

func (s *blobStore) getGuest(callKey string, handle uint64) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	call, ok := s.calls[callKey]
	if !ok {
		return nil, errCallNotFound
	}
	value, ok := call.blobs[handle]
	if !ok {
		return nil, errBlobNotFound
	}
	if value.owner != ownerGuest {
		return nil, errNotGuestOwned
	}
	return append([]byte(nil), value.data...), nil
}

func (s *blobStore) putGuest(callKey string, data []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.calls[callKey]
	if !ok {
		return 0, errCallNotFound
	}
	return s.putLocked(call, ownerGuest, data)
}

func (s *blobStore) putLocked(call *callBlobs, owner blobOwner, data []byte) (uint64, error) {
	length := uint64(len(data))
	if length > s.maxBlobBytes || call.totalBytes+length > s.maxCallBytes {
		return 0, errBlobLimit
	}
	handle := call.nextHandle
	call.nextHandle++
	if handle == 0 {
		return 0, errors.New("blob handle space exhausted")
	}
	call.blobs[handle] = blob{owner: owner, data: append([]byte(nil), data...)}
	call.totalBytes += length
	return handle, nil
}

func (s *blobStore) dropGuest(callKey string, handle uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.calls[callKey]
	if !ok {
		return errCallNotFound
	}
	value, ok := call.blobs[handle]
	if !ok {
		return errBlobNotFound
	}
	if value.owner != ownerGuest {
		return errHostOwned
	}
	call.totalBytes -= uint64(len(value.data))
	delete(call.blobs, handle)
	return nil
}
