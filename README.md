# rimor

*rimor* (Latin): I probe, I search thoroughly.

A terminal workbench for relational databases: browse the catalog, write SQL
in tabs with syntax highlighting, and explore results in a scrollable grid.
It supports **PostgreSQL**, **SQL Server** and **SQLite**.

```
╭─ 1  Explorer ─────────╮╭─ 2  ○ untitled-1 ─ ● untitled-2 ● ──────────────────────────────────────╮
│   + New connection    ││  1  SELECT c.name, c.city, count(o.id) AS orders, sum(o.total) AS total │
│                       ││  2  FROM customers c LEFT JOIN orders o ON o.customer_id = c.id         │
│ ▾ ◈ shop  sqlite      ││  3  GROUP BY c.id ORDER BY total DESC                                   │
│   ▾ ▫ Tables  2       ││                                                                         │
│     ▾ ▦ customers     ││                                                                         │
│       ▾ ▫ Columns  4  ││                                                                         │
│           ⚷ id  INTEG…│╰─ ◈ shop ────────────────────────────────────────────────────────────────╯
│           · name  TEXT│╭─ 3  Results ────────────────────────────────────────────────────────────╮
│           · city  TEXT││   name             city       orders  total                             │
│           · since  DA…││─────────────────────────────────────────────────────────────────────────│
│       ▸ ▪ Indexes     ││ 1 Grace Hopper     Arlington       2    995                             │
│     ▸ ▦ orders        ││ 2 Ada Lovelace     London          2  155.5                             │
│   ▸ ▪ Views           ││ 3 Barbara Liskov   Boston          1  42.25                             │
│   ▸ ▪ Indexes         ││ 4 Edsger Dijkstra  NULL            0   NULL                             │
│   ▸ ▪ Triggers        ││                                                                         │
│                       ││                                                                         │
│                       ││                                                                         │
│                       ││                                                                         │
│                       ││                                                                         │
│                       ││                                                                         │
╰───────────────────────╯╰─ ✓ 4 rows · <1 ms ──────────────────────────── 2/4 · name text  y copy ─╯
```

## Features

- **Explorer.** Saved connections and the catalog as a tree: databases,
  schemas, tables, views, materialized views, functions, procedures,
  sequences, indexes, triggers and columns with their types. Levels load
  when you expand them, without freezing the UI. Built-in schemas are left
  out, and so are schemas with nothing in them, so a typical SQL Server
  database shows just `dbo`. `.` on a database lists the empty ones too.
- **Query tabs.** Each tab has a *context*, a connection plus an optional
  database, shown in the pane's bottom border. A new tab takes the context
  of the item selected in the explorer, and `ctrl+e` changes it. Tabs save
  to `.sql` files, and the session (tabs, unsaved text, contexts, layout)
  comes back on the next start.
- **Editor.** Syntax highlighting for each dialect (PostgreSQL, T-SQL,
  SQLite), selection, undo/redo, clipboard, and indentation that follows
  the previous line.
- **Results.** Rows stream in the background: the first ones show within
  milliseconds while the rest arrive, with a live count, and `esc` stops
  fetching but keeps what came. They are stored compactly (a few bytes of
  overhead per cell), so millions of rows are fine. The grid scrolls
  vertically by row and horizontally by column, with right-aligned numbers
  and visible `NULL`s. Statements that
  change data show the rows affected. Errors show the database's code and
  message, plus the failing line with a caret where the driver reports a
  position.
- **Cell viewer and editing.** `enter` on a result cell opens its full value,
  selected for copying. When the result comes from one table and includes
  its key, the value can be edited there: rimor shows the `UPDATE` it will
  run, changes only that row, and refuses if the row changed since it was
  loaded. Editing works on PostgreSQL and SQL Server.
- **Read-only connections.** A connection can be marked read-only: the
  server (PostgreSQL) or the engine (SQLite) refuses writes, and on SQL Server
  rimor refuses statements that write.
- **Sessions per tab.** Each tab runs on its own connection, so `USE`, `SET`,
  temp tables and open transactions carry over between runs.
- **Layout.** Drag the borders between panes, or resize from the keyboard.
  Any pane can go full screen.
- **Configurable.** Shortcuts can be remapped, and a leader key gives every
  `alt` shortcut an alternative for terminals that keep `alt` for themselves.

## Install

rimor is pure Go with no C dependencies; Go 1.27 or newer builds it for
Linux, macOS and Windows.

