package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/upstream"
)

// API Key structure for management
type ApiKey struct {
	Key             string    `json:"key"`
	UserId          string    `json:"userId"`
	RateLimitBudget string    `json:"rateLimitBudget,omitempty"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func (e *ERPC) AdminAuthenticate(ctx context.Context, req *common.NormalizedRequest, method string, ap *auth.AuthPayload) (*common.User, error) {
	if e.adminAuthRegistry != nil {
		return e.adminAuthRegistry.Authenticate(ctx, req, method, ap)
	}
	return nil, fmt.Errorf("admin auth not configured")
}

func (e *ERPC) AdminHandleRequest(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	method, err := nq.Method()
	if err != nil {
		return nil, err
	}

	switch method {
	case "erpc_taxonomy":
		return e.handleTaxonomy(ctx, nq)
	case "erpc_config":
		return e.handleConfig(nq)
	case "erpc_project":
		return e.handleProject(nq)
	case "erpc_addApiKey":
		return e.handleAddApiKey(ctx, nq)
	case "erpc_listApiKeys":
		return e.handleListApiKeys(ctx, nq)
	case "erpc_updateApiKey":
		return e.handleUpdateApiKey(ctx, nq)
	case "erpc_deleteApiKey":
		return e.handleDeleteApiKey(ctx, nq)
	case "erpc_cordonUpstream":
		return e.handleCordonUpstream(ctx, nq, true)
	case "erpc_uncordonUpstream":
		return e.handleCordonUpstream(ctx, nq, false)
	case "erpc_listCordoned":
		return e.handleListCordoned(ctx, nq)

	default:
		return nil, common.NewErrEndpointUnsupported(
			fmt.Errorf("admin method %s is not supported", method),
		)
	}
}

// makeSelectionResponse wraps a result map into the JSON-RPC envelope the
// admin handlers all use.
func makeSelectionResponse(nq *common.NormalizedRequest, result map[string]interface{}) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// findDatabaseStrategyById finds a project's database auth strategy by its
// connector ID. The API-key handlers write through the strategy, not around it,
// so a change reaches the store and the caches in front of it together.
func (e *ERPC) findDatabaseStrategyById(projectId, connectorId string) (*auth.DatabaseStrategy, error) {
	if e.projectsRegistry == nil {
		return nil, fmt.Errorf("projects registry not configured")
	}

	// Get the prepared project
	preparedProject := e.projectsRegistry.preparedProjects[projectId]
	if preparedProject == nil {
		return nil, fmt.Errorf("project '%s' not found", projectId)
	}

	// Get the project's auth registry
	if preparedProject.consumerAuthRegistry == nil {
		return nil, fmt.Errorf("project '%s' has no auth registry", projectId)
	}

	// Find the database strategy within the project
	return preparedProject.consumerAuthRegistry.FindDatabaseStrategy(connectorId)
}

// deleteLegacyApiKeyRecord removes an API-key record left at (apiKey, userId)
// by an eRPC old enough to put the user id in the address.
//
// It only matters on PostgreSQL. That driver expands "*" on a main-index range
// key, so a read at data.ConnectorApiKeyRangeKey matches the legacy record too,
// and an update or a revoke that wrote only to the canonical address would
// leave the old record behind for the next read to pick up instead. A key an
// operator believes they revoked would keep working. Every other driver matches
// keys literally, where this delete finds nothing and is a no-op.
//
// A key issued twice under two different user ids is out of reach here: the
// read resolves one of the records and this clears that one. That case
// predates the fix and is unchanged by it.
func deleteLegacyApiKeyRecord(ctx context.Context, connector data.Connector, apiKey, userId string) error {
	if userId == data.ConnectorApiKeyRangeKey {
		return nil
	}
	return connector.Delete(ctx, apiKey, userId)
}

// handleAddApiKey adds a new API key
func (e *ERPC) handleAddApiKey(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, apiKey, userId, rateLimitBudget?}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	apiKey, ok := params["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("apiKey is required and must be a string"))
	}

	userId, ok := params["userId"].(string)
	if !ok || userId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("userId is required and must be a string"))
	}

	var rateLimitBudget string
	if budget, exists := params["rateLimitBudget"]; exists && budget != nil {
		if budgetStr, ok := budget.(string); ok {
			rateLimitBudget = budgetStr
		} else {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("rateLimitBudget must be a string"))
		}
	}

	strategy, err := e.findDatabaseStrategyById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}

	enabled := true
	if enabledVal, exists := params["enabled"]; exists && enabledVal != nil {
		if enabledBool, ok := enabledVal.(bool); ok {
			enabled = enabledBool
		} else {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("enabled must be a boolean"))
		}
	}

	// Create user data
	userData := map[string]interface{}{
		"userId":  userId,
		"enabled": enabled,
	}
	if rateLimitBudget != "" {
		userData["rateLimitBudget"] = rateLimitBudget
	}

	userDataBytes, err := json.Marshal(userData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal user data: %w", err)
	}

	// The record is addressed by the API key alone. That is all a caller
	// presents, and the auth strategy has nothing else to look it up with. The
	// user id rides inside the record body, where the strategy reads it from.
	err = strategy.GetConnector().Set(ctx, apiKey, data.ConnectorApiKeyRangeKey, userDataBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to store API key: %w", err)
	}

	// Drop any cached decision for this key. Issuing a key that a caller has
	// already tried would otherwise stay refused until the negative cache
	// expires.
	strategy.InvalidateCache(apiKey)

	result := map[string]interface{}{
		"success": true,
		"apiKey":  apiKey,
		"userId":  userId,
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleListApiKeys lists API keys for a connector
func (e *ERPC) handleListApiKeys(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, limit?, paginationToken?}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	limit := 50 // default
	if limitVal, exists := params["limit"]; exists && limitVal != nil {
		if limitFloat, ok := limitVal.(float64); ok {
			limit = int(limitFloat)
		}
	}

	paginationToken := ""
	if tokenVal, exists := params["paginationToken"]; exists && tokenVal != nil {
		if tokenStr, ok := tokenVal.(string); ok {
			paginationToken = tokenStr
		}
	}

	strategy, err := e.findDatabaseStrategyById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}

	// List from main index to get all API keys (optimized: only 1 row per API key)
	results, nextToken, err := strategy.GetConnector().List(ctx, data.ConnectorMainIndex, limit, paginationToken)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}

	apiKeys := make([]ApiKey, 0)
	for _, item := range results {
		// Parse user data from the value
		var userData map[string]interface{}
		if err := json.Unmarshal(item.Value, &userData); err != nil {
			continue // Skip invalid records
		}

		// Create API key object. The user id comes from the record body, which
		// is where the auth strategy reads it from. The range key is a fixed
		// address and names nobody.
		apiKey := ApiKey{
			Key:       item.PartitionKey,
			Enabled:   true,       // Default to enabled if not specified
			CreatedAt: time.Now(), // TODO: Add actual timestamps
			UpdatedAt: time.Now(),
		}

		if userId, ok := userData["userId"].(string); ok {
			apiKey.UserId = userId
		}

		// Read the enabled status from the database
		if enabled, ok := userData["enabled"]; ok {
			if enabledBool, ok := enabled.(bool); ok {
				apiKey.Enabled = enabledBool
			}
		}

		if budget, ok := userData["rateLimitBudget"]; ok {
			if budgetStr, ok := budget.(string); ok {
				apiKey.RateLimitBudget = budgetStr
			}
		}

		apiKeys = append(apiKeys, apiKey)
	}

	result := map[string]interface{}{
		"apiKeys":       apiKeys,
		"nextToken":     nextToken,
		"hasMore":       nextToken != "",
		"totalReturned": len(apiKeys),
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleUpdateApiKey updates an existing API key
func (e *ERPC) handleUpdateApiKey(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, apiKey, updates}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	apiKey, ok := params["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("apiKey is required and must be a string"))
	}

	updates, ok := params["updates"].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("updates is required and must be an object"))
	}

	strategy, err := e.findDatabaseStrategyById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}
	connector := strategy.GetConnector()

	currentBytes, err := connector.Get(ctx, data.ConnectorMainIndex, apiKey, data.ConnectorApiKeyRangeKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get current API key data: %w", err)
	}

	var currentData map[string]interface{}
	if err := json.Unmarshal(currentBytes, &currentData); err != nil {
		return nil, fmt.Errorf("failed to parse current data: %w", err)
	}

	// A record with no userId cannot authenticate anybody, so refuse to write
	// one back rather than persist it in that state.
	userId, ok := currentData["userId"].(string)
	if !ok || userId == "" {
		return nil, fmt.Errorf("missing or invalid userId in current data")
	}

	// Apply updates
	for key, value := range updates {
		if value == nil {
			delete(currentData, key)
		} else {
			currentData[key] = value
		}
	}

	// Save updated data to the same location
	updatedBytes, err := json.Marshal(currentData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal updated data: %w", err)
	}

	if err := connector.Set(ctx, apiKey, data.ConnectorApiKeyRangeKey, updatedBytes, nil); err != nil {
		return nil, fmt.Errorf("failed to update API key: %w", err)
	}

	if err := deleteLegacyApiKeyRecord(ctx, connector, apiKey, userId); err != nil {
		return nil, fmt.Errorf("failed to update API key: %w", err)
	}

	strategy.InvalidateCache(apiKey)

	result := map[string]interface{}{
		"success": true,
		"apiKey":  apiKey,
		"updated": updates,
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleDeleteApiKey deletes an API key and its reverse index
func (e *ERPC) handleDeleteApiKey(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, apiKey}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	apiKey, ok := params["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("apiKey is required and must be a string"))
	}

	strategy, err := e.findDatabaseStrategyById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}
	connector := strategy.GetConnector()

	// Read the record first, so the report can name the user whose access is
	// being withdrawn, and so revoking a key that was never issued says so
	// rather than reporting a revocation that did not happen.
	currentBytes, err := connector.Get(ctx, data.ConnectorMainIndex, apiKey, data.ConnectorApiKeyRangeKey, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get current API key data: %w", err)
	}

	var currentData map[string]interface{}
	if err := json.Unmarshal(currentBytes, &currentData); err != nil {
		return nil, fmt.Errorf("failed to parse current data: %w", err)
	}

	userId, ok := currentData["userId"].(string)
	if !ok || userId == "" {
		return nil, fmt.Errorf("missing or invalid userId in current data")
	}

	if err := connector.Delete(ctx, apiKey, data.ConnectorApiKeyRangeKey); err != nil {
		return nil, fmt.Errorf("failed to delete API key: %w", err)
	}

	if err := deleteLegacyApiKeyRecord(ctx, connector, apiKey, userId); err != nil {
		return nil, fmt.Errorf("failed to delete API key: %w", err)
	}

	strategy.InvalidateCache(apiKey)

	result := map[string]interface{}{
		"success": true,
		"apiKey":  apiKey,
		"userId":  userId,
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleConfig returns the eRPC configuration
func (e *ERPC) handleConfig(nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	jrrs, err := common.NewJsonRpcResponse(
		jrr.ID,
		e.cfg,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleTaxonomy returns the taxonomy of projects, networks, and upstreams
func (e *ERPC) handleTaxonomy(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	type taxonomyUpstream struct {
		Id     string `json:"id"`
		Vendor string `json:"vendor"`
	}
	type taxonomyProvider struct {
		Id     string `json:"id"`
		Vendor string `json:"vendor"`
	}
	type taxonomyNetwork struct {
		Id        string              `json:"id"`
		Alias     string              `json:"alias"`
		Upstreams []*taxonomyUpstream `json:"upstreams"`
		Providers []*taxonomyProvider `json:"providers"`
	}
	type taxonomyProject struct {
		Id       string             `json:"id"`
		Networks []*taxonomyNetwork `json:"networks"`
	}
	type taxonomyResult struct {
		Projects []*taxonomyProject `json:"projects"`
	}

	result := &taxonomyResult{}
	projects := e.GetProjects()
	for _, p := range projects {
		networks := []*taxonomyNetwork{}
		for _, n := range p.GetNetworks() {
			ntw := &taxonomyNetwork{
				Id:        n.Id(),
				Upstreams: []*taxonomyUpstream{},
			}
			if n.cfg != nil && n.cfg.Alias != "" {
				ntw.Alias = n.cfg.Alias
			}
			upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.Id())
			for _, u := range upstreams {
				ups := taxonomyUpstream{
					Id: u.Id(),
				}
				if u.Vendor() != nil {
					ups.Vendor = u.Vendor().Name()
				}
				ntw.Upstreams = append(ntw.Upstreams, &ups)
			}
			networks = append(networks, ntw)
		}
		result.Projects = append(result.Projects, &taxonomyProject{
			Id:       p.Config.Id,
			Networks: networks,
		})
	}

	jrrs, err := common.NewJsonRpcResponse(
		jrr.ID,
		result,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleProject returns the configuration and health information for a specific project
func (e *ERPC) handleProject(nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	type configResult struct {
		Config *common.ProjectConfig `json:"config"`
		Health *ProjectHealthInfo    `json:"health"`
	}

	if len(jrr.Params) == 0 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("project id (params[0]) is required"))
	}

	pid, ok := jrr.Params[0].(string)
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("project id (params[0]) must be a string"))
	}

	p, err := e.GetProject(pid)
	if err != nil {
		return nil, err
	}

	health, err := p.GatherHealthInfo()
	if err != nil {
		return nil, err
	}

	result := configResult{
		Config: p.Config,
		Health: health,
	}

	jrrs, err := common.NewJsonRpcResponse(
		jrr.ID,
		result,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// ─── Cordon admin RPCs ──────────────────────────────────────────────────
//
// Cordon is the operator's manual "mark this upstream out of rotation"
// switch. The next policy tick observes `cordonedReason` on the upstream
// and the default policy's `.removeCordoned()` step drops it from the
// returned list, taking it out of routing for every request until the
// operator uncordons it again. Cordon state survives across rolling-window
// rotations of the health tracker (it isn't a metric — it's an explicit
// mark on the (upstream, method) cell).
//
// Scope is `(upstream, method)`. Cordoning method `"*"` cordons the
// upstream wholesale; cordoning a specific method (e.g. `eth_getLogs`)
// only excludes the upstream when that method is dispatched.
//
// See docs/pages/operation/cordoning.mdx for the full operator runbook.

type cordonParams struct {
	ProjectID string `json:"projectId"`
	Upstream  string `json:"upstream"`
	Method    string `json:"method,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func parseCordonParams(nq *common.NormalizedRequest) (cordonParams, error) {
	var p cordonParams
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return p, err
	}
	if len(jrr.Params) == 0 {
		return p, fmt.Errorf("cordon admin: params is required")
	}
	raw, err := json.Marshal(jrr.Params[0])
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("cordon admin: invalid params: %w", err)
	}
	if p.ProjectID == "" || p.Upstream == "" {
		return p, fmt.Errorf("cordon admin: projectId and upstream are required")
	}
	if p.Method == "" {
		p.Method = "*"
	}
	return p, nil
}

