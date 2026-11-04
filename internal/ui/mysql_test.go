package ui

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	_ "github.com/go-sql-driver/mysql"

	"rimor.dev/internal/config"
	"rimor.dev/internal/db"
)

// TestMySQLInTheUI runs against each server in RIMOR_TEST_MYSQL and
// RIMOR_TEST_MARIADB (see internal/db/mysql_test.go).
func TestMySQLInTheUI(t *testing.T) {
	ran := false
	for _, env := range []string{"RIMOR_TEST_MYSQL", "RIMOR_TEST_MARIADB"} {
		dsn := os.Getenv(env)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(strings.ToLower(strings.TrimPrefix(env, "RIMOR_TEST_")), func(t *testing.T) { mysqlUI(t, dsn) })
	}
	if !ran {
		t.Skip("RIMOR_TEST_MYSQL and RIMOR_TEST_MARIADB not set")
	}
}

func mysqlUI(t *testing.T, dsn string) {
	admin, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS rimor_ui"); admin.Close() })
	if _, err := admin.Exec("DROP DATABASE IF EXISTS rimor_ui; CREATE DATABASE rimor_ui; " +
		"CREATE TABLE rimor_ui.orders (id INT PRIMARY KEY, status VARCHAR(10)); INSERT INTO rimor_ui.orders VALUES (1, 'new'), (2, 'late')"); err != nil {
		t.Fatal(err)
	}

	store := db.Store{Path: filepath.Join(t.TempDir(), "c.json")}
	store.Save([]db.Config{{Name: "shop", Driver: db.MySQL, DSN: dsn}})
	settings := config.Defaults()
	settings.Icons = config.IconsPlain
	d := &driver{t: t, m: New(store, "", settings), wait: 5 * time.Second}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 34})
	t.Cleanup(func() {
		for _, tb := range d.m.query.tabs {
			if tb.session != nil {
				db.CloseSession(tb.session)
			}
		}
		d.m.explorer.Close()
	})

	// The explorer lists the databases, the system ones left out.
	d.key("down", "enter")
	tree := d.tree()
	if !strings.Contains(tree, "shop  mysql") || !strings.Contains(tree, "rimor_ui") || strings.Contains(tree, "performance_schema") {
		t.Fatalf("explorer:\n%s", tree)
	}

	// A tab on the database runs a batch that starts with SET.
	for d.m.explorer.cursor.obj.Name != "rimor_ui" {
		d.key("down")
	}
	d.key("ctrl+t")
	if cur := d.m.query.current(); cur.ctx.database != "rimor_ui" {
		t.Fatalf("context %+v", cur.ctx)
	}
	s := d.runSQL("SET @late := 'late';\nSELECT id, status FROM orders WHERE status = @late")
	if !strings.Contains(s, "late") || !strings.Contains(d.status(), "1 row") {
		t.Errorf("results:\n%s\nstatus %q", s, d.status())
	}

	// A transaction shows in the status line.
	d.runSQL("BEGIN; UPDATE orders SET status = 'done' WHERE id = 1")
	if !strings.Contains(d.queryStatus(), "in transaction") {
		t.Errorf("no marker: %q", d.queryStatus())
	}
	d.runSQL("ROLLBACK")
	if strings.Contains(d.queryStatus(), "transaction") {
		t.Errorf("marker after rollback: %q", d.queryStatus())
	}

	// The agent: a MySQL database lists its objects, with no schema level.
	got := d.mustCall("list_objects", `{"connection":"shop","database":"rimor_ui"}`)
	if !strings.Contains(got, `"tables":["orders"]`) {
		t.Errorf("list_objects: %s", got)
	}
	got = d.mustCall("describe", `{"connection":"shop","database":"rimor_ui","name":"orders"}`)
	if !strings.Contains(got, `"schema":"rimor_ui"`) || !strings.Contains(got, `"primary_key":true`) {
		t.Errorf("describe: %s", got)
	}
}
