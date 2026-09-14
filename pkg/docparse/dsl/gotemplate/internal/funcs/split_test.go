package funcs_test

import (
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate/internal/funcs"
)

// An ARM resource id carries the subscription and the resource group inside one value, and a join on
// the resource group has no way to reach it otherwise.
func TestSplitReachesAnArmResourceGroup(t *testing.T) {
	const id = "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/virtualNetworks/vnet-1"
	parts := funcs.Split("/", id)
	if len(parts) < 5 || parts[4] != "rg-1" {
		t.Errorf("split = %v, want the resource group at index 4", parts)
	}
}
