package valverelay

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/erpc/erpc/valvebilling"
)

// priceRowsFile is the shape of the pricing export: the rows of
// shared.method_pricing plus the tier-3 default. It is the shape
// valvebilling/testdata/cost-corpus.json already carries, so the generator
// that produces the corpus produces this too and no new format exists.
type priceRowsFile struct {
	DefaultCU *int64                  `json:"defaultCu"`
	Rows      []valvebilling.PriceRow `json:"rows"`
}

// methodCUFile is the shape of the compute-unit export
// (valvebilling/testdata/method-cu.json).
type methodCUFile struct {
	DefaultCU *int64           `json:"defaultCu"`
	MethodCU  map[string]int64 `json:"methodCu"`
}

// LoadPriceTable builds the price table from the two exported files.
//
// Both are data the monorepo owns. Neither value is defaulted here: the
// monorepo's own DEFAULT_CU was 20 until it was cut to 6, two comments still
// said 20 afterwards, and two separate readers copied the wrong number from
// them. A constant in this file would be a third copy to go stale.
//
// The two files must agree on defaultCu. One stale export against one fresh
// one prices every unlisted method wrong, and nothing else would notice.
func LoadPriceTable(rowsPath, methodCUPath string) (*valvebilling.PriceTable, error) {
	var rows priceRowsFile
	if err := readJSON(rowsPath, &rows); err != nil {
		return nil, err
	}
	if rows.DefaultCU == nil {
		return nil, fmt.Errorf("valverelay: %s has no defaultCu; it is not defaulted here", rowsPath)
	}

	var cu methodCUFile
	if err := readJSON(methodCUPath, &cu); err != nil {
		return nil, err
	}
	if cu.DefaultCU == nil {
		return nil, fmt.Errorf("valverelay: %s has no defaultCu; it is not defaulted here", methodCUPath)
	}
	if *cu.DefaultCU != *rows.DefaultCU {
		return nil, fmt.Errorf(
			"valverelay: %s says defaultCu is %d and %s says %d; one export is stale",
			rowsPath, *rows.DefaultCU, methodCUPath, *cu.DefaultCU)
	}

	table := valvebilling.NewPriceTable(cu.MethodCU, *rows.DefaultCU)
	if err := table.Load(rows.Rows); err != nil {
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
