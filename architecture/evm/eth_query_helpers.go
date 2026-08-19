package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

const (
	defaultQueryShimConcurrency   = 10
	defaultQueryShimMaxBlockRange = 10_000
	defaultQueryShimMaxLimit      = 10_000
	defaultQueryShimDefaultLimit  = 100
)

func forwardSubRequest(
	ctx context.Context,
	network common.Network,
	parentReqID interface{},
	pinToUpstreamId string,
	method string,
	params []interface{},
) ([]byte, error) {
	jrq := common.NewJsonRpcRequest(method, params)
	if err := jrq.SetID(util.RandomID()); err != nil {
		return nil, err
	}

	req := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	req.SetNetwork(network)
	req.SetParentRequestId(parentReqID)
	req.ApplyDirectiveDefaults(network.Config().DirectiveDefaults)
	if pinToUpstreamId != "" {
		req.SetDirectives(&common.RequestDirectives{UseUpstream: pinToUpstreamId})
	}

	resp, err := network.Forward(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("sub-request %s returned nil response", method)
	}
	defer resp.Release()

	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return nil, err
	}
	if jrr == nil {
		return nil, fmt.Errorf("sub-request %s returned empty json-rpc response", method)
	}
	if jrr.Error != nil {
		return nil, jrr.Error
	}

	var buf bytes.Buffer
	if _, err := jrr.WriteResultTo(&buf, false); err != nil {
		return nil, err
	}
	if buf.Len() == 0 {
		return []byte("null"), nil
	}

	return append([]byte(nil), buf.Bytes()...), nil
}

func fetchBlockRange(
	ctx context.Context,
	network common.Network,
	parentReqID interface{},
	pinToUpstreamId string,
	from uint64,
	to uint64,
	order string,
	fullTx bool,
	concurrency int,
) ([]json.RawMessage, error) {
	if concurrency <= 0 {
		concurrency = defaultQueryShimConcurrency
	}

	blockNumbers := make([]uint64, 0, blockSpan(from, to))
	if strings.EqualFold(order, "desc") {
		if from < to {
			return nil, nil
		}
		for n := from; ; n-- {
			blockNumbers = append(blockNumbers, n)
			if n == to {
				break
			}
		}
	} else {
		for n := from; n <= to; n++ {
			blockNumbers = append(blockNumbers, n)
		}
	}

	results := make([]json.RawMessage, len(blockNumbers))
	errs := make([]error, 0)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for i, blockNumber := range blockNumbers {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, num uint64) {
			defer wg.Done()
			defer func() { <-sem }()

			result, err := forwardSubRequest(
				ctx,
				network,
				parentReqID,
				pinToUpstreamId,
				"eth_getBlockByNumber",
				[]interface{}{fmt.Sprintf("0x%x", num), fullTx},
			)
			if err != nil {
				if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
					return
				}
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if bytes.Equal(result, []byte("null")) {
				return
			}

			results[idx] = append(json.RawMessage(nil), result...)
		}(i, blockNumber)
	}

	wg.Wait()
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	filtered := make([]json.RawMessage, 0, len(results))
	for _, result := range results {
		if len(result) == 0 {
			continue
		}
		filtered = append(filtered, result)
	}

	return filtered, nil
}

func resolveBlockTag(ctx context.Context, network common.Network, tag string) (uint64, error) {
	tag = strings.TrimSpace(strings.ToLower(tag))
	switch tag {
	case "":
		return uint64(common.EvmHighestLatestBlockNumber(network, ctx)), nil
	case "earliest":
		return 0, nil
	case "latest":
		return uint64(common.EvmHighestLatestBlockNumber(network, ctx)), nil
	case "finalized":
		return uint64(common.EvmHighestFinalizedBlockNumber(network, ctx)), nil
	case "safe":
		if finalized := common.EvmHighestFinalizedBlockNumber(network, ctx); finalized > 0 {
			return uint64(finalized), nil
		}
		return uint64(common.EvmHighestLatestBlockNumber(network, ctx)), nil
	case "pending":
		return 0, common.NewErrInvalidRequest(fmt.Errorf("pending block tag is not supported for query shim"))
	default:
		v, err := common.HexToUint64(tag)
		if err != nil {
			return 0, common.NewErrInvalidRequest(fmt.Errorf("invalid block tag: %s", tag))
		}
		return v, nil
	}
}

