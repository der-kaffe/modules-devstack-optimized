package mysql

import (
	"context"
	"database/sql"
	"errors"
)

// Hot static reads (show, PATCH/DELETE locks, data state). Parameters stay
// bound server-side (InterpolateParams=false); reusing the prepared statement
// saves the PREPARE round trip on every call.
var hotStatements = []string{
	readImageQuery, lockImageQuery, oneImagePropsQuery, oneImageTagsQuery,
	readDataImage, readDataOperation, lockDataImage, lockDataOperation,
}

// PrepareStatements prepares the hot reads once. Call it at startup, outside
// any transaction: preparing on *sql.DB takes a pool connection, which inside
// a transaction could wait on a pool held entirely by transactions. Inside a
// transaction tx.StmtContext then reuses (or once prepares) the statement on
// the transaction's own connection. On error nothing is kept.
func (r *Repository) PrepareStatements(ctx context.Context) error {
	if r.stmts != nil {
		return errors.New("statements already prepared")
	}
	prepared := make(map[string]*sql.Stmt, len(hotStatements))
	for _, q := range hotStatements {
		st, err := r.db.PrepareContext(ctx, q)
		if err != nil {
			for _, p := range prepared {
				_ = p.Close()
			}
			return err
		}
		prepared[q] = st
	}
	r.stmts = prepared
	return nil
}

func (r *Repository) queryRow(ctx context.Context, tx *sql.Tx, q string, args ...any) *sql.Row {
	if st := r.stmts[q]; st != nil {
		return tx.StmtContext(ctx, st).QueryRowContext(ctx, args...)
	}
	return tx.QueryRowContext(ctx, q, args...)
}

func (r *Repository) query(ctx context.Context, tx *sql.Tx, q string, args ...any) (*sql.Rows, error) {
	if st := r.stmts[q]; st != nil {
		return tx.StmtContext(ctx, st).QueryContext(ctx, args...)
	}
	return tx.QueryContext(ctx, q, args...)
}
