package valverelay

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/erpc/erpc/valvebilling"
)

// priceExport is the shape of the pricing export: the rows of
// shared.method_pricing, the tier-3 compute-unit table, and the tier-3
// default. All three live in ONE file — valvebilling/testdata/cost-corpus.json
// — so the generator that produces the corpus produces this too and no new
// format exists.
type priceExport struct {
	DefaultCU *int64                  `json:"defaultCu"`
	MethodCU  map[string]int64        `json:"methodCu"`
	Rows      []valvebilling.PriceRow `json:"rows"`
}

// LoadPriceTable builds the price table from the exported corpus.
//
// It takes ONE path. It used to take two — rows and the compute-unit table
// were separate exports — and it cross-checked that their defaultCu agreed,
// because one stale export against one fresh one prices every unlisted method
// wrong and nothing else would notice. The monorepo folded methodCu into the
// corpus, so that drift is now impossible by construction and the check is
// gone with the second file. A guard against a state that cannot occur is
// machinery nothing exercises.
//
// defaultCu is not defaulted here. The monorepo's own DEFAULT_CU was 20 until
// it was cut to 6, two comments still said 20 afterwards, and two separate
// readers copied the wrong number from them. A constant in this file would be
// a third copy to go stale.
func LoadPriceTable(path string) (*valvebilling.PriceTable, error) {
	var export priceExport
	if err := readJSON(path, &export); err != nil {
		return nil, err
	}
	if export.DefaultCU == nil {
		return nil, fmt.Errorf("valverelay: %s has no defaultCu; it is not defaulted here", path)
	}
	if len(export.MethodCU) == 0 {
		return nil, fmt.Errorf(
			"valverelay: %s carries no methodCu; a Go-side copy of that table is how the two languages drift", path)
	}

	table := valvebilling.NewPriceTable(export.MethodCU, *export.DefaultCU)
	if err := table.Load(export.Rows); err != nil {
		return nil, err
	}
	return table, nil
}

func readJSON(path string, into interface{}) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("valverelay: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("valverelay: parsing %s: %w", path, err)
	}
	return nil
}
