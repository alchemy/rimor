package db

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// MySQL tests run against each server named in the environment, e.g.
//
//	RIMOR_TEST_MYSQL='root:pass@tcp(127.0.0.1:3306)/rimor_test'
//	RIMOR_TEST_MARIADB='root:pass@tcp(127.0.0.1:3307)/rimor_test'
//
// They create and drop databases named rimor_*.
func eachMySQL(t *testing.T, test func(t *testing.T, dsn string)) {
	t.Helper()
	ran := false
	for _, env := range []string{"RIMOR_TEST_MYSQL", "RIMOR_TEST_MARIADB"} {
		dsn := os.Getenv(env)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(strings.ToLower(strings.TrimPrefix(env, "RIMOR_TEST_")), func(t *testing.T) { test(t, dsn) })
	}
	if !ran {
		t.Skip("RIMOR_TEST_MYSQL and RIMOR_TEST_MARIADB not set")
	}
}

// mysqlShop creates the database rimor_shop: a small ERP with the cases the
// catalog has to read.
func mysqlShop(t *testing.T, dsn string) *Pool {
	t.Helper()
	admin, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS rimor_shop"); admin.Close() })
	for _, s := range []string{
		"DROP DATABASE IF EXISTS rimor_shop",
		"CREATE DATABASE rimor_shop",
		`CREATE TABLE rimor_shop.customers (
			id INT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(80) NOT NULL,
			region VARCHAR(20) DEFAULT 'north' COMMENT 'sales region',
			created TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		) COMMENT 'who buys'`,
		`CREATE TABLE rimor_shop.orders (
			id INT AUTO_INCREMENT PRIMARY KEY,
			customer_id INT NOT NULL,
			delivery_date DATE,
			total DECIMAL(10,2),
			doubled DECIMAL(12,2) AS (total * 2) STORED,
			` + "`order`" + ` VARCHAR(10),
			FOREIGN KEY (customer_id) REFERENCES rimor_shop.customers(id)
		)`,
		"CREATE INDEX orders_due ON rimor_shop.orders (delivery_date, `order`)",
		`CREATE TABLE rimor_shop.order_lines (
			order_id INT NOT NULL,
			line_no INT NOT NULL,
			qty INT UNSIGNED,
			PRIMARY KEY (order_id, line_no),
			FOREIGN KEY (order_id) REFERENCES rimor_shop.orders(id)
		)`,
		"CREATE VIEW rimor_shop.late_orders AS SELECT id, delivery_date FROM rimor_shop.orders",
		"CREATE FUNCTION rimor_shop.add_one(n INT) RETURNS INT DETERMINISTIC RETURN n + 1",
		"CREATE PROCEDURE rimor_shop.settle(IN who VARCHAR(20), OUT done INT) SET done = 1",
		"CREATE TRIGGER rimor_shop.touch BEFORE INSERT ON rimor_shop.customers FOR EACH ROW SET NEW.region = COALESCE(NEW.region, 'north')",
		"INSERT INTO rimor_shop.customers (name) VALUES ('Ada'), ('Grace')",
	} {
		if _, err := admin.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	p := NewPool(Config{Driver: MySQL, DSN: dsn})
	t.Cleanup(p.Close)
	return p
}

// displayWidth is MariaDB's int(11), which MySQL 8 writes int.
var displayWidth = regexp.MustCompile(`int\(\d+\)`)

// idType is how the server writes an INT column.
func idType(dsn string) string {
	if strings.Contains(dsn, os.Getenv("RIMOR_TEST_MARIADB")) && os.Getenv("RIMOR_TEST_MARIADB") != "" {
		return "int(11)"
	}
	return "int"
}

func TestMySQLCatalog(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		p := mysqlShop(t, dsn)
		got := walk(t, p.Config(), false)
		requireLines(t, got,
			"rimor_test default",
			"rimor_shop",
			"  Tables",
			"    customers",
			"        id "+idType(dsn)+" *",
			"        name varchar(80)",
			"        PRIMARY primary *",
			"    orders",
			"        orders_due",
			"        customer_id → customers(id)",
			"  Views",
			"    late_orders",
			"  Functions",
			"    add_one",
			"  Procedures",
			"    settle",
			"  Triggers",
			"    touch customers",
		)
		for _, l := range got {
			if strings.Contains(l, "information_schema") || strings.Contains(l, "performance_schema") {
				t.Errorf("a system database is listed: %q", l)
			}
		}
	})
}