func matchesTransactionFilter(tx map[string]interface{}, filter *QueryFilter) bool {
	if filter == nil || tx == nil {
		return true
	}

	if len(filter.FromAddresses) > 0 && !hexFieldMatches(tx["from"], filter.FromAddresses) {
		return false
	}
	if len(filter.ToAddresses) > 0 {
		if tx["to"] == nil || !hexFieldMatches(tx["to"], filter.ToAddresses) {
			return false
		}
	}
	if len(filter.Selectors) > 0 {
		input, _ := tx["input"].(string)
		inputBytes, err := common.HexToBytes(input)
		if err != nil || len(inputBytes) < 4 {
			return false
		}
		matched := false
		for _, selector := range filter.Selectors {
			if len(selector) == 4 && bytes.Equal(inputBytes[:4], selector) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	return true
}

func matchesTraceFilter(trace map[string]interface{}, filter *QueryFilter) bool {
	if filter == nil || trace == nil {
		return true
	}

	if len(filter.FromAddresses) > 0 && !hexFieldMatches(trace["from"], filter.FromAddresses) {
		return false
	}
	if len(filter.ToAddresses) > 0 {
		if trace["to"] == nil || !hexFieldMatches(trace["to"], filter.ToAddresses) {
			return false
		}
	}
	if len(filter.Selectors) > 0 {
		input, _ := trace["input"].(string)
		inputBytes, err := common.HexToBytes(input)
		if err != nil || len(inputBytes) < 4 {
			return false
		}
		matched := false
		for _, selector := range filter.Selectors {
			if len(selector) == 4 && bytes.Equal(inputBytes[:4], selector) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if filter.IsTopLevel != nil && *filter.IsTopLevel {
		if traceAddressLen(trace["traceAddress"]) > 0 {
			return false
		}
	}

	return true
}

func matchesTransferFilter(transfer map[string]interface{}, filter *QueryFilter) bool {
	if filter == nil || transfer == nil {
		return true
	}

	if len(filter.FromAddresses) > 0 && !hexFieldMatches(transfer["from"], filter.FromAddresses) {
		return false
	}
	if len(filter.ToAddresses) > 0 && !hexFieldMatches(transfer["to"], filter.ToAddresses) {
		return false
	}
	if filter.IsTopLevel != nil && *filter.IsTopLevel {
		if traceAddressLen(transfer["traceAddress"]) > 0 {
			return false
		}
	}

	return true
}

func projectFields(obj map[string]interface{}, selection interface{}, alwaysKeep []string) map[string]interface{} {
	if obj == nil {
		return nil
	}

	switch v := selection.(type) {
	case nil:
		return cloneMap(obj)
	case bool:
		if v {
			return cloneMap(obj)
		}
	}

	requested := map[string]struct{}{}
	for _, field := range normalizeFieldSelection(selection) {
		requested[field] = struct{}{}
	}
	for _, field := range alwaysKeep {
		requested[field] = struct{}{}
	}

	projected := make(map[string]interface{}, len(requested))
	for field := range requested {
		if value, ok := obj[field]; ok {
			projected[field] = value
		}
	}

	return projected
}

func deduplicateByKey(objects []map[string]interface{}, key string) []map[string]interface{} {
	deduped := make([]map[string]interface{}, 0, len(objects))
	seen := make(map[string]struct{}, len(objects))

	for _, obj := range objects {
		if obj == nil {
			continue
		}
		value, ok := obj[key]
		if !ok || value == nil {
			continue
		}
		cacheKey := fmt.Sprintf("%v", value)
		if _, exists := seen[cacheKey]; exists {
			continue
		}
		seen[cacheKey] = struct{}{}
		deduped = append(deduped, obj)
	}

	return deduped
}

func buildCursorBlock(block map[string]interface{}) *QueryCursorBlock {
	if block == nil {
		return nil
	}

	number, err := parseUint64Value(block["number"])
	if err != nil {
		return nil
	}

	cursor := &QueryCursorBlock{Number: number}
	if hash, ok := block["hash"].(string); ok && hash != "" {
		cursor.Hash, _ = common.HexToBytes(hash)
	}
	if parentHash, ok := block["parentHash"].(string); ok && parentHash != "" {
		cursor.ParentHash, _ = common.HexToBytes(parentHash)
	}

	return cursor
}

func queryShimConfig(qs *common.EvmQueryShimConfig) (concurrency int, maxBlockRange int64, maxLimit int, defaultLimit int) {
	concurrency = defaultQueryShimConcurrency
	maxBlockRange = defaultQueryShimMaxBlockRange
	maxLimit = defaultQueryShimMaxLimit
	defaultLimit = defaultQueryShimDefaultLimit

	if qs == nil {
		return
	}
	if qs.Concurrency > 0 {
		concurrency = qs.Concurrency
	}
	if qs.MaxBlockRange > 0 {
		maxBlockRange = qs.MaxBlockRange
	}
	if qs.MaxLimit > 0 {
		maxLimit = qs.MaxLimit
	}
	if qs.DefaultLimit > 0 {
		defaultLimit = qs.DefaultLimit
	}

	return
}

func queryCapacityExceeded(message string, details map[string]interface{}) error {
	return common.NewErrJsonRpcExceptionInternal(
		0,
		common.JsonRpcErrorCapacityExceeded,
		message,
		nil,
		details,
	)
}

func parseUint64Value(raw interface{}) (uint64, error) {
	switch v := raw.(type) {
	case nil:
		return 0, fmt.Errorf("missing quantity")
	case uint64:
		return v, nil
	case uint32:
		return uint64(v), nil
	case int:
		if v < 0 {
			return 0, fmt.Errorf("negative quantity")
		}
		return uint64(v), nil
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("negative quantity")
		}
		return uint64(v), nil
	case float64:
		if v < 0 {
			return 0, fmt.Errorf("negative quantity")
		}
		return uint64(v), nil
	case string:
		if v == "" {
			return 0, fmt.Errorf("empty quantity")
		}
		// Only the lowercase prefix. common.HexToUint64 rejects "0X" outright,
		// so testing for it here only stole the value from the decimal parser
		// below and answered with "invalid hex string" — a message that hides
		// which character is wrong. Nothing observed shows a client sending
		// "0X". See entry 120 in valve/upstream-bug-log.md.
		if strings.HasPrefix(v, "0x") {
			return common.HexToUint64(v)
		}
		return strconv.ParseUint(v, 10, 64)
	default:
		return 0, fmt.Errorf("unsupported quantity type %T", raw)
	}
}

// parseOptionalQuantity reads a wire quantity that a vendor may legitimately
// omit. Parity omits `gas` and `transactionIndex` on a reward trace, and the
// proto these values feed has no way to say "absent", so an absent field keeps
// the zero.
//
// A field that is PRESENT and unreadable is a different event. eRPC reports it
// rather than answering the client with a number it invented. See entry 132 in
// valve/upstream-bug-log.md.
func parseOptionalQuantity(raw string, field string) (uint64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := parseUint64Value(raw)
	if err != nil {
		return 0, fmt.Errorf("unreadable %s: %w", field, err)
	}
	return value, nil
}

func uint32FromUint64(value uint64, field string) (uint32, error) {
	if value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("%s exceeds uint32 range", field)
	}
	return uint32(value), nil
}

func normalizeFieldSelection(selection interface{}) []string {
	switch v := selection.(type) {
	case []string:
		return append([]string(nil), v...)
	case []interface{}:
		fields := make([]string, 0, len(v))
		for _, raw := range v {
			if field, ok := raw.(string); ok && field != "" {
				fields = append(fields, field)
			}
		}
		return fields
	default:
		return nil
	}
}

func hexFieldMatches(raw interface{}, candidates [][]byte) bool {
	value, ok := raw.(string)
	if !ok || value == "" {
		return false
	}

	valueBytes, err := common.HexToBytes(value)
	if err != nil {
		return false
	}

	for _, candidate := range candidates {
		if bytes.Equal(valueBytes, candidate) {
			return true
		}
	}

	return false
}

func cloneMap(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}

	out := make(map[string]interface{}, len(input))
	for key, value := range input {
		out[key] = deepCopyQueryValue(value)
	}
	return out
}

func deepCopyQueryValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return cloneMap(v)
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = deepCopyQueryValue(item)
		}
		return out
	default:
		return v
	}
}

