# Folders in the explorer

Status: planned, not started.

Users can organise saved connections in folders, as in a file system. A
connection is either at the top level or inside a folder, and folders can
have subfolders. With more than a dozen connections (per client, production
against test) a flat list stops being usable.

## Storage

Each connection gains a `folder` path. Folders are also listed on their own,
so that empty ones survive:

```json
{"folders": ["Clients", "Clients/Acme", "Clients/Acme/Archive", "Internal"],
 "connections": [
   {"name": "Acme ERP", "folder": "Clients/Acme", "driver": "sqlserver", …},
   {"name": "scratch", "driver": "sqlite", …}]}
```

- A connection without `folder` is at the top level, so existing files load
  unchanged.
- `/` separates the levels, so folder names cannot contain it.

Connection names stay unique across all folders. The session, the agent
tools and the keychain entries identify a connection by its name. With
globally unique names, moving a connection changes nothing else: tabs keep
their context, the keychain keeps its entry, and the agent's "ERP" stays
unambiguous. Unique-per-folder names, like file names, would mean
identifying connections by full path everywhere, and every move would break
those references.

## Explorer

```
▾ ▪ Clients
  ▾ ▪ Acme
    ▸ ◉ Acme ERP  mssql
    ▸ ▪ Archive
  ▸ ▪ Globex
▸ ▪ Internal
▸ ◈ scratch  sqlite
```

- Folders come first, then connections, each in alphabetical order, as in a
  file manager.
- The session remembers which folders are open.
- On a folder, `enter`, `l` and `h` open and close it as for any node, `e`
  renames it (as it edits a connection) and `d` deletes it. The other
  explorer keys (`r` refresh, `x` disconnect, `.`) keep their meaning.
- `n` creates a folder inside the selected folder, or next to the selected
  connection.
- `a` adds a connection to the current folder rather than always at the top
  level.
- `m` moves the selected connection or folder. A picker lists the folders,
  with "top level" first, and typing filters them. A folder cannot be moved
  into itself.
- The connection form shows the folder, so where a connection lives can be
  seen and changed while editing it.
- Deleting an empty folder needs no confirmation. Deleting one that is not
  empty asks first ("Delete Acme, its subfolder and its 3 connections?"),
  then removes everything inside along with the keychain entries, as
  deleting each connection would.

## Elsewhere

- The context picker (`ctrl+e`) shows the path next to the name
  (`Clients/Acme › Acme ERP`) and filters on both.
- The agent's `list_connections` gains `folder`, so a request for "Acme's
  ERP" resolves on its own.
- The README (Connections, Keys) and the explorer's key table in
  `internal/ui/keys.go` need updating. `TestKeyTablesMatchHandlers` fails
  until they are.

## Open decisions

1. **Order.** Alphabetical, as above, or the order the user arranges? Manual
   ordering needs keys to move items up and down: more to build and more to
   remember. Recommended: alphabetical.
2. **Deleting a non-empty folder.** Delete everything inside after the
   confirmation, as above, or move the contents up to the parent folder?
   Moving them can never lose a connection, but behaves less like a file
   system. Recommended: delete after confirming.

## Later

- A read-only folder, which makes every connection inside it read-only (all
  of "Production", say). Useful, but best kept out of the first version.