func TestMySQLDescribeAndSearch(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		p := mysqlShop(t, dsn)
		ctx := context.Background()

		tb, err := p.Describe(ctx, "rimor_shop", "", "orders")
		if err != nil {
			t.Fatal(err)
		}
		got := render(tb)
		t.Log("\n" + got)
		for _, w := range []string{
			"table rimor_shop.orders",
			" not null pk default AUTO_INCREMENT\n",
			"  customer_id int",
			"  doubled decimal(12,2) default AS (",
			"  order varchar(10)",
			"  index (delivery_date, order)\n",
			"  index (id) unique primary\n",
			"  fk customer_id → customers(id)\n",
			"  from rimor_shop.order_lines(order_id)\n",
		} {
			if !strings.Contains(got, w) {
				t.Errorf("orders lacks %q", w)
			}
		}
		auto := map[string]bool{}
		for _, c := range tb.Columns {
			auto[c.Name] = c.Auto
		}
		if !auto["id"] || !auto["doubled"] || auto["total"] {
			t.Errorf("auto columns: %v", auto)
		}

		tb, err = p.Describe(ctx, "rimor_shop", "", "customers")
		if err != nil {
			t.Fatal(err)
		}
		got = render(tb)
		for _, w := range []string{"region varchar(20) default 'north' -- sales region", "created timestamp default current_timestamp"} {
			if !strings.Contains(strings.ToLower(got), strings.ToLower(w)) {
				t.Errorf("customers lacks %q:\n%s", w, got)
			}
		}
		if tb.Comment != "who buys" {
			t.Errorf("comment %q", tb.Comment)
		}
		if tb, err := p.Describe(ctx, "rimor_shop", "", "late_orders"); err != nil || tb.Kind != "view" || tb.Comment != "" {
			t.Errorf("view: %+v %v", tb, err)
		}
		if _, err := p.Describe(ctx, "rimor_shop", "", "nope"); err != ErrNotFound {
			t.Errorf("missing: %v", err)
		}

		ms, err := p.Search(ctx, "rimor_shop", "ORDER", 50)
		if err != nil {
			t.Fatal(err)
		}
		names := matchNames(ms)
		for _, w := range []string{"view late_orders", "table order_lines", "table orders", "column orders.order", "column order_lines.order_id"} {
			if !slices.Contains(names, w) {
				t.Errorf("search lacks %q: %q", w, names)
			}
		}
		if ms, _ := p.Search(ctx, "rimor_shop", "%", 50); len(ms) != 0 {
			t.Errorf("%% matched %q", matchNames(ms))
		}
		if ms, _ := p.Search(ctx, "rimor_shop", "settle", 50); !slices.Equal(matchNames(ms), []string{"procedure settle"}) {
			t.Errorf("routine search: %q", matchNames(ms))
		}

		v, err := p.ServerVersion(ctx)
		if err != nil || !(strings.HasPrefix(v, "MySQL 8.") || strings.HasPrefix(v, "MariaDB 1")) || strings.Contains(v, "-MariaDB") {
			t.Errorf("version %q %v", v, err)
		}
	})
}