func blockSpan(from uint64, to uint64) uint64 {
	if from >= to {
		return from - to + 1
	}
	return to - from + 1
}

func blockMapFromRaw(raw json.RawMessage) (map[string]interface{}, error) {
	var block map[string]interface{}
	if err := sonic.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	return block, nil
}

func jsonMapFromProtoTrace(trace *bdsevm.Trace) map[string]interface{} {
	if trace == nil {
		return nil
	}

	traceAddress := make([]interface{}, 0, len(trace.TraceAddress))
	for _, idx := range trace.TraceAddress {
		traceAddress = append(traceAddress, fmt.Sprintf("0x%x", idx))
	}

	out := map[string]interface{}{
		"traceType":        strings.ToLower(strings.TrimPrefix(trace.TraceType.String(), "TRACE_")),
		"callType":         strings.ToLower(strings.TrimPrefix(trace.CallType.String(), "TRACE_CALL_")),
		"from":             bdsevm.BytesToHex(trace.From),
		"value":            trace.Value,
		"input":            bdsevm.BytesToHex(trace.Input),
		"output":           bdsevm.BytesToHex(trace.Output),
		"gas":              fmt.Sprintf("0x%x", trace.Gas),
		"gasUsed":          fmt.Sprintf("0x%x", trace.GasUsed),
		"subtraces":        fmt.Sprintf("0x%x", trace.Subtraces),
		"traceAddress":     traceAddress,
		"transactionHash":  bdsevm.BytesToHex(trace.TransactionHash),
		"transactionIndex": fmt.Sprintf("0x%x", trace.TransactionIndex),
		"blockNumber":      fmt.Sprintf("0x%x", trace.BlockNumber),
		"blockHash":        bdsevm.BytesToHex(trace.BlockHash),
	}
	if len(trace.To) > 0 {
		out["to"] = bdsevm.BytesToHex(trace.To)
	} else {
		out["to"] = nil
	}
	if trace.Error != nil {
		out["error"] = *trace.Error
	}
	if trace.BlockTimestamp != nil {
		out["blockTimestamp"] = fmt.Sprintf("0x%x", *trace.BlockTimestamp)
	}

	return out
}

