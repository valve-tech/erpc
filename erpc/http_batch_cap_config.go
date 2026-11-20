package erpc

import (
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// batchCapOf is a network's maxClientBatchSize, read without bootstrapping
// the network. The batch cap runs before authentication, so it must not
// build a network, call providers, or cache clients for a network id the
// caller chose: a prepared network gives its own config; otherwise the
// project's static config for that id, then networkDefaults. It returns the
// canonical id, so an alias and its id count as one network, and 0 for no cap.
func batchCapOf(project *PreparedProject, networkId string) (canonicalId string, arch common.NetworkArchitecture, limit int) {
	canonicalId, ok := util.CanonicalNetworkId(networkId)
	if !ok {
		return "", "", 0
	}
	if i := strings.Index(canonicalId, ":"); i > 0 {
		arch = common.NetworkArchitecture(canonicalId[:i])
	}
	var max *int
	if pn, ok := project.networksRegistry.preparedNetworks.Load(canonicalId); ok {
		if cfg := pn.(*Network).Config(); cfg != nil {
			max = cfg.MaxClientBatchSize
		}
	} else if cfg := project.FindNetworkConfig(canonicalId); cfg != nil {
		max = cfg.MaxClientBatchSize
	} else if project.Config != nil && project.Config.NetworkDefaults != nil {
		max = project.Config.NetworkDefaults.MaxClientBatchSize
	}
	if max == nil || *max <= 0 {
		return canonicalId, arch, 0
	}
	return canonicalId, arch, *max
}