func TestMySQLScripts(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		p := mysqlShop(t, dsn)
		ctx := context.Background()
		conn, err := p.DB(ctx, "rimor_shop")
		if err != nil {
			t.Fatal(err)
		}
		orders := scriptsFor(t, p, Object{Kind: KindTable, Schema: "rimor_shop", Name: "orders", Database: "rimor_shop"})
		prepare(t, conn, orders)
		if got := orders["insert"].SQL; !strings.HasPrefix(got, "INSERT INTO rimor_shop.orders (customer_id, delivery_date, total,") ||
			!strings.Contains(got, "`order`)") || strings.Contains(got, "doubled") || strings.Contains(got, " id") {
			t.Errorf("insert leaves out the auto columns:\n%s", got)
		}
		if got := orders["select"].SQL; !strings.HasSuffix(got, "LIMIT 50;\n") {
			t.Errorf("select:\n%s", got)
		}
		prepare(t, conn, scriptsFor(t, p, Object{Kind: KindView, Schema: "rimor_shop", Name: "late_orders", Database: "rimor_shop"}))
		prepare(t, conn, scriptsFor(t, p, Object{Kind: KindColumn, Schema: "rimor_shop", Table: "orders", Name: "order", Database: "rimor_shop"}))
		prepare(t, conn, scriptsFor(t, p, Object{Kind: KindForeignKey, Schema: "rimor_shop", Table: "orders", Name: "customer_id → customers(id)", Database: "rimor_shop"}))
		idx := scriptsFor(t, p, Object{Kind: KindIndex, Schema: "rimor_shop", Table: "orders", Name: "orders_due", Database: "rimor_shop"})
		if got := idx["drop"].SQL; !strings.Contains(got, "-- DROP INDEX orders_due ON rimor_shop.orders;") {
			t.Errorf("drop index:\n%s", got)
		}

		fn := scriptsFor(t, p, Object{Kind: KindFunction, Schema: "rimor_shop", Name: "add_one", Database: "rimor_shop"})["call"]
		if displayWidth.ReplaceAllString(fn.SQL, "int") != "SELECT rimor_shop.add_one(\n    NULL  -- n int\n);\n" || fn.Writes {
			t.Errorf("function call:\n%s", fn.SQL)
		}
		proc := scriptsFor(t, p, Object{Kind: KindProcedure, Schema: "rimor_shop", Name: "settle", Database: "rimor_shop"})["call"]
		want := "CALL rimor_shop.settle(\n    NULL,  -- who varchar(20)\n    @done  -- done int, output\n);\nSELECT @done;\n"
		if displayWidth.ReplaceAllString(proc.SQL, "int") != want {
			t.Errorf("procedure call:\n%s\nwant:\n%s", proc.SQL, want)
		}
		// The calls run, as they are, and the output comes back.
		s, err := conn.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.ExecContext(ctx, fn.SQL); err != nil {
			t.Errorf("function call: %v", err)
		}
		res, err := Run(ctx, s, proc.SQL, RunOptions{Driver: MySQL, Cancel: func() {}})
		if err != nil || res.Rows == nil {
			t.Fatalf("procedure call: %v %+v", err, res)
		}
		wait(t, res.Rows)
		if v := res.Rows.Cell(0, 0); v.Text != "1" {
			t.Errorf("@done = %q", v.Text)
		}
	})
}

// runMy runs a batch as the query pane does and waits for its rows.
func runMy(t *testing.T, s *sql.Conn, batch string) (*Result, error) {
	t.Helper()
	res, err := Run(context.Background(), s, batch, RunOptions{Driver: MySQL, Cancel: func() {}})
	if err == nil && res.Rows != nil {
		wait(t, res.Rows)
	}
	return res, err
}