func jsonMapFromProtoTransfer(transfer *bdsevm.NativeTransfer) map[string]interface{} {
	if transfer == nil {
		return nil
	}

	traceAddress := make([]interface{}, 0, len(transfer.TraceAddress))
	for _, idx := range transfer.TraceAddress {
		traceAddress = append(traceAddress, fmt.Sprintf("0x%x", idx))
	}

	out := map[string]interface{}{
		"from":             bdsevm.BytesToHex(transfer.From),
		"to":               bdsevm.BytesToHex(transfer.To),
		"value":            transfer.Value,
		"transactionHash":  bdsevm.BytesToHex(transfer.TransactionHash),
		"transactionIndex": fmt.Sprintf("0x%x", transfer.TransactionIndex),
		"blockNumber":      fmt.Sprintf("0x%x", transfer.BlockNumber),
		"blockHash":        bdsevm.BytesToHex(transfer.BlockHash),
		"traceAddress":     traceAddress,
	}
	if transfer.BlockTimestamp != nil {
		out["blockTimestamp"] = fmt.Sprintf("0x%x", *transfer.BlockTimestamp)
	}

	return out
}

// sortLogs orders a page of logs by (blockNumber, logIndex) and returns the
// block number of each log in its sorted position.
//
// Both keys are read exactly once, here. The page order AND the pagination
// cursor come from them, so a log whose blockNumber eRPC cannot read is
// reported rather than sorted as block 0 — a zero sorts to the head of an
// ascending page and can hand the client a cursor that restarts at genesis.
// See entry 132 in valve/upstream-bug-log.md.
func sortLogs(logs []map[string]interface{}, order string) ([]uint64, error) {
	type sortKey struct {
		block uint64
		index uint64
	}
	keys := make([]sortKey, len(logs))
	for i, log := range logs {
		block, err := parseUint64Value(log["blockNumber"])
		if err != nil {
			return nil, fmt.Errorf("log at position %d has an unreadable blockNumber: %w", i, err)
		}
		index, err := parseUint64Value(log["logIndex"])
		if err != nil {
			return nil, fmt.Errorf("log at position %d has an unreadable logIndex: %w", i, err)
		}
		keys[i] = sortKey{block: block, index: index}
	}

	// Sort a permutation so the keys stay paired with their log without
	// re-reading the maps on every comparison.
	pos := make([]int, len(logs))
	for i := range pos {
		pos[i] = i
	}
	descending := strings.EqualFold(order, "desc")
	sort.SliceStable(pos, func(i, j int) bool {
		left, right := keys[pos[i]], keys[pos[j]]
		if left.block != right.block {
			if descending {
				return left.block > right.block
			}
			return left.block < right.block
		}
		if descending {
			return left.index > right.index
		}
		return left.index < right.index
	})

	sorted := make([]map[string]interface{}, len(logs))
	blocks := make([]uint64, len(logs))
	for i, p := range pos {
		sorted[i] = logs[p]
		blocks[i] = keys[p].block
	}
	copy(logs, sorted)
	return blocks, nil
}

func traceAddressLen(raw interface{}) int {
	switch v := raw.(type) {
	case []interface{}:
		return len(v)
	case []string:
		return len(v)
	default:
		return 0
	}
}

func queryRangeIsEmpty(req *QueryRequest) bool {
	if req == nil {
		return true
	}
	if strings.EqualFold(req.Order, "desc") {
		return req.FromBlock < req.ToBlock
	}
	return req.FromBlock > req.ToBlock
}
