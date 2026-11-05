package indexer

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
)

// DedupKeyForFilter returns the identity of a filter notification, or "" if
// it cannot be established (the caller then delivers without deduping).
//
//   - logs: blockHash + txHash + logIndex, all required. The removed flag is
//     not part of the key but the state DedupWindow tracks per key, so
//     add, remove and re-add are each delivered once.
//   - newPendingTransactions: the tx hash, from a string or an object's hash.
//
// Keys are lowercased because hex on the wire is case-insensitive.
func DedupKeyForFilter(subType string, result json.RawMessage) string {
	key, _ := dedupKeyAndBlock(subType, result)
	return key
}

// dedupKeyAndBlock is DedupKeyForFilter plus the log's block number (0 when
// absent or not a log).
func dedupKeyAndBlock(subType string, result json.RawMessage) (string, int64) {
	switch subType {
	case SubTypeLogs:
		var log struct {
			BlockHash   string `json:"blockHash"`
			BlockNumber string `json:"blockNumber"`
			TxHash      string `json:"transactionHash"`
			LogIndex    string `json:"logIndex"`
		}
		if err := common.SonicCfg.Unmarshal(result, &log); err != nil {
			return "", 0
		}
		if log.BlockHash == "" || log.TxHash == "" || log.LogIndex == "" {
			return "", 0
		}
		num, _ := common.HexToInt64(log.BlockNumber)
		return strings.ToLower(log.BlockHash + ":" + log.TxHash + ":" + log.LogIndex), num
	case SubTypeNewPendingTransactions:
		var asString string
		if err := common.SonicCfg.Unmarshal(result, &asString); err == nil && asString != "" {
			return strings.ToLower(asString), 0
		}
		var asObj struct {
			Hash string `json:"hash"`
		}
		if err := common.SonicCfg.Unmarshal(result, &asObj); err == nil {
			return strings.ToLower(asObj.Hash), 0
		}
	}
	return "", 0
}

// logRemoved returns a log payload's "removed" flag, false if unparseable.
func logRemoved(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var probe struct {
		Removed bool `json:"removed"`
	}
	if err := common.SonicCfg.Unmarshal(payload, &probe); err != nil {
		return false
	}
	return probe.Removed
}

// dedupAgeBlocks is how far behind the newest block a window keeps keys.
// It sets the upstream lag (in blocks) the window absorbs when blocks are
// sparse; the size cap bounds it when they are full.
const dedupAgeBlocks = 64

type dedupKey [16]byte

func hashDedupKey(key string) dedupKey {
	sum := sha256.Sum256([]byte(key))
	var k dedupKey
	copy(k[:], sum[:16])
	return k
}

type dedupEntry struct {
	key   dedupKey
	block int64
}

// DedupWindow is a bounded FIFO map from key to its last delivered removed
// state. It is safe for concurrent use. Keys are stored as 16-byte hashes,
// so the window never holds the notification buffer a key was decoded from.
//
// A key leaves the window when the window is over capacity, or when its
// block is more than dedupAgeBlocks behind the newest block seen. The
// capacity can follow the protocol: sizeFn, when set, is read on every
// Mark, so a logs window can hold two blocks' worth of the most logs a block
// can carry, and two upstreams delivering the same full block out of step
// still collapse to one copy.
type DedupWindow struct {
	size   int
	sizeFn func() int

	mu     sync.Mutex
	state  map[dedupKey]bool
	order  []dedupEntry // FIFO
	closed bool
	// newest is the highest block number seen.
	newest int64
	// floor is the highest block number among evicted keys. A new key at
	// or below it may be a copy the window already forgot.
	floor int64
}

// NewDedupWindow returns a DedupWindow holding up to size keys, or
// DefaultDedupWindowSize if size is not positive.
func NewDedupWindow(size int) *DedupWindow {
	if size <= 0 {
		size = DefaultDedupWindowSize
	}
	// Grow on demand: a window preallocated to size costs about 560 KiB at
	// the default, for every distinct filter, even one that never matches.
	return &DedupWindow{
		size:  size,
		state: make(map[dedupKey]bool),
	}
}

// NewProtocolDedupWindow returns a window whose capacity is the larger of
// minSize and sizeFn(), read live.
func NewProtocolDedupWindow(minSize int, sizeFn func() int) *DedupWindow {
	w := NewDedupWindow(minSize)
	w.sizeFn = sizeFn
	return w
}

func (w *DedupWindow) capacity() int {
	if w.sizeFn != nil {
		if s := w.sizeFn(); s > w.size {
			return s
		}
	}
	return w.size
}

// Mark records key in the given removed state and reports whether it should
// be delivered (the key is new or its last delivered state differs) and how
// many keys the window gained (negative when evictions outnumber the add).
// A closed window delivers everything and holds nothing.
func (w *DedupWindow) Mark(key string, removed bool) (deliver bool, added int) {
	deliver, added, _ = w.MarkAt(key, removed, 0)
	return deliver, added
}

// MarkAt is Mark for a key from block `block` (0 if unknown). late reports a
// new key at or below the highest block the window has evicted keys from:
// it may be a second copy the window no longer remembers.
func (w *DedupWindow) MarkAt(key string, removed bool, block int64) (deliver bool, added int, late bool) {
	k := hashDedupKey(key)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return true, 0, false
	}
	if last, ok := w.state[k]; ok {
		if last == removed {
			return false, 0, false
		}
		w.state[k] = removed
		return true, 0, false
	}
	w.state[k] = removed
	w.order = append(w.order, dedupEntry{key: k, block: block})
	if block > w.newest {
		w.newest = block
	}

	size := w.capacity()
	evict := 0
	for evict < len(w.order) {
		e := w.order[evict]
		tooMany := len(w.order)-evict > size
		tooOld := e.block > 0 && e.block < w.newest-dedupAgeBlocks
		if !tooMany && !tooOld {
			break
		}
		if e.block > w.floor {
			w.floor = e.block
		}
		delete(w.state, e.key)
		evict++
	}
	if evict > 0 {
		w.order = w.order[evict:]
	}
	return true, 1 - evict, block > 0 && block <= w.floor
}

// Close empties the window for good and returns how many keys it held.
func (w *DedupWindow) Close() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(w.order)
	w.closed, w.state, w.order = true, nil, nil
	return n
}
