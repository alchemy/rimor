package db

import (
	"context"
	"database/sql"
	"errors"
)

// sqliteKeys scans rows of (table, id, ref table, column, ref column) into
// foreign keys. SQLite's keys have no names; id tells them apart within a
// table. A NULL ref column means the referenced table's primary key.
func sqliteKeys(ctx context.Context, conn *sql.DB, query string, args ...any) ([]ForeignKey, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ForeignKey
	lastTable, lastID := "", -1
	for rows.Next() {
		var table, refTable, col string
		var refCol sql.NullString
		var id int
		if err := rows.Scan(&table, &id, &refTable, &col, &refCol); err != nil {
			return nil, err
		}
		if table != lastTable || id != lastID {
			out = append(out, ForeignKey{Table: table, RefTable: refTable})
			lastTable, lastID = table, id
		}
		fk := &out[len(out)-1]
		fk.Columns = append(fk.Columns, col)
		fk.RefColumns = append(fk.RefColumns, refCol.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// Fill in references to a primary key, which SQLite leaves implicit.
	for i, fk := range out {
		if fk.RefColumns[0] != "" {
			continue
		}
		pk, err := sqlitePrimaryKey(ctx, conn, fk.RefTable)
		if err != nil {
			return nil, err
		}
		if len(pk) == len(fk.Columns) {
			out[i].RefColumns = pk
		}
	}
	return out, nil
}

func sqlitePrimaryKey(ctx context.Context, conn *sql.DB, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (sqlite) foreignKeys(ctx context.Context, conn *sql.DB, _, table string) ([]ForeignKey, error) {
	return sqliteKeys(ctx, conn, `
		SELECT ?1, id, "table", "from", "to" FROM pragma_foreign_key_list(?1) ORDER BY id, seq`, table)
}

func (lite sqlite) describe(ctx context.Context, conn *sql.DB, _, name string) (*Table, error) {
	t := &Table{}
	err := conn.QueryRowContext(ctx, `
		SELECT type, name FROM sqlite_master
		WHERE type IN ('table', 'view') AND name = ? COLLATE NOCASE`, name).Scan(&t.Kind, &t.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT name, type, "notnull" = 0, coalesce(dflt_value, ''), pk > 0
		FROM pragma_table_info(?) ORDER BY cid`, t.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objs []Object
	for rows.Next() {
		var c TableColumn
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.PrimaryKey); err != nil {
			return nil, err
		}
		t.Columns = append(t.Columns, c)
		objs = append(objs, Object{Kind: KindColumn, Name: c.Name, Detail: c.Type, Primary: c.PrimaryKey})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	// The rowid, as the explorer finds it.
	if objs, err = sqliteRowid(ctx, conn, t.Name, objs); err != nil {
		return nil, err
	}
	if len(objs) > len(t.Columns) {
		rowid := TableColumn{Name: objs[0].Name, Type: "INTEGER", Implicit: true}
		t.Columns = append([]TableColumn{rowid}, t.Columns...)
	}
	for i, o := range objs {
		if o.Detail == "rowid" {
			t.Columns[i].Nullable = false // the rowid under another name
		}
	}

	idx, err := conn.QueryContext(ctx, `
		SELECT il.name, il."unique", il.origin = 'pk', coalesce(ii.name, '(expression)')
		FROM pragma_index_list(?1) il, pragma_index_info(il.name) ii
		ORDER BY il.name, ii.seqno`, t.Name)
	if err != nil {
		return nil, err
	}
	defer idx.Close()
	for idx.Next() {
		var x TableIndex
		var col string
		if err := idx.Scan(&x.Name, &x.Unique, &x.Primary, &col); err != nil {
			return nil, err
		}
		if n := len(t.Indexes); n > 0 && t.Indexes[n-1].Name == x.Name {
			t.Indexes[n-1].Columns = append(t.Indexes[n-1].Columns, col)
			continue
		}
		x.Columns = []string{col}
		t.Indexes = append(t.Indexes, x)
	}
	if err := idx.Err(); err != nil {
		return nil, err
	}
	idx.Close()

	if t.ForeignKeys, err = lite.foreignKeys(ctx, conn, "", t.Name); err != nil {
		return nil, err
	}
	t.ReferencedBy, err = sqliteKeys(ctx, conn, `
		SELECT m.name, f.id, f."table", f."from", f."to"
		FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = ? COLLATE NOCASE
		ORDER BY m.name, f.id, f.seq`, t.Name)
	return t, err
}

func (sqlite) search(ctx context.Context, conn *sql.DB, pattern string, limit int) ([]Match, error) {
	return queryMatches(ctx, conn, `
		SELECT kind, '', name, col, typ FROM (
		    SELECT type AS kind, name, '' AS col, '' AS typ FROM sqlite_master
		    WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		      AND lower(name) LIKE ?1 ESCAPE '\'
		    UNION ALL
		    SELECT 'column', m.name, p.name, p.type
		    FROM sqlite_master m, pragma_table_info(m.name) p
		    WHERE m.type IN ('table', 'view') AND m.name NOT LIKE 'sqlite\_%' ESCAPE '\'
		      AND lower(p.name) LIKE ?1 ESCAPE '\'
		)
		ORDER BY kind = 'column', name, col
		LIMIT ?2`, pattern, limit)
}

func (sqlite) version(ctx context.Context, conn *sql.DB) (string, error) {
	var v string
	err := conn.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&v)
	return "SQLite " + v, err
}
