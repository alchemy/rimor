package db

import (
	"context"
	"database/sql"
	"errors"
)

// pgKeys lists foreign keys as (name, schema, table, ref schema, ref
// table, column, ref column) rows, one per column pair in key order.
// where filters pg_constraint con.
const pgKeys = `
	SELECT con.conname, sn.nspname, sc.relname, rn.nspname, rc.relname,
	       sa.attname, ra.attname
	FROM pg_constraint con
	JOIN pg_class sc ON sc.oid = con.conrelid
	JOIN pg_namespace sn ON sn.oid = sc.relnamespace
	JOIN pg_class rc ON rc.oid = con.confrelid
	JOIN pg_namespace rn ON rn.oid = rc.relnamespace
	CROSS JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY AS k(src, ref, n)
	JOIN pg_attribute sa ON sa.attrelid = con.conrelid AND sa.attnum = k.src
	JOIN pg_attribute ra ON ra.attrelid = con.confrelid AND ra.attnum = k.ref
	WHERE con.contype = 'f' AND `

func (postgres) foreignKeys(ctx context.Context, conn *sql.DB, schema, table string) ([]ForeignKey, error) {
	return queryKeys(ctx, conn, pgKeys+`con.conrelid = `+pgRelation+`
		ORDER BY con.conname, k.n`, schema, table)
}

func (pg postgres) describe(ctx context.Context, conn *sql.DB, schema, name string) (*Table, error) {
	if schema == "" {
		if err := conn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
			return nil, err
		}
	}
	t := &Table{Schema: schema, Name: name}
	var relkind string
	err := conn.QueryRowContext(ctx, `
		SELECT c.relkind::text, coalesce(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'f', 'v', 'm')`,
		schema, name).Scan(&relkind, &t.Comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch relkind {
	case "v":
		t.Kind = "view"
	case "m":
		t.Kind = "materialized view"
	default:
		t.Kind = "table"
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		       coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
		       EXISTS (SELECT 1 FROM pg_index i
		               WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY(i.indkey)),
		       coalesce(col_description(a.attrelid, a.attnum), '')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = `+pgRelation+` AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c TableColumn
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.PrimaryKey, &c.Comment); err != nil {
			return nil, err
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	idx, err := conn.QueryContext(ctx, `
		SELECT c.relname, i.indisunique, i.indisprimary,
		       (SELECT string_agg(pg_get_indexdef(i.indexrelid, k, true), chr(31) ORDER BY k)
		        FROM generate_series(1, i.indnkeyatts) k)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = `+pgRelation+`
		ORDER BY c.relname`, schema, name)
	if err != nil {
		return nil, err
	}
	defer idx.Close()
	for idx.Next() {
		var x TableIndex
		var cols string
		if err := idx.Scan(&x.Name, &x.Unique, &x.Primary, &cols); err != nil {
			return nil, err
		}
		x.Columns = splitList(cols)
		t.Indexes = append(t.Indexes, x)
	}
	if err := idx.Err(); err != nil {
		return nil, err
	}

	if t.ForeignKeys, err = pg.foreignKeys(ctx, conn, schema, name); err != nil {
		return nil, err
	}
	t.ReferencedBy, err = queryKeys(ctx, conn, pgKeys+`con.confrelid = `+pgRelation+`
		ORDER BY sn.nspname, sc.relname, con.conname, k.n`, schema, name)
	return t, err
}

func (postgres) search(ctx context.Context, conn *sql.DB, pattern string, limit int) ([]Match, error) {
	return queryMatches(ctx, conn, `
		WITH user_schemas AS (
		    SELECT oid, nspname FROM pg_namespace
		    WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
		)
		SELECT * FROM (
		    SELECT CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' ELSE 'table' END AS kind,
		           n.nspname AS schema, c.relname AS name, '' AS col, '' AS typ
		    FROM pg_class c JOIN user_schemas n ON n.oid = c.relnamespace
		    WHERE c.relkind IN ('r', 'p', 'f', 'v', 'm') AND lower(c.relname) LIKE $1
		    UNION ALL
		    SELECT CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
		           n.nspname, p.proname, '', ''
		    FROM pg_proc p JOIN user_schemas n ON n.oid = p.pronamespace
		    WHERE p.prokind IN ('f', 'p') AND lower(p.proname) LIKE $1
		    UNION ALL
		    SELECT 'column', n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
		    FROM pg_attribute a
		    JOIN pg_class c ON c.oid = a.attrelid
		    JOIN user_schemas n ON n.oid = c.relnamespace
		    WHERE c.relkind IN ('r', 'p', 'f', 'v', 'm') AND a.attnum > 0 AND NOT a.attisdropped
		      AND lower(a.attname) LIKE $1
		) m
		ORDER BY kind = 'column', schema, name, col
		LIMIT $2`, pattern, limit)
}

func (postgres) version(ctx context.Context, conn *sql.DB) (string, error) {
	var v string
	err := conn.QueryRowContext(ctx, `SELECT current_setting('server_version')`).Scan(&v)
	return "PostgreSQL " + v, err
}