```sh
go install rimor.dev/cmd/rimor@latest
```

(This needs `rimor.dev` to point Go at the repository; until it does, build
from a checkout.)

From a checkout:

```sh
go build ./cmd/rimor
./rimor
```

## Themes

`theme` in `config.toml` picks the colours:

| Theme | |
|---|---|
| `auto` (default) | On Omarchy, `terminal`, so rimor follows the system theme. Elsewhere `dark` or `light`, matching the background the terminal reports |
| `dark` | Catppuccin Mocha |
| `light` | Catppuccin Latte |
| `terminal` | The terminal's own 16 colours; the cursor row and selection are shaded from its background |

rimor never paints the terminal's background. Setting `NO_COLOR` turns colours
off, with the cursor row and selection in reverse video.

The Nerd Font icons appear automatically when your terminal bundles them
(kitty, Ghostty, WezTerm) or a Nerd Font is installed; otherwise rimor uses
plain Unicode symbols. See `icons` in [Configuration](#configuration).

## Editing data

`enter` on a result cell opens it. The value is selected, so `ctrl+c` copies
it. When rimor can tell which table row the cell comes from, the value can
be changed in place:

| Key | Single-line value | Multi-line value or JSON |
|---|---|---|
| `enter` | save | new line |
| `ctrl+s` | save | save |
| `alt+enter` | new line | new line |
| `ctrl+f` | | format JSON |
| `ctrl+n` | set NULL | set NULL |
| `esc` | close without saving | close without saving |

Saving runs the `UPDATE` shown below the value.

**JSON** columns (`json` and `jsonb` on PostgreSQL; on SQL Server the 2025
`json` type, and text columns with an enabled `ISJSON` check constraint)
open highlighted in a larger editor, and are checked before saving:
invalid JSON is never sent, and the cursor goes to the fault. `jsonb` and
SQL Server's `json` open pretty-printed, since the database stores its own
form anyway; PostgreSQL's `json` and SQL Server text columns keep text
exactly as written, so they open as stored and `ctrl+f` formats them on
request.

Editing needs a result from a single table that includes the table's
primary key, or a unique key on non-null columns. Key columns and
columns the database computes are not editable. The update only applies if
the cell still holds the value it was opened with; otherwise rimor says the
row changed and leaves it alone. It runs on the tab's connection, so inside
an open transaction it can still be rolled back.

## Connections

Press `a` in the explorer, or Enter on **New connection**, and give it a
name, a driver and a connection string. `ctrl+t` in the form tests the
connection.

| Driver | Connection string |
|---|---|
| PostgreSQL | `postgres://user:pass@localhost:5432/db?sslmode=disable` |
| SQL Server | `sqlserver://user:pass@localhost:1433?database=db` |
| SQLite | `/path/to/database.db` |

Characters with a special meaning in URLs must be percent-encoded in
passwords (`@` is `%40`). For SQL Server, the URL path names an *instance*,
not a database; the database goes in `?database=`. Add
`&TrustServerCertificate=true` for servers with self-signed certificates.

Connection strings are stored in plain text in `connections.json`, which is
readable only by you.

Mark a connection **read-only** in the same form. On PostgreSQL the server
then refuses writes, and on SQLite the engine does. SQL Server has no
read-only session setting, so rimor refuses statements that write, `EXEC`
included; for a hard guarantee, use a login without write permissions.

## Keys

App-wide shortcuts work in every pane, including the editor. Each one also
works as the leader key (`ctrl+g`) followed by a second key; press `ctrl+g`
on its own to list them.

| Keys | Action |
|---|---|
| `alt+1` `alt+2` `alt+3` | Focus explorer, query, results |
| `alt+f` | Toggle full screen for the focused pane |
| `shift+alt+1/2/3` | Show a pane in full screen |
| `ctrl+enter` `F5` `alt+enter` | Run the selection, or the whole tab |
| `ctrl+t` | New tab, in the context of the explorer's selection |
| `ctrl+o` | Open a `.sql` file |
| `alt+shift+←/→` `alt+shift+↑/↓` | Resize the explorer / the query editor |
| `alt+=` | Reset pane sizes |
| `ctrl+q` | Quit |

**Explorer:** `j`/`k` or the arrows move, `enter`/`l` expands, `h` collapses,
`a` adds a connection, `e` edits it, `d` deletes it, `r` refreshes, `x`
disconnects, `.` shows or hides a database's empty schemas.

**Query:** `ctrl+s` saves, `alt+s` saves as, `ctrl+w` closes the tab,
`ctrl+pgup/pgdn` or `alt+[`/`alt+]` switch tabs, `ctrl+e` changes the context,
`esc` cancels a running query.

**Results:** `enter` opens the cell, `esc` stops fetching, the arrows or `hjkl` move, `pgup`/`pgdn` page, `g`/`G` jump to the
first or last row, `home`/`end` to the first or last column, `y` copies the
cell, `Y` copies the row.

Outside the editor, `tab` cycles focus and `q` quits. The mouse focuses panes
and drags the borders between them; a double click on a border resets it.
Use `shift`+drag for the terminal's own text selection.

### macOS and Windows terminals

- **macOS:** Terminal.app and iTerm2 send special characters for Option+key by
  default. Enable *Use Option as Meta key* (Terminal) or *Option key: Esc+*
  (iTerm2), or use the leader key. In Ghostty, set `macos-option-as-alt`.
- **Windows Terminal** sends `ctrl+enter` as a plain Enter, and uses
  `alt+enter` and `alt+shift+arrows` itself. Run with `F5`, resize with
  `ctrl+g` then the arrows, or remap the keys.
- `ctrl+enter` needs a terminal that speaks the kitty keyboard protocol
  (kitty, Ghostty, WezTerm, foot, recent Alacritty). rimor detects this and
  shows `^⏎` in its hints only where it works; elsewhere they show `F5`.

## Configuration

On first run rimor creates its configuration folder:

| | Settings and connections | Session |
|---|---|---|
| Linux, macOS | `~/.config/rimor` | `~/.local/state/rimor` |
| Windows | `%APPDATA%\rimor` | `%LOCALAPPDATA%\rimor` |

`$XDG_CONFIG_HOME` and `$XDG_STATE_HOME` take precedence when set.

`config.toml` is written with every default present but commented out;
uncomment a line to change it:

```toml
theme = "light"         # "auto", "dark", "light" or "terminal"
icons = "plain"         # "auto", "nerd" or "plain"
leader = "ctrl+b"
result_memory_mb = 2048 # memory a query's rows may take

[keys]
full_screen = ["alt+z", "leader z"]   # replaces alt+f and leader f
run = ["ctrl+r", "f5"]
```

The template lists every action. Keys inside panes (editor, explorer,
results) are not remappable yet.

## Limitations

- Statements are classified before they run: one starting with `SELECT`,
  `WITH`, `EXEC`, `SHOW` and the like, or containing `RETURNING`/`OUTPUT`,
  shows rows; anything else shows the rows affected. A mutating statement
  never runs twice to find out.
- On PostgreSQL a row-returning query must be a single statement; select it
  to run one of several. SQL Server batches work, and the first result set
  with columns is shown.
- A result's rows stay in memory, up to `result_memory_mb` (512 MB by
  default); fetching stops there and keeps what arrived. Spilling larger
  results to disk is planned.
