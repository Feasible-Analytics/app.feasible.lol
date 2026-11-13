//
// uninterruptible_test.go
// Tests that a cancelled context never interrupts a statement on a handle that writes.
//
// Created: 2026-11-13
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// cancelDuringStatement holds the cancel function the next statement calling
// cancel_mid_statement() fires.
var cancelDuringStatement atomic.Pointer[context.CancelFunc]

// registerCancelFunction installs cancel_mid_statement(). On its first row it
// cancels the statement's context, then pauses so the driver's interrupt lands
// while the statement is still running. The driver attaches functions when a
// connection opens, so tests call this before opening anything.
var registerCancelFunction = sync.OnceFunc(func() {
	sqlite.MustRegisterScalarFunction("cancel_mid_statement", 1, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		if cancel := cancelDuringStatement.Swap(nil); cancel != nil {
			(*cancel)()
			time.Sleep(50 * time.Millisecond)
		}

		return int64(1), nil
	})
})

// cancelledInsert writes enough rows that the statement is still running when
// its context is cancelled on the first one.
const cancelledInsert = `
	WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 20000)
	INSERT INTO t (n) SELECT n FROM seq WHERE cancel_mid_statement(n)`

// countT reads how many rows table t holds.
func countT(t *testing.T, db *sql.DB) int {
	t.Helper()

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count
}

// TestACancelledContextDoesNotInterruptAWrite covers every handle that writes.
// An interrupted statement can be left unfinalized by the driver, and the
// connection holding it then keeps the write lock after it is closed.
func TestACancelledContextDoesNotInterruptAWrite(t *testing.T) {
	registerCancelFunction()

	account := newDatabase(t)

	general, err := Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { general.Close() })

	for name, db := range map[string]*sql.DB{"account writer": account.Writer(), "general handle": general} {
		t.Run(name, func(t *testing.T) {
			background := context.Background()
			if _, err := db.ExecContext(background, "CREATE TABLE t (n INTEGER)"); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(background)
			defer cancel()
			cancelDuringStatement.Store(&cancel)

			if _, err := db.ExecContext(ctx, cancelledInsert); err != nil {
				t.Fatalf("a write was interrupted by its cancelled context: %v", err)
			}
			if got := countT(t, db); got != 20000 {
				t.Fatalf("the write stored %d rows, want 20000", got)
			}

			stmt, err := db.PrepareContext(background, cancelledInsert)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := stmt.Close(); err != nil {
					t.Errorf("close statement: %v", err)
				}
			}()

			ctx, cancel = context.WithCancel(background)
			defer cancel()
			cancelDuringStatement.Store(&cancel)

			if _, err := stmt.ExecContext(ctx); err != nil {
				t.Fatalf("a prepared write was interrupted by its cancelled context: %v", err)
			}
			if got := countT(t, db); got != 40000 {
				t.Fatalf("the prepared write left %d rows, want 40000", got)
			}

			if _, err := db.ExecContext(background, "INSERT INTO t (n) VALUES (0)"); err != nil {
				t.Fatalf("the connection was not usable after the cancellation: %v", err)
			}
		})
	}
}

// TestACancelledTransactionLeavesTheWriteLockFree is the property the account
// shard depends on: a caller giving up mid-transaction rolls it back, and the
// next writer on the same file gets the lock at once instead of waiting forever.
func TestACancelledTransactionLeavesTheWriteLockFree(t *testing.T) {
	registerCancelFunction()

	path := filepath.Join(t.TempDir(), "analytics.db")
	first, err := OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("close first: %v", err)
		}
	})

	second, err := OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second: %v", err)
		}
	})

	background := context.Background()
	if _, err := first.Writer().ExecContext(background, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(background)
	defer cancel()

	tx, err := first.Writer().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	cancelDuringStatement.Store(&cancel)
	if _, err := tx.ExecContext(ctx, cancelledInsert); err != nil {
		t.Fatalf("the statement in flight was interrupted: %v", err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("a cancelled transaction committed")
	}

	start := time.Now()
	if _, err := second.Writer().ExecContext(background, "INSERT INTO t (n) VALUES (1)"); err != nil {
		t.Fatalf("the next writer could not take the lock: %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the next writer waited %s for the lock", waited)
	}

	if got := countT(t, first.Writer()); got != 1 {
		t.Fatalf("the table holds %d rows, want only the second writer's 1", got)
	}
}
