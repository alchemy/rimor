package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// mysql hierarchy (a MySQL database is what other engines call a schema,
// so there is no schema level):
//
//	database → Tables | Views | Functions | Procedures | Triggers
//	table    → Columns | Indexes | Foreign Keys
//	view     → Columns
//
// Objects carry their database in Schema, so names qualify as `db`.`name`.
type mysqlCatalog struct{}

// mysqlSystem are the databases the server keeps for itself.
const mysqlSystem = `('information_schema', 'performance_schema', 'mysql', 'sys')`

func (mysqlCatalog) Children(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	if p == nil {
		return queryObjects(ctx, conn, func(name, detail string) Object {
			return Object{Kind: KindDatabase, Name: name, Detail: detail, Database: name, Schema: name}
		}, `
			SELECT SCHEMA_NAME, CASE WHEN SCHEMA_NAME = DATABASE() THEN 'default' ELSE '' END
			FROM information_schema.SCHEMATA
			WHERE SCHEMA_NAME NOT IN `+mysqlSystem+`
			ORDER BY SCHEMA_NAME`)
	}

	switch p.Kind {
	case KindDatabase:
		return []Object{
			folder("Tables", KindTable, p.Name, ""),
			folder("Views", KindView, p.Name, ""),
			folder("Functions", KindFunction, p.Name, ""),
			folder("Procedures", KindProcedure, p.Name, ""),
			folder("Triggers", KindTrigger, p.Name, ""),
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
		return mysqlFolder(ctx, conn, p)
	}
	return nil, nil
}

func mysqlFolder(ctx context.Context, conn *sql.DB, p *Object) ([]Object, error) {
	inSchema := func(kind Kind) func(name, detail string) Object {
		return func(name, detail string) Object {
			return Object{Kind: kind, Name: name, Detail: detail, Schema: p.Schema}
		}
	}
	tables := func(kind Kind, types string) ([]Object, error) {
		return queryObjects(ctx, conn, inSchema(kind), `
			SELECT TABLE_NAME, '' FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = ? AND TABLE_TYPE IN (`+types+`)
			ORDER BY TABLE_NAME`, p.Schema)
	}
	routines := func(kind Kind, typ string) ([]Object, error) {
		return queryObjects(ctx, conn, inSchema(kind), `
			SELECT ROUTINE_NAME, '' FROM information_schema.ROUTINES
			WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = ?
			ORDER BY ROUTINE_NAME`, p.Schema, typ)
	}

	switch p.Contains {
	case KindTable:
		return tables(KindTable, "'BASE TABLE', 'SYSTEM VERSIONED'") // the latter: MariaDB
	case KindView:
		return tables(KindView, "'VIEW'")
	case KindFunction:
		return routines(KindFunction, "FUNCTION")
	case KindProcedure:
		return routines(KindProcedure, "PROCEDURE")
	case KindTrigger:
		// Triggers show the table they belong to.
		return queryObjects(ctx, conn, inSchema(KindTrigger), `
			SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE FROM information_schema.TRIGGERS
			WHERE TRIGGER_SCHEMA = ?
			ORDER BY TRIGGER_NAME`, p.Schema)
	case KindColumn:
		return queryColumns(ctx, conn, `
			SELECT COLUMN_NAME, COLUMN_TYPE, COLUMN_KEY = 'PRI'
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
			ORDER BY ORDINAL_POSITION`, p.Schema, p.Table)
	case KindIndex:
		return queryIndexes(ctx, conn, `
			SELECT INDEX_NAME, MIN(NON_UNIQUE) = 0, INDEX_NAME = 'PRIMARY'
			FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
			GROUP BY INDEX_NAME
			ORDER BY INDEX_NAME`, p.Schema, p.Table)
	case KindForeignKey:
		return foreignKeyObjects(mysqlCatalog{}.foreignKeys(ctx, conn, p.Schema, p.Table))
	}
	return nil, fmt.Errorf("mysql: unexpected folder %q", p.Name)
}

// mysqlKeys lists foreign keys as (name, schema, table, ref schema, ref
// table, column, ref column) rows, one per column pair in key order.
// where filters information_schema.KEY_COLUMN_USAGE.
const mysqlKeys = `
	SELECT CONSTRAINT_NAME, TABLE_SCHEMA, TABLE_NAME, REFERENCED_TABLE_SCHEMA, REFERENCED_TABLE_NAME,
	       COLUMN_NAME, REFERENCED_COLUMN_NAME
	FROM information_schema.KEY_COLUMN_USAGE
	WHERE REFERENCED_TABLE_NAME IS NOT NULL AND `

func (mysqlCatalog) foreignKeys(ctx context.Context, conn *sql.DB, schema, table string) ([]ForeignKey, error) {
	return queryKeys(ctx, conn, mysqlKeys+`TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND TABLE_NAME = ?
		ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`, schema, table)
}

// mariaDB reports whether the server is MariaDB, whose catalog differs in
// places: COLUMN_DEFAULT holds SQL ('north', NULL) rather than the value.
func mariaDB(ctx context.Context, conn *sql.DB) (bool, error) {
	var v string
	err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v)
	return strings.Contains(v, "MariaDB"), err
}