func TestMySQLRun(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		p := mysqlShop(t, dsn)
		ctx := context.Background()
		s, err := p.Session(ctx, "rimor_shop")
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release(s)

		// Rows after a SET: no first-word guessing.
		res, err := runMy(t, s, "SET @n := 2;\nSELECT @n AS n, name FROM customers ORDER BY id")
		if err != nil || res.Rows == nil || res.Rows.Len() != 2 || res.Rows.Cell(1, 1).Text != "Grace" {
			t.Fatalf("SET then SELECT: %v %+v", err, res)
		}
		if !res.Rows.Numeric(0) {
			t.Error("a BIGINT column is not numeric")
		}

		// A session value compared with a column: the connection takes the
		// server's collation, so MySQL 8 (utf8mb4_0900_ai_ci) does not
		// refuse with "Illegal mix of collations".
		res, err = runMy(t, s, "SET @r := 'north';\nSELECT name FROM customers WHERE region = @r ORDER BY id")
		if err != nil || res.Rows == nil || res.Rows.Len() != 2 {
			t.Errorf("comparing with a session value: %v %+v", err, res)
		}

		// Writes: the rows affected.
		res, err = runMy(t, s, "UPDATE customers SET region = 'south'")
		if err != nil || res.Rows != nil || !res.HasAffected || res.RowsAffected != 2 {
			t.Errorf("update: %v %+v", err, res)
		}
		res, err = runMy(t, s, "CREATE TEMPORARY TABLE tmp (a int)")
		if err != nil || res.Rows != nil || res.RowsAffected != 0 {
			t.Errorf("DDL: %v %+v", err, res)
		}

		// Unsigned, BIT and binary values.
		res, err = runMy(t, s, "CREATE TEMPORARY TABLE bits (b BIT(3)); INSERT INTO bits VALUES (b'101');\n"+
			"SELECT CAST(7 AS UNSIGNED) AS u, b, b'101' AS raw FROM bits")
		if err != nil || res.Rows == nil {
			t.Fatalf("values: %v", err)
		}
		if b, raw := res.Rows.Cell(0, 1).Text, res.Rows.Cell(0, 2).Text; b != "5" || raw != "0x05" {
			t.Errorf("BIT shows %q, a binary value %q", b, raw)
		}
		if !res.Rows.Numeric(0) || !res.Rows.Numeric(1) {
			t.Error("UNSIGNED or BIT columns are not numeric")
		}

		// Errors: number, SQLSTATE and line.
		_, err = runMy(t, s, "SELECT *\nFORM customers")
		if e := Describe(err); err == nil || e.Code != "1064" || e.Line != 2 || !strings.HasPrefix(e.Detail, "SQLSTATE 42000") {
			t.Errorf("syntax error: %v %+v", err, e)
		}
		// An error after the rows is reported too, with the rows kept.
		res, err = runMy(t, s, "SELECT 1 AS fine; SELECT * FROM nope")
		if err != nil || res.Rows == nil || res.Rows.Len() != 1 {
			t.Fatalf("error after the rows: %v %+v", err, res)
		}
		if p := res.Rows.Progress(); p.Err == nil || Describe(p.Err).Code != "1146" {
			t.Errorf("the error after the rows was lost: %v", p.Err)
		}
		_, err = runMy(t, s, "SELECT * FROM nope")
		if e := Describe(err); e.Code != "1146" {
			t.Errorf("missing table: %+v", e)
		}
		// The session still answers.
		if res, err := runMy(t, s, "SELECT 42"); err != nil || res.Rows.Cell(0, 0).Text != "42" {
			t.Errorf("after errors: %v", err)
		}
	})
}

func TestMySQLReadOnly(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		mysqlShop(t, dsn)
		ctx := context.Background()
		p := NewPool(Config{Driver: MySQL, DSN: dsn, ReadOnly: true})
		defer p.Close()
		s, err := p.Session(ctx, "rimor_shop")
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release(s)
		if res, err := runMy(t, s, "SELECT count(*) FROM customers"); err != nil || res.Rows.Cell(0, 0).Text != "2" {
			t.Errorf("reading: %v", err)
		}
		for _, stmt := range []string{"INSERT INTO customers (name) VALUES ('Eve')", "CREATE TABLE more (a int)", "DROP TABLE order_lines"} {
			if _, err := runMy(t, s, stmt); Describe(err).Code != "1792" {
				t.Errorf("%s on a read-only connection: %v", stmt, err)
			}
		}
	})
}

