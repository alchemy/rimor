package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
)

// A tab's session can be left inside a transaction the user opened, with
// BEGIN, BEGIN TRANSACTION or SAVEPOINT, or that SQL Server opened under
// SET IMPLICIT_TRANSACTIONS ON. Before the session is closed, which rolls
// it back, the UI asks the user what to do; these find out whether there
// is anything to ask about, and end the transaction as answered.
//
// Both must run while the session is idle: not during a statement or the
// fetch of its rows.

// TxState is a session's transaction, as the server reports it.
type TxState struct {
	Open bool
	// Failed means only a rollback can end it: PostgreSQL after an error in
	// the transaction, SQL Server when XACT_STATE() is -1.
	Failed bool
	// Levels is SQL Server's @@TRANCOUNT: nested BEGIN TRANSACTIONs, each
	// needing its COMMIT. 1 for an open transaction elsewhere.
	Levels int
}

// Transaction reports whether the session is inside a transaction.
func Transaction(ctx context.Context, s *sql.Conn, d Driver) (TxState, error) {
	switch d {
	case Postgres:
		var status byte
		err := s.Raw(func(dc any) error {
			c, ok := dc.(*stdlib.Conn)
			if !ok {
				return fmt.Errorf("not a pgx connection: %T", dc)
			}
			status = c.Conn().PgConn().TxStatus()
			return nil
		})
		if err != nil {
			return TxState{}, err
		}
		// 'I' idle, 'T' in a transaction, 'E' in a failed one.
		switch status {
		case 'T':
			return TxState{Open: true, Levels: 1}, nil
		case 'E':
			return TxState{Open: true, Failed: true, Levels: 1}, nil
		}
		return TxState{}, nil

	case SQLServer:
		var count, state int
		if err := s.QueryRowContext(ctx, "SELECT @@TRANCOUNT, XACT_STATE()").Scan(&count, &state); err != nil {
			return TxState{}, err
		}
		return TxState{Open: count > 0, Failed: state == -1, Levels: count}, nil

	case SQLite:
		// SQLite's own flag (sqlite3_get_autocommit) is not reachable
		// through database/sql, so ask: BEGIN fails inside a transaction;
		// outside one it starts a deferred transaction, which takes no
		// lock until something reads or writes, and ROLLBACK ends it.
		if _, err := s.ExecContext(ctx, "BEGIN"); err != nil {
			if strings.Contains(err.Error(), "within a transaction") {
				return TxState{Open: true, Levels: 1}, nil
			}
			return TxState{}, err
		}
		_, err := s.ExecContext(ctx, "ROLLBACK")
		return TxState{}, err
	}
	return TxState{}, fmt.Errorf("transactions: unknown driver %q", d)
}

// ErrTxFailed refuses to commit a transaction that can only be rolled back.
var ErrTxFailed = errors.New("the transaction failed: it can only be rolled back")

// EndTransaction commits the session's transaction, every level of it, or
// rolls it back.
func EndTransaction(ctx context.Context, s *sql.Conn, d Driver, commit bool) error {
	if commit {
		tx, err := Transaction(ctx, s, d)
		if err != nil {
			return err
		}
		if tx.Failed {
			return ErrTxFailed // PostgreSQL would roll back and call it a commit
		}
	}
	stmt := "ROLLBACK"
	switch {
	case commit && d == SQLServer:
		stmt = "WHILE @@TRANCOUNT > 0 COMMIT TRANSACTION" // one COMMIT per level
	case commit:
		stmt = "COMMIT"
	case d == SQLServer:
		stmt = "IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION" // all levels at once
	}
	_, err := s.ExecContext(ctx, stmt)
	return err
}
