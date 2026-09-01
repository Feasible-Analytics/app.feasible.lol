//
// numeric.go
// Reading a number out of a custom property that is stored as text.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"database/sql/driver"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// NumberFunction is the SQL function every numeric property aggregate reads its
// values through.
//
// Custom properties are stored as text — the ingest path flattens every value
// to a string so that a filter has one type to compare against — so summing one
// means parsing it. SQLite's own CAST is the wrong tool for that and quietly so:
// CAST('abc' AS REAL) is 0.0 and CAST('12kg' AS REAL) is 12.0, both with no
// error. A property that holds a number on most events and a label on a few
// would then average in a pile of zeros, and nothing on the screen would say so.
//
// This answers NULL for anything that is not a number from end to end, which
// takes the value out of the aggregate rather than folding it in, and lets the
// same pass count how many were left out.
const NumberFunction = "feasible_number"

// numberArgs is how many arguments the function takes: the value.
const numberArgs = 1

var (
	numberOnce sync.Once
	numberErr  error
)

// init registers the parser exactly once, against the driver rather than a
// connection, so every connection opened afterwards has it. A failure is kept
// rather than panicked on, so a process that never asks for a numeric aggregate
// still starts and one that does gets a clear error on that query.
func init() {
	numberOnce.Do(func() {
		numberErr = sqlite.RegisterDeterministicScalarFunction(NumberFunction, numberArgs, numberScalar)
	})
}

// numberScalar is the function body SQLite calls per row.
func numberScalar(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != numberArgs {
		return nil, fmt.Errorf("%s takes %d argument", NumberFunction, numberArgs)
	}

	switch typed := args[0].(type) {
	case int64:
		return float64(typed), nil
	case float64:
		return finite(typed), nil
	case string:
		return parseNumber(typed)
	case []byte:
		return parseNumber(string(typed))
	}

	// NULL in, NULL out: an event that never carried the property is absent
	// from the aggregate rather than being a zero in it.
	return nil, nil
}

// parseNumber reads a stored property value as a number, answering NULL for
// anything Go's own parser will not take whole. Whole is the important word:
// accepting a numeric prefix is how "12kg" becomes twelve.
func parseNumber(raw string) (driver.Value, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return nil, nil
	}

	return finite(value), nil
}

// finite answers NULL for a value no aggregate can survive. A single infinity
// in a SUM makes every group total infinite, and JSON cannot represent either
// of these, so the encoder would fail the whole response over one bad property.
func finite(value float64) driver.Value {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}

	return value
}

// numberError reports a failed registration. The compiler asks before it emits
// a call, so an unusable parser is a clear error on the one query that needed
// it rather than a syntax error from SQLite.
func numberError() error {
	if numberErr == nil {
		return nil
	}

	return fmt.Errorf("numeric property aggregation is unavailable: %w", numberErr)
}
