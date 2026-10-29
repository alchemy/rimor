package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestChangeText(t *testing.T) {
	for in, want := range map[string]string{
		"CREATE PROCEDURE p AS SELECT 1":                      "ALTER PROCEDURE p AS SELECT 1",
		"-- note\n/* a /* nested */ b */\ncreate view v as x": "-- note\n/* a /* nested */ b */\nALTER view v as x",
		"CREATE OR ALTER PROCEDURE p AS SELECT 1":             "CREATE OR ALTER PROCEDURE p AS SELECT 1",
	} {
		if got, err := mssqlAlter(in); err != nil || got != want {
			t.Errorf("mssqlAlter(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := mssqlAlter("SELECT 1"); err == nil {
		t.Error("ALTER made of a SELECT")
	}
	if got := orReplace("CREATE DEFINER=`root`@`%` PROCEDURE p() SELECT 1"); got != "CREATE OR REPLACE DEFINER=`root`@`%` PROCEDURE p() SELECT 1" {
		t.Errorf("orReplace: %q", got)
	}
	if got := orReplace("CREATE OR REPLACE VIEW v AS SELECT 1"); got != "CREATE OR REPLACE VIEW v AS SELECT 1" {
		t.Errorf("orReplace twice: %q", got)
	}

	for stmt, want := range map[string]string{
		"CREATE DEFINER=`root`@`%` PROCEDURE `settle`(IN who int) BEGIN SELECT 1; END":        "`settle`",
		"CREATE PROCEDURE shop.settle() SELECT 'PROCEDURE x'":                                 "shop.settle",
		"CREATE /* PROCEDURE no */ PROCEDURE `my db`.`a``b`() SELECT 1":                       "`my db`.`a``b`",
		"CREATE DEFINER='procedure'@'%' TRIGGER t BEFORE INSERT ON x FOR EACH ROW SET @a = 1": "t",
	} {
		kind := "PROCEDURE"
		if strings.Contains(stmt, "TRIGGER") {
			kind = "TRIGGER"
		}
		s, e, ok := mysqlNameSpan(stmt, kind)
		if !ok || stmt[s:e] != want {
			t.Errorf("name in %q: %q, %v; want %q", stmt, stmt[s:e], ok, want)
		}
	}
	for name, want := range map[string]string{
		"`settle`": "settle_rimor_try", "shop.settle": "shop.settle_rimor_try",
		"`my db`.`a``b`": "`my db`.`a``b_rimor_try`",
	} {
		if got := mysqlTryName(name); got != want {
			t.Errorf("mysqlTryName(%q) = %q, want %q", name, got, want)
		}
	}
}

// changeScriptOf finds the change script of an object.
func changeScriptOf(t *testing.T, p *Pool, o Object) Script {
	t.Helper()
	scripts, err := p.Scripts(context.Background(), o, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		if s.Change != nil {
			return s
		}
	}
	t.Fatalf("no change script for %s among %d", o.Name, len(scripts))
	return Script{}
}

// apply runs a change on a session.
func apply(t *testing.T, s *sql.Conn, c *Change, text string, force bool) (string, error) {
	t.Helper()
	ctx := context.Background()
	res, err := ApplyChange(ctx, s, *c, text, RunOptions{Cancel: func() {}}, force)
	if err != nil {
		return "", err
	}
	if res.Batch != nil {
		for res.Fetching() {
			<-res.Updated()
		}
		if err := res.Batch.Err(); err != nil {
			return "", err
		}
	}
	return Definition(ctx, s, *c)
}

func TestChangeSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "c.db")
	raw, _ := sql.Open("sqlite", path)
	if _, err := raw.Exec(`CREATE TABLE t (a int); INSERT INTO t VALUES (1), (2);
		CREATE VIEW v AS SELECT a FROM t WHERE a > 1;
		CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END;`); err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	p := NewPool(Config{Driver: SQLite, DSN: path})
	defer p.Close()
	s, err := p.Session(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(s)

	sc := changeScriptOf(t, p, Object{Kind: KindView, Name: "v"})
	if !strings.Contains(sc.SQL, "CREATE VIEW v AS SELECT a FROM t WHERE a > 1;") || sc.Title != "Change the view" {
		t.Fatalf("script:\n%s", sc.SQL)
	}
	text := strings.Replace(sc.SQL, "a > 1", "a > 0", 1)
	now, err := apply(t, s, sc.Change, text, false)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if n != 2 || !strings.Contains(now, "a > 0") {
		t.Errorf("view not changed: %d rows, now %q", n, now)
	}

	// An error leaves the view as it was.
	sc.Change.Original = now
	if _, err := apply(t, s, sc.Change, "CREATE VIEW v AS SELEC broken", false); err == nil {
		t.Fatal("a broken view was applied")
	}
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if n != 2 {
		t.Errorf("the view was lost: %d rows", n)
	}

	// Text that makes something else is refused.
	if _, err := apply(t, s, sc.Change, "CREATE VIEW w AS SELECT 1", false); err == nil {
		t.Error("another view replaced v")
	}
	raw.QueryRow("SELECT count(*) FROM v").Scan(&n)
	if n != 2 {
		t.Errorf("v went: %d rows", n)
	}

	// Someone else changed it: refused, unless forced.
	raw.Exec("DROP VIEW v; CREATE VIEW v AS SELECT 7 AS a")
	_, err = apply(t, s, sc.Change, text, false)
	var changed *ChangedError
	if !errors.As(err, &changed) || changed.Gone {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := apply(t, s, sc.Change, text, true); err != nil {
		t.Errorf("forced: %v", err)
	}

	// A trigger.
	tr := changeScriptOf(t, p, Object{Kind: KindTrigger, Name: "tr"})
	if _, err := apply(t, s, tr.Change, strings.Replace(tr.SQL, "SELECT 1", "SELECT 2", 1), false); err != nil {
		t.Errorf("trigger: %v", err)
	}
	var def string
	raw.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'tr'").Scan(&def)
	if !strings.Contains(def, "SELECT 2") {
		t.Errorf("trigger now %q", def)
	}
}

func TestChangePostgres(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_POSTGRES")
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() { admin.Exec("DROP SCHEMA IF EXISTS rimor_change CASCADE") }
	drop()
	t.Cleanup(func() { drop(); admin.Close() })
	for _, q := range []string{
		"CREATE SCHEMA rimor_change",
		"CREATE FUNCTION rimor_change.twice(n int) RETURNS int LANGUAGE sql AS 'SELECT n * 2'",
		"CREATE PROCEDURE rimor_change.note(msg text) LANGUAGE plpgsql AS $$ BEGIN RAISE NOTICE '%', msg; END $$",
		"CREATE VIEW rimor_change.v AS SELECT 1 AS a",
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	p := NewPool(Config{Driver: Postgres, DSN: dsn})
	defer p.Close()
	s, err := p.Session(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(s)

	// Functions come from the explorer, with their overload.
	folder := Object{Kind: KindFolder, Contains: KindFunction, Schema: "rimor_change"}
	routines, err := p.Children(ctx, &folder)
	if err != nil || len(routines) != 2 {
		t.Fatalf("routines %+v %v", routines, err)
	}
	fn := changeScriptOf(t, p, routines[1]) // twice
	if !strings.Contains(fn.SQL, "CREATE OR REPLACE FUNCTION rimor_change.twice(n integer)") {
		t.Fatalf("function script:\n%s", fn.SQL)
	}
	if _, err := apply(t, s, fn.Change, strings.Replace(fn.SQL, "n * 2", "n * 3", 1), false); err != nil {
		t.Fatal(err)
	}
	var v int
	admin.QueryRow("SELECT rimor_change.twice(2)").Scan(&v)
	if v != 6 {
		t.Errorf("function not changed: %d", v)
	}

	proc := changeScriptOf(t, p, routines[0]) // note
	if !strings.Contains(proc.SQL, "CREATE OR REPLACE PROCEDURE rimor_change.note(") {
		t.Errorf("procedure script:\n%s", proc.SQL)
	}

	view := changeScriptOf(t, p, Object{Kind: KindView, Schema: "rimor_change", Name: "v"})
	if _, err := apply(t, s, view.Change, strings.Replace(view.SQL, "1 AS a", "1 AS a, 2 AS b", 1), false); err != nil {
		t.Fatalf("view: %v\n%s", err, view.SQL)
	}
	admin.Exec("CREATE OR REPLACE VIEW rimor_change.v AS SELECT 1 AS a, 3 AS b") // someone else
	if _, err := apply(t, s, view.Change, view.SQL, false); !errors.As(err, new(*ChangedError)) {
		t.Errorf("conflict not seen: %v", err)
	}
	admin.Exec("DROP FUNCTION rimor_change.twice")
	var changed *ChangedError
	if _, err := apply(t, s, fn.Change, fn.SQL, false); !errors.As(err, &changed) || !changed.Gone {
		t.Errorf("a dropped function: %v", err)
	}
}

// TestChangeSQLServer uses a global temporary procedure: the test database
// only gets temporary objects.
func TestChangeSQLServer(t *testing.T) {
	dsn := serverDSN(t, "RIMOR_TEST_SQLSERVER")
	ctx := context.Background()
	p := NewPool(Config{Driver: SQLServer, DSN: dsn})
	defer p.Close()
	owner, err := p.Session(ctx, "") // keeps the ## procedure alive
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(owner)
	mustExec(t, owner, "SET QUOTED_IDENTIFIER OFF",
		"IF OBJECT_ID('tempdb..##rimor_chg') IS NOT NULL DROP PROCEDURE ##rimor_chg",
		"CREATE PROCEDURE ##rimor_chg AS SELECT \"one\" AS a", // "…" is a string with QUOTED_IDENTIFIER OFF
		"SET QUOTED_IDENTIFIER ON")

	// Temporary procedures are not in the explorer: straight to the script.
	sc, err := p.changeScript(ctx, Object{Kind: KindProcedure, Name: "##rimor_chg"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sc.SQL, "SET QUOTED_IDENTIFIER OFF;\nGO\nALTER PROCEDURE ##rimor_chg") {
		t.Fatalf("script keeps the procedure's settings:\n%s", sc.SQL)
	}
	text := strings.Replace(sc.SQL, `"one"`, `"two"`, 1)
	if _, err := apply(t, owner, sc.Change, text, false); err != nil {
		t.Fatal(err)
	}
	var a string
	if err := owner.QueryRowContext(ctx, "EXEC ##rimor_chg").Scan(&a); err != nil || a != "two" {
		t.Errorf("the procedure returns %q (%v)", a, err)
	}
	if _, err := apply(t, owner, sc.Change, text, false); !errors.As(err, new(*ChangedError)) {
		t.Errorf("the tab's old definition was not compared: %v", err)
	}
}

func TestChangeMySQL(t *testing.T) {
	eachMySQL(t, func(t *testing.T, dsn string) {
		p := mysqlShop(t, dsn)
		ctx := context.Background()
		s, err := p.Session(ctx, "rimor_shop")
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release(s)
		var version string
		s.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version)
		maria := strings.Contains(version, "MariaDB")
		obj := func(k Kind, name string) Object {
			return Object{Kind: k, Schema: "rimor_shop", Database: "rimor_shop", Name: name}
		}
		value := func(q string) (v string) {
			t.Helper()
			if err := s.QueryRowContext(ctx, q).Scan(&v); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			return v
		}

		// A function, in the sql_mode it was created in.
		fn := changeScriptOf(t, p, obj(KindFunction, "add_one"))
		if maria != strings.Contains(fn.SQL, "CREATE OR REPLACE") {
			t.Fatalf("function script:\n%s", fn.SQL)
		}
		next, err := apply(t, s, fn.Change, strings.Replace(fn.SQL, "n + 1", "n + 2", 1), false)
		if err != nil {
			t.Fatal(err)
		}
		if v := value("SELECT rimor_shop.add_one(1)"); v != "3" || !strings.Contains(next, "n + 2") {
			t.Errorf("function not changed: %s, now %q", v, next)
		}

		// A broken body leaves the function as it was, and no trial behind.
		fn.Change.Original = next
		if _, err := apply(t, s, fn.Change, strings.Replace(fn.SQL, "RETURN n + 1", "RETURN n +", 1), false); err == nil {
			t.Fatal("a broken function was applied")
		}
		if v := value("SELECT rimor_shop.add_one(1)"); v != "3" {
			t.Errorf("the function was lost: %s", v)
		}
		if n := value("SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema = 'rimor_shop'"); n != "2" {
			t.Errorf("%s routines: a trial was left behind", n)
		}

		// Another name: refused on MySQL, where add_one would be dropped;
		// on MariaDB plain SQL that creates add_two beside it.
		if _, err := apply(t, s, fn.Change, strings.Replace(fn.SQL, "add_one", "add_two", 1), false); err == nil && !maria {
			t.Error("a renamed function was applied")
		}
		if v := value("SELECT rimor_shop.add_one(1)"); v != "3" {
			t.Errorf("a renamed function replaced add_one: %s", v)
		}

		// A procedure with a compound body.
		proc := changeScriptOf(t, p, obj(KindProcedure, "settle"))
		body := strings.Replace(proc.SQL, "SET done = 1", "BEGIN\n  SET done = 2;\n  SET done = done + 1;\nEND", 1)
		if _, err := apply(t, s, proc.Change, body, false); err != nil {
			t.Fatalf("procedure: %v\n%s", err, body)
		}
		s.ExecContext(ctx, "CALL rimor_shop.settle('x', @d)")
		if v := value("SELECT @d"); v != "3" {
			t.Errorf("procedure not changed: %s", v)
		}

		// A trigger.
		tr := changeScriptOf(t, p, obj(KindTrigger, "touch"))
		if _, err := apply(t, s, tr.Change, strings.Replace(tr.SQL, "COALESCE(NEW.region, 'north')", "'south'", 1), false); err != nil {
			t.Fatalf("trigger: %v\n%s", err, tr.SQL)
		}
		s.ExecContext(ctx, "INSERT INTO rimor_shop.customers (name) VALUES ('Linus')")
		if v := value("SELECT region FROM rimor_shop.customers WHERE name = 'Linus'"); v != "south" {
			t.Errorf("trigger not changed: %s", v)
		}

		// A view, replaced in place on both.
		view := changeScriptOf(t, p, obj(KindView, "late_orders"))
		if !strings.Contains(view.SQL, "CREATE OR REPLACE") {
			t.Fatalf("view script:\n%s", view.SQL)
		}
		if _, err := apply(t, s, view.Change, strings.Replace(view.SQL, "AS `delivery_date`", "AS `delivery_date`,`total`", 1), false); err != nil {
			t.Fatalf("view: %v\n%s", err, view.SQL)
		}
		if n := value("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'rimor_shop' AND table_name = 'late_orders'"); n != "3" {
			t.Errorf("view has %s columns", n)
		}

		// Someone else's change.
		s.ExecContext(ctx, "DROP PROCEDURE rimor_shop.settle")
		var changed *ChangedError
		if _, err := apply(t, s, proc.Change, body, false); !errors.As(err, &changed) || !changed.Gone {
			t.Errorf("a dropped procedure: %v", err)
		}
	})
}
