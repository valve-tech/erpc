package indexer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
)

// DedupKeyForFilter returns a stable identifier for a filter-subscription
// notification payload, or "" when the payload cannot be parsed (caller
// should then fan out without deduping, since we can't prove it's a dupe).
//
//   - logs: blockHash + txHash + logIndex + removed flag (the removed
//     flag is part of the key so a reorg-out log isn't collapsed with its
//     matching reorg-in log — both are delivered to clients).
//   - newPendingTransactions: the tx hash, whether the upstream returned
//     a raw string or an object with a .hash field.
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
			Removed     bool   `json:"removed"`
		}
		if err := common.SonicCfg.Unmarshal(result, &log); err != nil {
			return "", 0
		}
		num, _ := common.HexToInt64(log.BlockNumber)
		return fmt.Sprintf("%s:%s:%s:%t", log.BlockHash, log.TxHash, log.LogIndex, log.Removed), num
	case SubTypeNewPendingTransactions:
		var asString string
		if err := common.SonicCfg.Unmarshal(result, &asString); err == nil && asString != "" {
			return asString, 0
		}
		var asObj struct {
			Hash string `json:"hash"`
		}
		if err := common.SonicCfg.Unmarshal(result, &asObj); err == nil {
			return asObj.Hash, 0
		}
	}
	return "", 0
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

// DedupWindow is a bounded FIFO seen-set. It is safe for concurrent use;
// callers typically hold one per (network, subType, paramsHash) fan-out
// group. Keys are stored as 16-byte hashes.
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

	mu    sync.Mutex
	seen  map[dedupKey]struct{}
	order []dedupEntry // FIFO
	// newest is the highest block number seen.
	newest int64
	// floor is the highest block number among evicted keys. A new key at
	// or below it may be a copy the window already forgot.
	floor int64
}

// NewDedupWindow returns a DedupWindow sized to hold up to `size` keys
// before the oldest entries are evicted. Pass 0 to use DefaultDedupWindowSize.
func NewDedupWindow(size int) *DedupWindow {
	if size <= 0 {
		size = DefaultDedupWindowSize
	}
	// Grow on demand: a window preallocated to size costs about 560 KiB at
	// the default, for every distinct filter, even one that never matches.
	return &DedupWindow{
		size: size,
		seen: make(map[dedupKey]struct{}),
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

// Mark records a key and returns true if it's newly seen. Returns false if
// the key is already in the window (caller should drop the duplicate).
func (w *DedupWindow) Mark(key string) bool {
	fresh, _ := w.MarkAt(key, 0)
	return fresh
}

// MarkAt is Mark for a key from block `block` (0 if unknown). late reports a
// fresh key at or below the highest block the window has evicted keys from:
// it may be a second copy the window no longer remembers.
func (w *DedupWindow) MarkAt(key string, block int64) (fresh, late bool) {
	k := hashDedupKey(key)
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[k]; ok {
		return false, false
	}
	w.seen[k] = struct{}{}
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
		delete(w.seen, e.key)
		evict++
	}
	if evict > 0 {
		w.order = w.order[evict:]
	}
	return true, block > 0 && block <= w.floor
}
