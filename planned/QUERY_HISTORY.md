# Query history

Status: planned, not started.

Every statement run is kept, so that last week's query can be found and run
again without hunting through tabs and files. tabletist has this, per
connection and searchable; rimor has nothing like it today.

## What is kept

One entry per run, written when the run ends:

| Field | Example |
|---|---|
| When | `2026-10-22 09:14` |
| Connection, database | `ERP › sales` |
| SQL | what ran: the selection, or the whole tab |
| Outcome | `100 rows`, `3 rows affected`, `error 8134`, `cancelled` |
| Duration | `1.2 s` |
| Source | the user's tab, or a tab an agent opened |

- Runs of the same SQL on the same connection and database, one after the
  other, are one entry with a count and the last time, so a query rerun
  while tuning does not fill the list.
- Result rows are never kept, only the counts.
- One-shot sessions (`rimor postgres://…`) keep nothing, as they keep
  nothing else.

## Storage

- `history.jsonl` in the state folder, beside the session, readable only
  by the user (mode 0600), one JSON line per entry, appended.
- The last 5,000 entries are kept, older ones dropped: `history_size` in
  `config.toml`, `0` turns history off.
- SQL can hold sensitive values (literals, customer codes), so the README
  says what is kept and where, and how to turn it off or clear it.

## Using it

- **`ctrl+r`** (as in shells), app-wide: a dialog lists the history, newest
  first, grouped by day, with the connection, the outcome and the first
  line of the SQL. Typing filters on the SQL, the connection and the
  database, as the context picker does.
- **Narrowing:** `tab` cycles between all connections and the current tab's
  only.
- **`enter`** opens the entry in a new tab, on its connection and database,
  not run. **`alt+enter`** puts it in the current tab instead, undoable.
- **`y`** copies the SQL; **`d`** deletes the entry (for a query that should
  not have been kept).
- A preview below the list shows the whole statement, highlighted.

## Elsewhere

- **Agents:** none at first. History is the user's record, and could hold
  queries the user would not hand to an agent. A `read_history` tool could
  come later, behind its own setting.
- **README and keys:** the README (a History section; `history_size` in
  Configuration), the key tables in `internal/ui/keys.go` (a new scope for
  the dialog) and the `config.toml` template need updating;
  `TestKeyTablesMatchHandlers` fails until the tables match.

## Open decisions

1. **Entries that failed:** keep them (useful: "the query I was fixing"),
   or only runs that succeeded? Recommended: keep them, marked.
2. **One file, or one per connection?** One file is simpler and lets the
   filter span connections. Recommended: one file.
