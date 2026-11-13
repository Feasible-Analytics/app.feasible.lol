//
// uninterruptible.go
// Connections that finish every statement they start.
//
// Created: 2026-11-13
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
)

// sqliteConn is the part of the driver's connection the wrapper forwards.
// Asserting it once at connect time keeps every forwarding method a plain call.
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// sqliteStmt is the part of the driver's prepared statement the wrapper
// forwards.
type sqliteStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

// openUninterruptible opens a handle that can write. A cancelled context makes
// the driver interrupt the statement in flight, and the driver can then drop
// that statement without finalizing it; closing the connection leaves it alive
// in SQLite, holding its transaction and the database's write lock with nothing
// left to release them. So a handle that writes never hands the driver a
// context that can be cancelled. database/sql still checks the context before
// each call and rolls a cancelled transaction back between statements.
func openUninterruptible(dsn string) (*sql.DB, error) {
	registered, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, err
	}

	connector := uninterruptibleConnector{dsn: dsn, driver: registered.Driver()}
	registered.Close()

	return sql.OpenDB(connector), nil
}

// uninterruptibleConnector opens connections through the registered driver.
type uninterruptibleConnector struct {
	dsn    string
	driver driver.Driver
}

// Connect opens one driver connection and wraps it. A driver without the
// context interfaces is refused rather than half-wrapped, because a method the
// wrapper does not forward is a method database/sql silently stops using.
func (c uninterruptibleConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}

	inner, ok := conn.(sqliteConn)
	if !ok {
		_ = conn.Close() // the refusal below is the error that matters
		return nil, fmt.Errorf("store: driver connection %T lacks the context interfaces", conn)
	}

	return &uninterruptibleConn{inner: inner}, nil
}

// Driver reports the driver the connector opens through.
func (c uninterruptibleConnector) Driver() driver.Driver {
	return c.driver
}

// uninterruptibleConn forwards to the driver connection with cancellation removed.
type uninterruptibleConn struct {
	inner sqliteConn
}

// Prepare prepares a statement and wraps it so its execution is not
// cancellable either.
func (c *uninterruptibleConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext prepares a statement and wraps it.
func (c *uninterruptibleConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.inner.PrepareContext(context.WithoutCancel(ctx), query)
	if err != nil {
		return nil, err
	}

	inner, ok := stmt.(sqliteStmt)
	if !ok {
		_ = stmt.Close() // the refusal below is the error that matters
		return nil, fmt.Errorf("store: driver statement %T lacks the context interfaces", stmt)
	}

	return &uninterruptibleStmt{inner: inner}, nil
}

// Close closes the driver connection.
func (c *uninterruptibleConn) Close() error {
	return c.inner.Close()
}

// Begin starts a transaction with default options.
func (c *uninterruptibleConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx starts a transaction. Lock waits are bounded by busy_timeout, not by
// the caller.
func (c *uninterruptibleConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.inner.BeginTx(context.WithoutCancel(ctx), opts)
}

// ExecContext runs a statement to completion.
func (c *uninterruptibleConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.inner.ExecContext(context.WithoutCancel(ctx), query, args)
}

// QueryContext runs a query without letting a cancellation interrupt it.
// database/sql still closes the rows when the caller's context ends.
func (c *uninterruptibleConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.inner.QueryContext(context.WithoutCancel(ctx), query, args)
}

// Ping checks the driver connection.
func (c *uninterruptibleConn) Ping(ctx context.Context) error {
	return c.inner.Ping(context.WithoutCancel(ctx))
}

// ResetSession lets the driver discard a connection it no longer trusts.
func (c *uninterruptibleConn) ResetSession(ctx context.Context) error {
	return c.inner.ResetSession(ctx)
}

// IsValid lets the driver keep a broken connection out of the pool.
func (c *uninterruptibleConn) IsValid() bool {
	return c.inner.IsValid()
}

// uninterruptibleStmt forwards to a driver statement with cancellation removed.
type uninterruptibleStmt struct {
	inner sqliteStmt
}

// Close finalizes the driver statement.
func (s *uninterruptibleStmt) Close() error {
	return s.inner.Close()
}

// NumInput reports the driver's placeholder count.
func (s *uninterruptibleStmt) NumInput() int {
	return s.inner.NumInput()
}

// Exec runs the statement with positional arguments.
func (s *uninterruptibleStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

// Query runs the statement with positional arguments.
func (s *uninterruptibleStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

// ExecContext runs the statement to completion.
func (s *uninterruptibleStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.inner.ExecContext(context.WithoutCancel(ctx), args)
}

// QueryContext runs the statement without letting a cancellation interrupt
// it.
func (s *uninterruptibleStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.inner.QueryContext(context.WithoutCancel(ctx), args)
}

// namedValues numbers positional arguments the way database/sql does.
func namedValues(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, value := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}

	return named
}
