# Autocomplete in the editor

Status: planned, not started.

The editor offers names as you type: keywords, schemas, tables and views,
and the columns of the tables a statement uses. tabletist has this; rimor
has highlighting but no completion today.

## What is offered, where

The cursor's place in the statement decides the list:

| After | Offered |
|---|---|
| `FROM`, `JOIN`, `UPDATE`, `INTO`, `TABLE` | schemas, tables and views; after `schema.`, that schema's |
| `alias.` or `table.` | that table's columns |
| `SELECT`, `WHERE`, `ON`, `GROUP BY`, `ORDER BY`, `SET` | the columns of the statement's tables (with their alias when they have one), then functions |
| `EXEC`, `CALL` | procedures; on PostgreSQL, functions after `SELECT` too |
| anywhere else | the dialect's keywords |

- Matching ignores case and starts at the beginning of the word (`cust`
  finds `customers`, not `old_customers`), then within words. Names are
  inserted quoted when they need it, in the tab's dialect, as the
  explorer's scripts quote them (`quoter` in `internal/db/script.go`).
- The statement under the cursor is read with the lexers the highlighter
  already uses (chroma's, per dialect, in `internal/editor/highlight.go`),
  so strings and comments are skipped.
  Statements need not be complete or valid: completion is for half-written
  SQL.

## Where the names come from

- The tab's connection and database, through the catalog queries the
  explorer and the agent tools already use (`Pool.Children`,
  `Pool.Search`, `Pool.Describe`).
- A cache per database, filled in the background: the tables and views on
  the first completion, a table's columns the first time they are needed.
  The editor never waits for the database: until names arrive, keywords
  are offered, and the list updates when they do.
- The cache is dropped when the explorer refreshes that database (`r`) and
  when the tab changes context.

## Using it

- **`ctrl+space`** asks for the list anywhere. It also opens by itself after
  `.` and after a few typed letters of a name, unless turned off
  (`autocomplete = "manual"` in `config.toml`).
- A small list under the cursor, at most ten rows, each with its kind
  (table, view, column and its type, keyword).
- **`tab`** or **`enter`** accepts, **`↑`/`↓`** move, **`esc`** closes; typing
  goes on narrowing it. While the list is open, `tab` accepts rather than
  indents and `enter` accepts rather than breaking the line.
- Terminals report `ctrl+space` differently (`ctrl+@`, NUL); both are read
  as the same key, and the leader key gets an alternative (`ctrl+g` then
  space).

## Elsewhere

- The cell editor's JSON mode does not complete.
- README (Editor feature, Keys), the editor's key table and the `config.toml`
  template need updating.

## Open decisions

1. **Opening by itself or only on `ctrl+space`?** A list that appears while
   typing helps most users and annoys some. Recommended: by itself, with
   the setting to turn it off.
2. **Functions:** the database's own (user-defined) only, or the built-in
   ones too? Built-in lists per dialect are long and need maintaining.
   Recommended: user-defined first, built-ins later.
