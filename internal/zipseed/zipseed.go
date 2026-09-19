// Package zipseed seeds the zip_codes rotation table from a CSV of all US
// residential ZIP codes embedded in the binary. Seeding only runs when the
// table is empty, so redeploys never reset rotation state.
package zipseed

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/csv"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dwellingtw/backend/internal/db"
)

//go:embed zips.csv
var zipsCSV []byte

type row struct {
	Zip, City, State, County string
	Population               int
}

// parseRows decodes the embedded CSV (header: zip,city,state,county,population).
func parseRows() ([]row, error) {
	r := csv.NewReader(bytes.NewReader(zipsCSV))
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read zips csv: %w", err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("zips csv has no data rows")
	}
	out := make([]row, 0, len(records)-1)
	for _, rec := range records[1:] { // skip header
		pop, err := strconv.Atoi(rec[4])
		if err != nil {
			return nil, fmt.Errorf("zip %s: bad population %q: %w", rec[0], rec[4], err)
		}
		out = append(out, row{Zip: rec[0], City: rec[1], State: rec[2], County: rec[3], Population: pop})
	}
	return out, nil
}

// Seed inserts the embedded ZIP set into zip_codes when the table is empty.
// It returns the number of rows inserted (0 when the table was already
// seeded, so rotation state survives redeploys).
//
// The emptiness check and the COPY share one transaction under an advisory
// lock: instances booting together against an empty table would otherwise all
// pass the check, and all but one would die on the primary key.
func Seed(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var inserted int64
	err := db.WithXactLock(ctx, pool, db.LockSeed, func(tx pgx.Tx) error {
		var seeded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM zip_codes)`).Scan(&seeded); err != nil {
			return fmt.Errorf("check zip_codes: %w", err)
		}
		if seeded {
			return nil
		}
		rows, err := parseRows()
		if err != nil {
			return err
		}
		src := make([][]any, len(rows))
		for i, r := range rows {
			src[i] = []any{r.Zip, r.City, r.State, r.County, r.Population}
		}
		inserted, err = tx.CopyFrom(ctx,
			pgx.Identifier{"zip_codes"},
			[]string{"zip", "city", "state", "county", "population"},
			pgx.CopyFromRows(src),
		)
		if err != nil {
			return fmt.Errorf("copy zip_codes: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(inserted), nil
}
