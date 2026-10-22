# Exporting results

Status: planned, not started.

A result can be saved to a file or copied in a format other tools read.
Today rimor copies one cell or one row; neither rimor nor tabletist exports
a whole result.

## Formats

| Format | Notes |
|---|---|
| CSV | RFC 4180 quoting, a header row, `,` or `;` (Excel in Italian and other locales expects `;`), UTF-8 with an optional BOM for Excel |
| TSV | tabs, as the row copy already does |
| JSON | an array of objects, column names as keys; numbers as numbers where the column is numeric, NULL as `null` |
| JSON Lines | one object per line, for large results and for tools such as `jq` |
| SQL `INSERT`s | in the tab's dialect, names quoted as the scripts quote them; the table is the result's own when it comes from one table, else `result` to rename |
| Markdown table | for pasting into issues and documents |

- **NULL:** empty in CSV and TSV by default (or `NULL`, or `\N`, chosen in
  the dialog), `null` in JSON, `NULL` in SQL.
- **Values** are written as the grid shows them (dates, decimals), so an
  export matches what was seen.

## Using it

- **`e`** in the results pane opens a dialog: the format, its options, and
  where it goes.
  - **To a file:** a path, with completion as when saving a tab; the
    extension follows the format.
  - **To the clipboard:** for small results. Terminals limit how much the
    clipboard sequence (OSC 52) carries, so above about 1 MB rimor says so
    and suggests a file.
- **What is exported:** the current result (with several result sets, the
  one shown), all its columns, and the rows that arrived. The dialog says
  how many, and warns when fetching stopped early (`esc`, or the memory
  limit): then not every row is there.

## Results larger than memory

Rows stop at `result_memory_mb`, so a large result is never whole in rimor.
For those, the dialog offers **run again into the file**: the statement runs
once more and its rows stream straight to the file, without the limit, with
progress and `esc` to stop.

- Offered only for statements that do not write (the check the agent
  warning uses, `db.MayWrite`), since running a write twice could change
  data twice.
- It runs on the tab's session, so temp tables and session settings still
  apply.

## Elsewhere

- **Agents:** no export tool. An agent that may read a tab's rows already
  gets them through `read_results`; writing files is for the user.
- README (Results feature, Keys), the results key table and a new dialog
  scope in `internal/ui/keys.go` need updating.

## Open decisions

1. **The CSV separator's default:** `,`, or taken from the system locale?
   Recommended: `,`, with `;` one key away in the dialog, and remembered.
2. **The SQL format's batch size:** one `INSERT` per row, or multi-row
   `INSERT … VALUES (…), (…)` in batches (faster to load, but SQL Server
   allows at most 1,000 rows per `VALUES`)? Recommended: batches of 100.
