package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// sqlserver hierarchy:
//
//	database → schema
//	schema → Tables | Views | Procedures | Functions
//	table  → Columns | Indexes | Foreign Keys
//	view   → Columns
type sqlserver struct{}

// mssqlObject resolves schema.table to an object_id, quoting both parts.
const mssqlObject = `OBJECT_ID(QUOTENAME(@p1) + '.' + QUOTENAME(@p2))`

func (sqlserver) Children(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	if p == nil {
		// Databases the login can open; system databases only when they are
		// the DSN's default.
		return queryObjects(ctx, conn, func(name, detail string) Object {
			return Object{Kind: KindDatabase, Name: name, Detail: detail, Database: name}
		}, `
			SELECT name, CASE WHEN name = DB_NAME() THEN 'default' ELSE '' END
			FROM sys.databases
			WHERE HAS_DBACCESS(name) = 1 AND (database_id > 4 OR name = DB_NAME())
			ORDER BY name`)
	}

	switch p.Kind {
	case KindDatabase:
		// Every schema except the fixed database-role ones (>= 16384) and
		// the built-in system ones, marked empty when the login sees no
		// objects, user types or XML schema collections in it. One query
		// per database; sys.objects is indexed by schema.
		return queryObjects(ctx, conn, schemaObject, `
			SELECT s.name,
			       CASE WHEN s.name = SCHEMA_NAME()
			              OR EXISTS (SELECT 1 FROM sys.objects o
			                         WHERE o.schema_id = s.schema_id AND o.is_ms_shipped = 0)
			              OR EXISTS (SELECT 1 FROM sys.types t
			                         WHERE t.schema_id = s.schema_id AND t.is_user_defined = 1)
			              OR EXISTS (SELECT 1 FROM sys.xml_schema_collections x
			                         WHERE x.schema_id = s.schema_id AND x.xml_collection_id > 1)
			            THEN '' ELSE 'empty' END
			FROM sys.schemas s
			WHERE s.schema_id < 16384 AND s.name NOT IN ('sys', 'INFORMATION_SCHEMA')
			ORDER BY s.name`)
	case KindSchema:
		return []Object{
			folder("Tables", KindTable, p.Schema, ""),
			folder("Views", KindView, p.Schema, ""),
			folder("Procedures", KindProcedure, p.Schema, ""),
			folder("Functions", KindFunction, p.Schema, ""),
		}, nil
	case KindTable:
		return []Object{
			folder("Columns", KindColumn, p.Schema, p.Name),
			folder("Indexes", KindIndex, p.Schema, p.Name),
			folder("Foreign Keys", KindForeignKey, p.Schema, p.Name),
		}, nil
	case KindView:
		return []Object{folder("Columns", KindColumn, p.Schema, p.Name)}, nil
	case KindFolder:
		return sqlserverFolder(ctx, conn, p)
	}
	return nil, nil
}

func sqlserverFolder(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	objects := func(kind Kind, types string) ([]Object, error) {
		return queryObjects(ctx, conn, func(name, _ string) Object {
			return Object{Kind: kind, Name: name, Schema: p.Schema}
		}, `
			SELECT o.name, '' FROM sys.objects o
			JOIN sys.schemas s ON s.schema_id = o.schema_id
			WHERE s.name = @p1 AND o.type IN (`+types+`) AND o.is_ms_shipped = 0
			ORDER BY o.name`, p.Schema)
	}

	switch p.Contains {
	case KindTable:
		return objects(KindTable, "'U'")
	case KindView:
		return objects(KindView, "'V'")
	case KindProcedure:
		return objects(KindProcedure, "'P', 'PC'")
	case KindFunction:
		return objects(KindFunction, "'FN', 'IF', 'TF', 'FS', 'FT'")
	case KindColumn:
		return sqlserverColumns(ctx, conn, p.Schema, p.Table)
	case KindForeignKey:
		return foreignKeyObjects(sqlserver{}.foreignKeys(ctx, conn, p.Schema, p.Table))
	case KindIndex:
		return queryIndexes(ctx, conn, `
			SELECT name, is_unique, is_primary_key FROM sys.indexes
			WHERE object_id = `+mssqlObject+` AND name IS NOT NULL
			ORDER BY name`, p.Schema, p.Table)
	}
	return nil, fmt.Errorf("sqlserver: unexpected folder %q", p.Name)
}

func sqlserverColumns(ctx context.Context, conn *sql.DB, schema, table string) ([]Object, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.name, t.name, c.max_length, c.precision, c.scale,
		       CAST(CASE WHEN EXISTS (
		           SELECT 1 FROM sys.index_columns ic
		           JOIN sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id
		           WHERE i.is_primary_key = 1
		             AND ic.object_id = c.object_id AND ic.column_id = c.column_id
		       ) THEN 1 ELSE 0 END AS bit)
		FROM sys.columns c
		JOIN sys.types t ON t.user_type_id = c.user_type_id
		WHERE c.object_id = `+mssqlObject+`
		ORDER BY c.column_id`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Object
	for rows.Next() {
		var name, typ string
		var length, precision, scale int
		var primary bool
		if err := rows.Scan(&name, &typ, &length, &precision, &scale, &primary); err != nil {
			return nil, err
		}
		out = append(out, Object{
			Kind:    KindColumn,
			Name:    name,
			Detail:  mssqlType(typ, length, precision, scale),
			Primary: primary,
		})
	}
	return out, rows.Err()
}

// mssqlType renders a column type the way it is written in DDL.
func mssqlType(name string, length, precision, scale int) string {
	size := func(n int) string {
		if n == -1 {
			return "max"
		}
		return fmt.Sprint(n)
	}
	switch strings.ToLower(name) {
	case "varchar", "char", "varbinary", "binary":
		return fmt.Sprintf("%s(%s)", name, size(length))
	case "nvarchar", "nchar":
		if length > 0 {
			length /= 2
		}
		return fmt.Sprintf("%s(%s)", name, size(length))
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", name, precision, scale)
	}
	return name
}

// mssqlWithDatabase sets the database of a URL or key=value DSN.
func mssqlWithDatabase(dsn, database string) (string, error) {
	if strings.HasPrefix(strings.ToLower(dsn), "sqlserver://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("database", database)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	// key=value form: drop any database setting and append ours.
	var parts []string
	for _, part := range strings.Split(dsn, ";") {
		key, _, _ := strings.Cut(part, "=")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "database", "initial catalog", "":
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(append(parts, "database="+database), ";"), nil
}