// findUpstreamById walks the project's upstream registry and returns the
// matching upstream, or an error if none match.
func (e *ERPC) findUpstreamById(projectID, upstreamID string) (*upstream.Upstream, error) {
	prj, err := e.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if prj.upstreamsRegistry == nil {
		return nil, fmt.Errorf("cordon admin: project %s has no upstream registry", projectID)
	}
	for _, u := range prj.upstreamsRegistry.GetAllUpstreams() {
		if u.Id() == upstreamID {
			return u, nil
		}
	}
	return nil, fmt.Errorf("cordon admin: upstream %q not found in project %q", upstreamID, projectID)
}

// handleCordonUpstream marks an upstream cordoned (cordon=true) or
// uncordoned (cordon=false) for a specific method scope.
func (e *ERPC) handleCordonUpstream(_ context.Context, nq *common.NormalizedRequest, cordon bool) (*common.NormalizedResponse, error) {
	p, err := parseCordonParams(nq)
	if err != nil {
		return nil, err
	}
	u, err := e.findUpstreamById(p.ProjectID, p.Upstream)
	if err != nil {
		return nil, err
	}
	reason := p.Reason
	if reason == "" {
		if cordon {
			reason = "admin: manual cordon"
		} else {
			reason = "admin: manual uncordon"
		}
	}
	if cordon {
		u.Cordon(p.Method, reason)
	} else {
		u.Uncordon(p.Method, reason)
	}
	return makeSelectionResponse(nq, map[string]interface{}{
		"projectId": p.ProjectID,
		"upstream":  p.Upstream,
		"method":    p.Method,
		"cordoned":  cordon,
		"reason":    reason,
	})
}

