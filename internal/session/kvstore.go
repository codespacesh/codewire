package session

import (
	"strings"
	"sync"
	"time"
)

// KVStore is an in-memory key-value store with namespace support and TTL.
type KVStore struct {
	mu   sync.RWMutex
	data map[string]map[string]kvEntry // namespace -> key -> entry
}

type kvEntry struct {
	value     []byte
	expiresAt *time.Time
	timer     *time.Timer
}

// NewKVStore creates a ready-to-use KV store.
func NewKVStore() *KVStore {
	return &KVStore{
		data: make(map[string]map[string]kvEntry),
	}
}

// Set stores a key-value pair in the given namespace with optional TTL.
func (kv *KVStore) Set(namespace, key string, value []byte, ttl time.Duration) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	ns, ok := kv.data[namespace]
	if !ok {
		ns = make(map[string]kvEntry)
		kv.data[namespace] = ns
	}

	// Cancel existing timer if any.
	if existing, exists := ns[key]; exists && existing.timer != nil {
		existing.timer.Stop()
	}

	entry := kvEntry{value: value}

	if ttl > 0 {
		expiresAt := time.Now().Add(ttl)
		entry.expiresAt = &expiresAt
		entry.timer = time.AfterFunc(ttl, func() {
			kv.Delete(namespace, key)
		})
	}

	ns[key] = entry
}

// Get retrieves a value by namespace and key. Returns nil if not found.
func (kv *KVStore) Get(namespace, key string) []byte {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	ns, ok := kv.data[namespace]
	if !ok {
		return nil
	}

	entry, ok := ns[key]
	if !ok {
		return nil
	}

	return entry.value
}

// Delete removes a key from the given namespace.
func (kv *KVStore) Delete(namespace, key string) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	ns, ok := kv.data[namespace]
	if !ok {
		return
	}

	if existing, exists := ns[key]; exists && existing.timer != nil {
		existing.timer.Stop()
	}

	delete(ns, key)

	if len(ns) == 0 {
		delete(kv.data, namespace)
	}
}

// KVEntry is the public type returned by List.
type KVEntry struct {
	Key       string
	Value     []byte
	ExpiresAt *time.Time
}

// List returns all entries in a namespace matching the given prefix.
func (kv *KVStore) List(namespace, prefix string) []KVEntry {
	kv.mu.RLock()
	defer kv.mu.RUnlock()

	ns, ok := kv.data[namespace]
	if !ok {
		return nil
	}

	var entries []KVEntry
	for key, entry := range ns {
		if prefix == "" || strings.HasPrefix(key, prefix) {
			entries = append(entries, KVEntry{
				Key:       key,
				Value:     entry.value,
				ExpiresAt: entry.expiresAt,
			})
		}
	}

	return entries
}
