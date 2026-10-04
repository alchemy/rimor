package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// sqlite hierarchy (main database only):
//
//	Tables | Views | Indexes | Triggers
//	table → Columns | Indexes
//	view  → Columns
type sqlite struct{}

func (sqlite) Children(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	if p == nil {
		return []Object{
			folder("Tables", KindTable, "", ""),
			folder("Views", KindView, "", ""),
			folder("Indexes", KindIndex, "", ""),
			folder("Triggers", KindTrigger, "", ""),
		}, nil
	}

	switch p.Kind {
	case KindTable:
		return []Object{
			folder("Columns", KindColumn, "", p.Name),
			folder("Indexes", KindIndex, "", p.Name),
		}, nil
	case KindView:
		return []Object{folder("Columns", KindColumn, "", p.Name)}, nil
	case KindFolder:
		return sqliteFolder(ctx, conn, p)
	}
	return nil, nil
}

func sqliteFolder(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	if p.Table != "" {
		switch p.Contains {
		case KindColumn:
			cols, err := queryColumns(ctx, conn, `
				SELECT name, type, pk > 0 FROM pragma_table_info(?) ORDER BY cid`, p.Table)
			if err != nil {
				return nil, err
			}
			return sqliteRowid(ctx, conn, p.Table, cols)
		case KindIndex:
			return queryIndexes(ctx, conn, `
				SELECT name, "unique", origin = 'pk' FROM pragma_index_list(?) ORDER BY name`, p.Table)
		}
		return nil, fmt.Errorf("sqlite: unexpected folder %q", p.Name)
	}

	var typ string
	switch p.Contains {
	case KindTable:
		typ = "table"
	case KindView:
		typ = "view"
	case KindIndex:
		typ = "index"
	case KindTrigger:
		typ = "trigger"
	default:
		return nil, fmt.Errorf("sqlite: unexpected folder %q", p.Name)
	}
	// Indexes and triggers show the table they belong to.
	return queryObjects(ctx, conn, func(name, detail string) Object {
		if p.Contains == KindTable || p.Contains == KindView {
			detail = ""
		}
		return Object{Kind: p.Contains, Name: name, Detail: detail}
	}, `
		SELECT name, tbl_name FROM sqlite_master
		WHERE type = ? AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		ORDER BY name`, typ)
}

// sqliteRowid shows a table's rowid among its columns. Every ordinary
// table has one, unless it is declared WITHOUT ROWID; views have none. A
// sole INTEGER PRIMARY KEY column is the rowid under another name, so it is
// marked rather than listed twice. A column named rowid hides the implicit
// one, which is then reached as oid or _rowid_.
func sqliteRowid(ctx context.Context, conn *sql.DB, table string, cols []Object) ([]Object, error) {
	var typ string
	var withoutRowid bool
	err := conn.QueryRowContext(ctx,
		`SELECT type, wr FROM pragma_table_list WHERE schema = 'main' AND name = ?`, table).Scan(&typ, &withoutRowid)
	if err != nil || typ != "table" || withoutRowid {
		return cols, nil // a view, a WITHOUT ROWID table, or not known
	}

	var keys []int
	for i, c := range cols {
		if c.Primary {
			keys = append(keys, i)
		}
	}
	if len(keys) == 1 && strings.EqualFold(cols[keys[0]].Detail, "INTEGER") {
		cols[keys[0]].Detail = "rowid" // always an integer: the type says nothing more
		return cols, nil
	}

	taken := map[string]bool{}
	for _, c := range cols {
		taken[strings.ToLower(c.Name)] = true
	}
	for _, name := range []string{"rowid", "oid", "_rowid_"} {
		if !taken[name] {
			rowid := Object{Kind: KindColumn, Name: name, Detail: "implicit", Implicit: true}
			return append([]Object{rowid}, cols...), nil
		}
	}
	return cols, nil // every name is taken: the rowid cannot be selected
}
