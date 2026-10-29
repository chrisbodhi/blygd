// Package tx runs database transactions and marks their contexts, so code
// that must not run inside one can tell (CLAUDE.md: no network inside a
// transaction). internal/fetch refuses a marked context. Every transaction
// in blygd starts here; internal/archtest fails if one starts anywhere else.
package tx

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type key struct{}

// Beginner is what a transaction starts from: a pool or a connection.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Run runs fn in a transaction, committing when it returns nil. fn gets a
// context marked as inside a transaction: name its parameter ctx, so the
// unmarked one from outside is shadowed and cannot be used by mistake.
func Run(ctx context.Context, db Beginner, opts pgx.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx = context.WithValue(ctx, key{}, true)
	return pgx.BeginTxFunc(ctx, db, opts, func(t pgx.Tx) error { return fn(ctx, t) })
}

// Active reports whether ctx is inside a transaction Run started.
func Active(ctx context.Context) bool {
	v, _ := ctx.Value(key{}).(bool)
	return v
}
