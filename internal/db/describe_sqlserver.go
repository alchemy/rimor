package db

import (
	"context"
	"database/sql"
	"errors"
)

// mssqlKeys lists foreign keys as (name, schema, table, ref schema, ref
// table, column, ref column) rows, one per column pair in key order.
// where filters sys.foreign_keys fk.
const mssqlKeys = `
	SELECT fk.name, ss.name, st.name, rs.name, rt.name, sc.name, rc.name
	FROM sys.foreign_keys fk
	JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
	JOIN sys.objects st ON st.object_id = fk.parent_object_id
	JOIN sys.schemas ss ON ss.schema_id = st.schema_id
	JOIN sys.objects rt ON rt.object_id = fk.referenced_object_id
	JOIN sys.schemas rs ON rs.schema_id = rt.schema_id
	JOIN sys.columns sc ON sc.object_id = fkc.parent_object_id AND sc.column_id = fkc.parent_column_id
	JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
	WHERE `

func (sqlserver) foreignKeys(ctx context.Context, conn *sql.DB, schema, table string) ([]ForeignKey, error) {
	return queryKeys(ctx, conn, mssqlKeys+`fk.parent_object_id = `+mssqlObject+`
		ORDER BY fk.name, fkc.constraint_column_id`, schema, table)
}

func (ms sqlserver) describe(ctx context.Context, conn *sql.DB, schema, name string) (*Table, error) {
	if schema == "" {
		if err := conn.QueryRowContext(ctx, `SELECT SCHEMA_NAME()`).Scan(&schema); err != nil {
			return nil, err
		}
	}
	t := &Table{Schema: schema, Name: name, Kind: "table"}
	var typ string
	err := conn.QueryRowContext(ctx, `
		SELECT RTRIM(o.type), ISNULL(CAST(ep.value AS nvarchar(max)), N'')
		FROM sys.objects o
		JOIN sys.schemas s ON s.schema_id = o.schema_id
		LEFT JOIN sys.extended_properties ep
		       ON ep.class = 1 AND ep.major_id = o.object_id AND ep.minor_id = 0 AND ep.name = 'MS_Description'
		WHERE s.name = @p1 AND o.name = @p2 AND o.type IN ('U', 'V')`, schema, name).Scan(&typ, &t.Comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if typ == "V" {
		t.Kind = "view"
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT c.name, t.name, c.max_length, c.precision, c.scale, c.is_nullable,
		       COALESCE(dc.definition, N'AS ' + cc.definition,
		                CASE WHEN c.is_identity = 1 THEN N'IDENTITY' END, N''),
		       CAST(CASE WHEN EXISTS (
		           SELECT 1 FROM sys.index_columns ic
		           JOIN sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id
		           WHERE i.is_primary_key = 1
		             AND ic.object_id = c.object_id AND ic.column_id = c.column_id
		       ) THEN 1 ELSE 0 END AS bit),
		       ISNULL(CAST(ep.value AS nvarchar(max)), N'')
		FROM sys.columns c
		JOIN sys.types t ON t.user_type_id = c.user_type_id
		LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
		LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		LEFT JOIN sys.extended_properties ep
		       ON ep.class = 1 AND ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.name = 'MS_Description'
		WHERE c.object_id = `+mssqlObject+`
		ORDER BY c.column_id`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c TableColumn
		var typ string
		var length, precision, scale int
		if err := rows.Scan(&c.Name, &typ, &length, &precision, &scale, &c.Nullable, &c.Default, &c.PrimaryKey, &c.Comment); err != nil {
			return nil, err
		}
		c.Type = mssqlType(typ, length, precision, scale)
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	idx, err := conn.QueryContext(ctx, `
		SELECT i.name, i.is_unique, i.is_primary_key, c.name
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.is_included_column = 0
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = `+mssqlObject+` AND i.name IS NOT NULL
		ORDER BY i.name, ic.key_ordinal`, schema, name)
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

	if t.ForeignKeys, err = ms.foreignKeys(ctx, conn, schema, name); err != nil {
		return nil, err
	}
	t.ReferencedBy, err = queryKeys(ctx, conn, mssqlKeys+`fk.referenced_object_id = `+mssqlObject+`
		ORDER BY ss.name, st.name, fk.name, fkc.constraint_column_id`, schema, name)
	return t, err
}

func (sqlserver) search(ctx context.Context, conn *sql.DB, pattern string, limit int) ([]Match, error) {
	return queryMatches(ctx, conn, `
		SELECT TOP (@p2) kind, sch, name, col, typ FROM (
		    SELECT CASE RTRIM(o.type) WHEN 'U' THEN 'table' WHEN 'V' THEN 'view'
		                WHEN 'P' THEN 'procedure' WHEN 'PC' THEN 'procedure' ELSE 'function' END AS kind,
		           s.name AS sch, o.name AS name, N'' AS col, N'' AS typ
		    FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id
		    WHERE o.is_ms_shipped = 0 AND o.type IN ('U', 'V', 'P', 'PC', 'FN', 'IF', 'TF', 'FS', 'FT')
		      AND LOWER(o.name) LIKE @p1 ESCAPE '\'
		    UNION ALL
		    SELECT 'column', s.name, o.name, c.name, t.name
		    FROM sys.columns c
		    JOIN sys.objects o ON o.object_id = c.object_id
		    JOIN sys.schemas s ON s.schema_id = o.schema_id
		    JOIN sys.types t ON t.user_type_id = c.user_type_id
		    WHERE o.is_ms_shipped = 0 AND o.type IN ('U', 'V') AND LOWER(c.name) LIKE @p1 ESCAPE '\'
		) m
		ORDER BY CASE WHEN kind = 'column' THEN 1 ELSE 0 END, sch, name, col`, pattern, limit)
}

func (sqlserver) version(ctx context.Context, conn *sql.DB) (string, error) {
	var v, edition string
	err := conn.QueryRowContext(ctx, `
		SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)),
		       CAST(SERVERPROPERTY('Edition') AS nvarchar(128))`).Scan(&v, &edition)
	return "SQL Server " + v + " (" + edition + ")", err
}
