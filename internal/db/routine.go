package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Routine is what calling a function or procedure takes.
type Routine struct {
	// Kind is "function" (called with SELECT * FROM on PostgreSQL),
	// "scalar function" or "table function" (SQL Server), or "procedure".
	Kind   string
	Params []Param
}

// Param is a routine's parameter, in order.
type Param struct {
	Name   string // "" when unnamed; with its @ on SQL Server
	Type   string
	Output bool // OUT or INOUT, or OUTPUT on SQL Server
}

// Routine reads the parameters of a function or procedure from the
// explorer. On PostgreSQL, o.Detail (the identity arguments) picks the
// overload.
func (p *Pool) Routine(ctx context.Context, o Object) (*Routine, error) {
	conn, err := p.DB(ctx, o.Database)
	if err != nil {
		return nil, err
	}
	switch p.cfg.Driver {
	case Postgres:
		return pgRoutine(ctx, conn, o)
	case SQLServer:
		return mssqlRoutine(ctx, conn, o)
	case MySQL:
		return mysqlRoutine(ctx, conn, o)
	}
	return nil, fmt.Errorf("%s has no routines", p.cfg.Driver.Label())
}

func pgRoutine(ctx context.Context, conn *sql.DB, o Object) (*Routine, error) {
	args := strings.TrimSuffix(strings.TrimPrefix(o.Detail, "("), ")")
	var oid uint32
	var kind string
	err := conn.QueryRowContext(ctx, `
		SELECT p.oid, p.prokind::text FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.proname = $2 AND pg_get_function_identity_arguments(p.oid) = $3
		LIMIT 1`, o.Schema, o.Name, args).Scan(&oid, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s(%s) no longer exists", o.Name, args)
	}
	if err != nil {
		return nil, err
	}
	r := &Routine{Kind: "function"}
	if kind == "p" {
		r.Kind = "procedure"
	}

	// Every argument with its mode: i(n), o(ut), b (inout), v(ariadic),
	// t (a column of RETURNS TABLE, not passed).
	rows, err := conn.QueryContext(ctx, `
		SELECT coalesce(a.name, ''), format_type(a.type, NULL), coalesce(a.mode, 'i')
		FROM pg_proc p,
		     unnest(coalesce(p.proallargtypes, p.proargtypes::oid[]), p.proargnames, p.proargmodes::text[])
		         WITH ORDINALITY AS a(type, name, mode, n)
		WHERE p.oid = $1 AND a.type IS NOT NULL
		ORDER BY a.n`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var prm Param
		var mode string
		if err := rows.Scan(&prm.Name, &prm.Type, &mode); err != nil {
			return nil, err
		}
		switch {
		case mode == "t":
			continue
		case mode == "o" && r.Kind == "function":
			continue // a function's OUT arguments are its result
		}
		prm.Output = mode == "o" || mode == "b"
		r.Params = append(r.Params, prm)
	}
	return r, rows.Err()
}

func mssqlRoutine(ctx context.Context, conn *sql.DB, o Object) (*Routine, error) {
	var typ string
	err := conn.QueryRowContext(ctx, `SELECT RTRIM(type) FROM sys.objects WHERE object_id = `+mssqlObject,
		o.Schema, o.Name).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s no longer exists", o.Name)
	}
	if err != nil {
		return nil, err
	}
	r := &Routine{}
	switch typ {
	case "P", "PC":
		r.Kind = "procedure"
	case "IF", "TF", "FT":
		r.Kind = "table function"
	default:
		r.Kind = "scalar function"
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT p.name, TYPE_NAME(p.user_type_id), p.max_length, p.precision, p.scale, p.is_output
		FROM sys.parameters p
		WHERE p.object_id = `+mssqlObject+` AND p.parameter_id > 0
		ORDER BY p.parameter_id`, o.Schema, o.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var prm Param
		var typ string
		var length, precision, scale int
		if err := rows.Scan(&prm.Name, &typ, &length, &precision, &scale, &prm.Output); err != nil {
			return nil, err
		}
		prm.Type = mssqlType(typ, length, precision, scale)
		r.Params = append(r.Params, prm)
	}
	return r, rows.Err()
}
