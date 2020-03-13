package analyzer

import (
	"github.com/src-d/go-mysql-server/memory"
	"github.com/src-d/go-mysql-server/sql"
	"github.com/src-d/go-mysql-server/sql/plan"
)

const dualTableName = "dual"

var dualTable = func() sql.Table {
	t := memory.NewTable(dualTableName, sql.Schema{
		{Name: "dummy", Source: dualTableName, Type: sql.LongText, Nullable: false},
	})
	_ = t.Insert(sql.NewEmptyContext(), sql.NewRow("x"))
	return t
}()

func resolveTables(ctx *sql.Context, a *Analyzer, n sql.Node) (sql.Node, error) {
	span, _ := ctx.Span("resolve_tables")
	defer span.Finish()

	a.Log("resolve table, node of type: %T", n)
	return plan.TransformUp(n, func(n sql.Node) (sql.Node, error) {
		a.Log("transforming node of type: %T", n)
		if n.Resolved() {
			return n, nil
		}

		t, ok := n.(*plan.UnresolvedTable)
		if !ok {
			return n, nil
		}

		name := t.Name()
		db := t.Database
		if db == "" {
			db = a.Catalog.CurrentDatabase()
		}

		if t.AsOf != nil {
			asOf, err := t.AsOf.Eval(ctx, nil)
			if err != nil {
				return nil, err
			}

			rt, err := a.Catalog.TableAsOf(ctx, db, name, asOf)
			if err != nil {
				return handleTableLookupFailure(err, name, a, t)
			}

			return plan.NewResolvedTable(rt), nil
		}

		rt, err := a.Catalog.Table(ctx, db, name)
		if err != nil {
			return handleTableLookupFailure(err, name, a, t)
		}

		a.Log("table resolved: %q", t.Name())
		return plan.NewResolvedTable(rt), nil
	})
}

func handleTableLookupFailure(err error, tableName string, a *Analyzer, t *plan.UnresolvedTable) (sql.Node, error) {
	if sql.ErrTableNotFound.Is(err) {
		if tableName == dualTableName {
			a.Log("table resolved: %q", t.Name())
			return plan.NewResolvedTable(dualTable), nil
		}
	}
	return nil, err
}
