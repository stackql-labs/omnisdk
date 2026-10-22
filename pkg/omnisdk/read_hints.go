package omnisdk

import "github.com/stackql-labs/omnisdk/internal/system_g/facade"

// ReadHints are advisory SQL controls for one graph alias. Local filtering,
// ordering and projection remain authoritative. DisableRowControls prevents
// source LIMIT/OFFSET/ORDER pushdown for grouping, aggregation or DISTINCT.
type ReadHints struct {
	Columns            []string    `json:"columns,omitempty"`
	AllColumns         bool        `json:"all_columns,omitempty"`
	OrderBy            []ReadOrder `json:"order_by,omitempty"`
	Limit              int         `json:"limit,omitempty"`
	Offset             int         `json:"offset,omitempty"`
	Count              bool        `json:"count,omitempty"`
	DisableRowControls bool        `json:"disable_row_controls,omitempty"`
}

// ReadOrder is one original SQL sort term.
type ReadOrder struct {
	Column     string `json:"column"`
	Descending bool   `json:"descending,omitempty"`
}

// ReadEffects is optional metadata on Rows. A caller applying SQL OFFSET after
// collecting rows subtracts AppliedOffset so the server offset is consumed once.
type ReadEffects interface{ AppliedOffset() int }

var _ facade.ReadEffects = (*rows)(nil)

func (r *rows) AppliedOffset() int { return r.remoteOffset }
