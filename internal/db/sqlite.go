package db

import (
	"context"
	"database/sql"
	"fmt"
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
			return queryColumns(ctx, conn, `
				SELECT name, type, pk > 0 FROM pragma_table_info(?) ORDER BY cid`, p.Table)
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