func (my mysqlCatalog) describe(ctx context.Context, conn *sql.DB, schema, name string) (*Table, error) {
	if schema == "" {
		var db sql.NullString
		if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&db); err != nil {
			return nil, err
		}
		schema = db.String
	}
	maria, err := mariaDB(ctx, conn)
	if err != nil {
		return nil, err
	}
	t := &Table{Schema: schema, Name: name, Kind: "table"}
	var typ string
	err = conn.QueryRowContext(ctx, `
		SELECT TABLE_TYPE, COALESCE(TABLE_COMMENT, '') FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, schema, name).Scan(&typ, &t.Comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if typ == "VIEW" {
		t.Kind, t.Comment = "view", "" // a view's comment reads "VIEW"
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE = 'YES', COLUMN_DEFAULT, COLUMN_KEY = 'PRI',
		       COLUMN_COMMENT, EXTRA, COALESCE(GENERATION_EXPRESSION, ''), DATA_TYPE
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c TableColumn
		var def sql.NullString
		var extra, generated, dataType string
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &def, &c.PrimaryKey, &c.Comment, &extra, &generated, &dataType); err != nil {
			return nil, err
		}
		extra = strings.ToLower(extra)
		switch {
		case generated != "":
			c.Default, c.Auto = "AS ("+generated+")", true
		case strings.Contains(extra, "auto_increment"):
			c.Default, c.Auto = "AUTO_INCREMENT", true
		case def.Valid:
			c.Default = mysqlDefault(def.String, extra, dataType, maria)
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	idx, err := conn.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE = 0, INDEX_NAME = 'PRIMARY', COALESCE(COLUMN_NAME, '(expression)')
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, schema, name)
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

	if t.ForeignKeys, err = my.foreignKeys(ctx, conn, schema, name); err != nil {
		return nil, err
	}
	t.ReferencedBy, err = queryKeys(ctx, conn, mysqlKeys+`REFERENCED_TABLE_SCHEMA = ? AND REFERENCED_TABLE_NAME = ?
		ORDER BY TABLE_SCHEMA, TABLE_NAME, CONSTRAINT_NAME, ORDINAL_POSITION`, schema, name)
	return t, err
}

// mysqlDefault writes a column default as SQL. MariaDB stores it so
// already ('north', NULL for none); MySQL stores the value, and marks an
// expression (CURRENT_TIMESTAMP) DEFAULT_GENERATED.
func mysqlDefault(def, extra, dataType string, maria bool) string {
	switch {
	case maria && strings.EqualFold(def, "NULL"):
		return ""
	case maria, strings.Contains(extra, "default_generated"):
		return def
	}
	switch strings.ToLower(dataType) {
	case "tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "float", "double", "bit", "year":
		return def
	}
	if strings.EqualFold(def, "CURRENT_TIMESTAMP") || strings.HasPrefix(strings.ToUpper(def), "CURRENT_TIMESTAMP(") {
		return def
	}
	return "'" + strings.ReplaceAll(def, "'", "''") + "'"
}

func (mysqlCatalog) search(ctx context.Context, conn *sql.DB, text string, limit int) ([]Match, error) {
	// ! escapes the pattern: the backslash LIKE uses by default stops
	// being special under sql_mode NO_BACKSLASH_ESCAPES.
	pattern := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(text)) + "%"
	return queryMatches(ctx, conn, `
		SELECT kind, sch, name, col, typ FROM (
		    SELECT CASE TABLE_TYPE WHEN 'VIEW' THEN 'view' ELSE 'table' END AS kind,
		           TABLE_SCHEMA AS sch, TABLE_NAME AS name, '' AS col, '' AS typ
		    FROM information_schema.TABLES
		    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE IN ('BASE TABLE', 'SYSTEM VERSIONED', 'VIEW')
		      AND LOWER(TABLE_NAME) LIKE ? ESCAPE '!'
		    UNION ALL
		    SELECT LOWER(ROUTINE_TYPE), ROUTINE_SCHEMA, ROUTINE_NAME, '', ''
		    FROM information_schema.ROUTINES
		    WHERE ROUTINE_SCHEMA = DATABASE() AND LOWER(ROUTINE_NAME) LIKE ? ESCAPE '!'
		    UNION ALL
		    SELECT 'column', TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE
		    FROM information_schema.COLUMNS
		    WHERE TABLE_SCHEMA = DATABASE() AND LOWER(COLUMN_NAME) LIKE ? ESCAPE '!'
		) m
		ORDER BY kind = 'column', sch, name, col
		LIMIT ?`, pattern, pattern, pattern, limit)
}

func (mysqlCatalog) version(ctx context.Context, conn *sql.DB) (string, error) {
	var v string
	if err := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return "", err
	}
	if before, _, ok := strings.Cut(v, "-MariaDB"); ok {
		return "MariaDB " + before, nil
	}
	return "MySQL " + v, nil
}

func mysqlRoutine(ctx context.Context, conn *sql.DB, o Object) (*Routine, error) {
	var typ string
	err := conn.QueryRowContext(ctx, `
		SELECT ROUTINE_TYPE FROM information_schema.ROUTINES
		WHERE ROUTINE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND ROUTINE_NAME = ?`, o.Schema, o.Name).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s no longer exists", o.Name)
	}
	if err != nil {
		return nil, err
	}
	r := &Routine{Kind: "scalar function"} // MySQL functions return one value
	if typ == "PROCEDURE" {
		r.Kind = "procedure"
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT COALESCE(PARAMETER_NAME, ''), DTD_IDENTIFIER, COALESCE(PARAMETER_MODE, 'IN')
		FROM information_schema.PARAMETERS
		WHERE SPECIFIC_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND SPECIFIC_NAME = ? AND ORDINAL_POSITION > 0
		ORDER BY ORDINAL_POSITION`, o.Schema, o.Name)
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
		prm.Output = mode == "OUT" || mode == "INOUT"
		r.Params = append(r.Params, prm)
	}
	return r, rows.Err()
}