// handleListCordoned returns every upstream currently cordoned in a
// project — useful for `--reconcile` style scripts that want to inspect
// state before deciding whether to uncordon.
func (e *ERPC) handleListCordoned(_ context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	type listParams struct {
		ProjectID string `json:"projectId"`
	}
	var lp listParams
	if len(jrr.Params) > 0 {
		raw, _ := json.Marshal(jrr.Params[0])
		_ = json.Unmarshal(raw, &lp)
	}
	if lp.ProjectID == "" {
		return nil, fmt.Errorf("cordon admin: projectId is required")
	}
	prj, err := e.GetProject(lp.ProjectID)
	if err != nil {
		return nil, err
	}
	if prj.upstreamsRegistry == nil {
		return nil, fmt.Errorf("cordon admin: project %s has no upstream registry", lp.ProjectID)
	}
	type cordonedRow struct {
		Upstream string `json:"upstream"`
		Reason   string `json:"reason"`
	}
	rows := []cordonedRow{}
	for _, u := range prj.upstreamsRegistry.GetAllUpstreams() {
		if reason, cordoned := u.CordonedReason("*"); cordoned {
			rows = append(rows, cordonedRow{Upstream: u.Id(), Reason: reason})
		}
	}
	return makeSelectionResponse(nq, map[string]interface{}{
		"projectId": lp.ProjectID,
		"cordoned":  rows,
	})
}
