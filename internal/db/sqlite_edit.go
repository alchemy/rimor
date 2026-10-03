package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	msqlite "modernc.org/sqlite"
)

// describeSQLite maps a result to its table with sqlite3_column_table_name
// and sqlite3_column_origin_name, which the driver reads from the prepared
// statement without running it.
func describeSQLite(ctx context.Context, conn *sql.Conn, query string, columns int) (*Origin, error) {
	var info []msqlite.ColumnInfo
	err := conn.Raw(func(dc any) error {
		ci, ok := dc.(interface {
			ColumnInfo(query string) ([]msqlite.ColumnInfo, error)
		})
		if !ok {
			return fmt.Errorf("unexpected driver connection %T", dc)
		}
		var err error
		info, err = ci.ColumnInfo(query)
		return err
	})
	if err != nil {
		return nil, notEditable("rimor cannot tell which table this result comes from: %v", firstLine(err.Error()))
	}
	if len(info) != columns {
		return nil, notEditable("rimor cannot tell which table this result comes from.")
	}

	var schema, table string
	for _, c := range info {
		switch {
		case c.TableName == "":
		case table == "":
			schema, table = c.DatabaseName, c.TableName
		case c.DatabaseName != schema || c.TableName != table:
			return nil, notEditable("The result joins several tables; editing needs a result from one table.")
		}
	}
	if table == "" {
		return nil, notEditable("No column of the result comes straight from a table.")
	}

	o := newOrigin(SQLite, columns)
	o.table = sqliteQuote(schema) + "." + sqliteQuote(table)
	o.Table = table
	if schema != "main" {
		o.Table = schema + "." + table
	}
	if err := singleUse(query, table); err != nil {
		return nil, err
	}

	// The table's columns: declared type, key position, generated or not.
	type attr struct {
		name, typ string
		pk        int
		generated bool
		notNull   bool
	}
	attrs := map[string]attr{}   // by lower-case name: SQLite ignores case
	notNull := map[string]bool{} // likewise, for unique keys
	rows, err := conn.QueryContext(ctx,
		`SELECT name, type, pk, hidden, "notnull" FROM pragma_table_xinfo(?1, ?2)`, table, schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a attr
		var hidden int
		if err := rows.Scan(&a.name, &a.typ, &a.pk, &hidden, &a.notNull); err != nil {
			rows.Close()
			return nil, err
		}
		a.generated = hidden == 2 || hidden == 3 // virtual or stored generated column
		attrs[strings.ToLower(a.name)] = a
		notNull[strings.ToLower(a.name)] = a.notNull || a.pk > 0
	}
	rows.Close()

	jsonChecks, err := sqliteJSONChecks(ctx, conn, schema, table)
	if err != nil {
		return nil, err
	}

	at := map[string]int{} // first result column of each table column
	rowid := -1            // a result column holding the rowid
	for i, c := range info {
		if c.TableName != table || c.OriginName == "" {
			continue
		}
		a, ok := attrs[strings.ToLower(c.OriginName)]
		if !ok {
			// Not a declared column: the rowid, under one of its names.
			switch strings.ToLower(c.OriginName) {
			case "rowid", "oid", "_rowid_":
				if rowid < 0 {
					rowid = i
					o.columns[i], o.names[i], o.fixed[i] = "rowid", "rowid", true
				}
			}
			continue
		}
		o.columns[i], o.types[i], o.names[i], o.fixed[i] = sqliteQuote(a.name), a.typ, a.name, a.generated
		switch strings.ToUpper(a.typ) {
		case "JSONB":
			o.jsonb[i] = true
		case "JSON":
			o.json[i] = true
		}
		if kind, ok := jsonChecks[strings.ToLower(a.name)]; ok && !o.json[i] && !o.jsonb[i] {
			o.json[i], o.jsonb[i] = kind == "json", kind == "jsonb"
		}
		if _, seen := at[strings.ToLower(a.name)]; !seen {
			at[strings.ToLower(a.name)] = i
		}
	}

	// The primary key.
	var pk []attr
	for _, a := range attrs {
		if a.pk > 0 {
			pk = append(pk, a)
		}
	}
	if len(pk) > 0 {
		key := make([]int, len(pk))
		complete := true
		for _, a := range pk {
			i, ok := at[strings.ToLower(a.name)]
			if !ok {
				complete = false
				break
			}
			key[a.pk-1] = i
		}
		if complete {
			o.key = key
			return o, nil
		}
	}

	// Else a unique index on non-null columns.
	if key, err := sqliteUniqueKey(ctx, conn, schema, table, notNull, at); err != nil {
		return nil, err
	} else if key != nil {
		o.key = key
		return o, nil
	}

	// Else the rowid, when the result includes it.
	if rowid >= 0 {
		o.key = []int{rowid}
		return o, nil
	}
	if len(pk) > 0 {
		return nil, notEditable("Add the key of %s to the SELECT to edit it.", o.Table)
	}
	return nil, notEditable("%s has no primary key; add rowid to the SELECT to edit it.", o.Table)
}

// sqliteUniqueKey finds a unique index on non-null plain columns, all in
// the result; nil when there is none.
func sqliteUniqueKey(ctx context.Context, conn *sql.Conn, schema, table string, notNull map[string]bool, at map[string]int) ([]int, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM pragma_index_list(?1, ?2) WHERE "unique" AND NOT partial ORDER BY origin = 'pk' DESC, name`, table, schema)
	if err != nil {
		return nil, err
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		indexes = append(indexes, name)
	}
	rows.Close()

	for _, index := range indexes {
		cols, err := conn.QueryContext(ctx, `SELECT cid, name FROM pragma_index_info(?1, ?2) ORDER BY seqno`, index, schema)
		if err != nil {
			return nil, err
		}
		var key []int
		ok := true
		for cols.Next() {
			var cid int
			var name sql.NullString
			if err := cols.Scan(&cid, &name); err != nil {
				cols.Close()
				return nil, err
			}
			i, inResult := at[strings.ToLower(name.String)]
			if cid < 0 || !name.Valid || !notNull[strings.ToLower(name.String)] || !inResult {
				ok = false // an expression, a nullable column, or not selected
			}
			key = append(key, i)
		}
		cols.Close()
		if ok && len(key) > 0 {
			return key, nil
		}
	}
	return nil, nil
}

var jsonValidCall = regexp.MustCompile(`(?i)json_valid\s*\(\s*["` + "`" + `\[]?([A-Za-z_][A-Za-z0-9_$]*)["` + "`" + `\]]?\s*(?:,\s*(\d+)\s*)?\)`)

// sqliteJSONChecks reads the table's CREATE statement for json_valid
// checks: json_valid(col) keeps col to JSON text; with flags 4 or 8, to
// binary JSONB.
func sqliteJSONChecks(ctx context.Context, conn *sql.Conn, schema, table string) (map[string]string, error) {
	var create sql.NullString
	err := conn.QueryRowContext(ctx,
		`SELECT sql FROM `+sqliteQuote(schema)+`.sqlite_master WHERE type = 'table' AND name = ?1`, table).Scan(&create)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range jsonValidCall.FindAllStringSubmatch(create.String, -1) {
		kind := "json"
		if m[2] != "" {
			if flags, _ := strconv.Atoi(m[2]); flags&(4|8) != 0 && flags&(1|2) == 0 {
				kind = "jsonb"
			}
		}
		out[strings.ToLower(m[1])] = kind
	}
	return out, nil
}

func sqliteQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