func TestTransactionsMySQL(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		ctx := context.Background()
		p := NewPool(Config{Driver: MySQL, DSN: dsn})
		defer p.Close()
		s, err := p.Session(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release(s)
		mustExec(t, s, "CREATE TEMPORARY TABLE t (a int)", "INSERT INTO t VALUES (1)")

		txIs(t, s, MySQL, "fresh session", none)
		mustExec(t, s, "BEGIN", "UPDATE t SET a = 2")
		txIs(t, s, MySQL, "BEGIN and an UPDATE", open)
		if err := EndTransaction(ctx, s, MySQL, false); err != nil {
			t.Fatal(err)
		}
		txIs(t, s, MySQL, "rolled back", none)
		if v := value(t, s, "t"); v != 1 {
			t.Errorf("after rollback a = %d", v)
		}

		// autocommit off: the first write opens a transaction, unasked.
		mustExec(t, s, "SET autocommit = 0", "UPDATE t SET a = 3")
		txIs(t, s, MySQL, "autocommit off", open)
		if err := EndTransaction(ctx, s, MySQL, true); err != nil {
			t.Fatal(err)
		}
		mustExec(t, s, "SET autocommit = 1")
		txIs(t, s, MySQL, "committed", none)
		if v := value(t, s, "t"); v != 3 {
			t.Errorf("after commit a = %d", v)
		}

		// The flag is read from the driver; this fails if an update of
		// go-sql-driver/mysql renames it.
		found := false
		s.Raw(func(dc any) error {
			v := reflect.ValueOf(dc)
			if v.Kind() == reflect.Pointer {
				v = v.Elem()
			}
			f := v.FieldByName("status")
			found = f.IsValid() && f.CanUint()
			return nil
		})
		if !found {
			t.Error("go-sql-driver/mysql no longer has the status field rimor reads: see mysqlTransaction")
		}
	})
}

func TestMySQLPasswords(t *testing.T) {
	for _, c := range []struct{ dsn, clean, pwd string }{
		{"app:s3cr3t@tcp(db.local:3306)/shop?parseTime=true", "app@tcp(db.local:3306)/shop?parseTime=true", "s3cr3t"},
		{"app:p@ss:wd@tcp(db.local)/shop", "app@tcp(db.local)/shop", "p@ss:wd"},
		{"mysql://app:s3cr%40t@db.local:3306/shop", "mysql://app@db.local:3306/shop", "s3cr@t"},
	} {
		clean, pwd, ok := splitPassword(MySQL, c.dsn)
		if !ok || clean != c.clean || pwd != c.pwd {
			t.Errorf("split %q: %q %q %v", c.dsn, clean, pwd, ok)
			continue
		}
		joined := joinPassword(MySQL, clean, pwd)
		cfg, err := mysqlConfig(joined, "")
		if err != nil || cfg.Passwd != c.pwd {
			t.Errorf("join %q: the driver reads %q (%v)", joined, cfg.Passwd, err)
		}
		if cfg, _ := mysqlConfig(clean, ""); cfg.Passwd != "" {
			t.Errorf("%q still has a password", clean)
		}
	}
	for _, dsn := range []string{"app@tcp(db.local)/shop", "/shop", "mysql://app@db.local/shop"} {
		if _, _, ok := splitPassword(MySQL, dsn); ok {
			t.Errorf("found a password in %q", dsn)
		}
	}
	// The driver form, as the driver reads it.
	cfg, err := mysqlConfig("app:x@tcp(db.local:3306)/shop?parseTime=true", "other")
	if err != nil || cfg.DBName != "other" || !cfg.MultiStatements || !cfg.ParseTime || cfg.Addr != "db.local:3306" {
		t.Errorf("config: %+v %v", cfg, err)
	}
	cfg, err = mysqlConfig("mysql://app:x@db.local/shop?parseTime=true", "")
	if err != nil || cfg.Addr != "db.local:3306" || cfg.DBName != "shop" || cfg.User != "app" || !cfg.ParseTime {
		t.Errorf("URL config: %+v %v", cfg, err)
	}
}
