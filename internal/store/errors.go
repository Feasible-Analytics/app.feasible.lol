//
// errors.go
// Recognising the driver errors callers have to branch on.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsUniqueViolation reports whether an error is SQLite refusing a duplicate
// under a UNIQUE constraint. It reads the driver's typed error code rather
// than the message text, so a wording change in the driver cannot turn a
// duplicate into an unexplained internal error.
func IsUniqueViolation(err error) bool {
	var driverErr *sqlite.Error
	if !errors.As(err, &driverErr) {
		return false
	}

	return driverErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