- A schema counts as empty when the login sees no objects, user types or XML
  schema collections in it, so a schema whose objects you lack permission
  to see is hidden too. The note under the database says how many are
  hidden, and `.` shows them.
- Copying uses the terminal's clipboard support (OSC 52), which
  Terminal.app lacks and iTerm2 has off by default.

## Development

```sh
go test ./...
```

The SQLite tests always run. The server tests run when a connection string
is given:

```sh
RIMOR_TEST_POSTGRES='postgres://postgres@localhost:5432/postgres?sslmode=disable' \
RIMOR_TEST_POSTGRES_DB=warehouse \
RIMOR_TEST_SQLSERVER='sqlserver://user:pass@host?database=db&TrustServerCertificate=true' \
go test ./...
```

`RIMOR_TEST_POSTGRES_DB` names a second database on the same server, for the
tests that switch databases. The SQL Server tests only read data and use temp
tables.

| Package | Contents |
|---|---|
| `cmd/rimor` | entry point |
| `internal/ui` | panes, layout, dialogs, key handling |
| `internal/editor` | the SQL editor and highlighting |
| `internal/db` | connections, catalog queries per driver, execution |
| `internal/config` | `config.toml` and the bindable actions |
| `internal/paths` | where files live on each platform |

## License

[MIT](LICENSE)
