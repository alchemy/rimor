package db

import (
	"context"
	"database/sql"
	"fmt"
)

// postgres hierarchy:
//
//	database → schema
//	schema → Tables | Views | Materialized Views | Functions | Sequences
//	table  → Columns | Indexes
//	view   → Columns
type postgres struct{}

// pgRelation resolves schema.table to an oid, quoting both parts.
const pgRelation = `(quote_ident($1) || '.' || quote_ident($2))::regclass`

func (postgres) Children(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	if p == nil {
		return queryObjects(ctx, conn, func(name, detail string) Object {
			return Object{Kind: KindDatabase, Name: name, Detail: detail, Database: name}
		}, `
			SELECT datname, CASE WHEN datname = current_database() THEN 'default' ELSE '' END
			FROM pg_database
			WHERE datallowconn AND NOT datistemplate
			  AND has_database_privilege(datname, 'CONNECT')
			ORDER BY datname`)
	}

	switch p.Kind {
	case KindDatabase:
		return queryObjects(ctx, conn, func(name, _ string) Object {
			return Object{Kind: KindSchema, Name: name, Schema: name}
		}, `
			SELECT nspname, ''
			FROM pg_namespace
			WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
			ORDER BY nspname`)
	case KindSchema:
		return []Object{
			folder("Tables", KindTable, p.Schema, ""),
			folder("Views", KindView, p.Schema, ""),
			folder("Materialized Views", KindMatView, p.Schema, ""),
			folder("Functions", KindFunction, p.Schema, ""),
			folder("Sequences", KindSequence, p.Schema, ""),
		}, nil
	case KindTable:
		return []Object{
			folder("Columns", KindColumn, p.Schema, p.Name),
			folder("Indexes", KindIndex, p.Schema, p.Name),
		}, nil
	case KindView, KindMatView:
		return []Object{folder("Columns", KindColumn, p.Schema, p.Name)}, nil
	case KindFolder:
		return postgresFolder(ctx, conn, p)
	}
	return nil, nil
}

func postgresFolder(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	inSchema := func(kind Kind) func(name, detail string) Object {
		return func(name, detail string) Object {
			return Object{Kind: kind, Name: name, Detail: detail, Schema: p.Schema}
		}
	}
	relations := func(kind Kind, relkinds string) ([]Object, error) {
		return queryObjects(ctx, conn, inSchema(kind), `
			SELECT c.relname, ''
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relkind::text = ANY(string_to_array($2, ','))
			ORDER BY c.relname`, p.Schema, relkinds)
	}

	switch p.Contains {
	case KindTable:
		return relations(KindTable, "r,p,f")
	case KindView:
		return relations(KindView, "v")
	case KindMatView:
		return relations(KindMatView, "m")
	case KindSequence:
		return relations(KindSequence, "S")
	case KindFunction:
		return queryObjects(ctx, conn, inSchema(KindFunction), `
			SELECT p.proname, '(' || pg_get_function_identity_arguments(p.oid) || ')'
			FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = $1
			ORDER BY p.proname, 2`, p.Schema)
	case KindColumn:
		return queryColumns(ctx, conn, `
			SELECT a.attname,
			       format_type(a.atttypid, a.atttypmod),
			       EXISTS (
			           SELECT 1 FROM pg_index i
			           WHERE i.indrelid = a.attrelid AND i.indisprimary
			             AND a.attnum = ANY(i.indkey)
			       )
			FROM pg_attribute a
			WHERE a.attrelid = `+pgRelation+`
			  AND a.attnum > 0 AND NOT a.attisdropped
			ORDER BY a.attnum`, p.Schema, p.Table)
	case KindIndex:
		return queryIndexes(ctx, conn, `
			SELECT c.relname, i.indisunique, i.indisprimary
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			WHERE i.indrelid = `+pgRelation+`
			ORDER BY c.relname`, p.Schema, p.Table)
	}
	return nil, fmt.Errorf("postgres: unexpected folder %q", p.Name)
}
