package agent

// The tools an agent sees. The running rimor implements them (package
// ui); rimor mcp only lists them and passes calls on.

// Tool is an MCP tool definition.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// Instructions tell the agent what rimor is and how the tools fit
// together; MCP clients add them to the agent's context.
const Instructions = `rimor is the terminal SQL workbench the user has open, for PostgreSQL, SQL Server and SQLite. These tools read the catalog of the user's saved connections and put SQL in front of the user as query tabs.

You cannot execute SQL. open_query and update_query only write a tab; the user reviews it and runs it. Say so when you hand a query over, and do not claim to know its results.

Typical use: list_connections to find the connection the user means (by its name), search_objects to find tables and columns by name, describe for the exact columns, keys and foreign keys before writing joins, then open_query. Write in the connection's dialect (PostgreSQL, T-SQL or SQLite). On a read-only connection writes will be refused, so offer the statement but say it cannot run there.

After you hand a tab over, wait_for_run waits until the user has run it and reports how it went: failed and why, or which columns and how many rows. The rows themselves are private unless the user lets you read them for that tab (they press a key in rimor; pass want_results to ask). Then wait_for_run includes the first rows and read_results pages through the rest.

For work that takes many queries, such as diagnosing a slow server, keep one tab and revise it with update_query for each step: the user reads it, runs it, and wait_for_run gives you the result. Put one statement that returns rows in each run: rimor shows only the first result set of a batch. Ask before anything that writes; the user is warned about SQL that may write.`

// object is a JSON schema for an object with these properties, of which
// required ones must be given.
func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

const (
	connDesc = "Name of a saved connection, as list_connections gives it."
	dbDesc   = "Database on the connection's server; omit for the connection's default database. SQLite connections have no databases."
)

var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

// Tools lists the tools in the order clients show them.
var Tools = []Tool{
	{
		Name:  "list_connections",
		Title: "List connections",
		Description: "The user's saved database connections: name, driver (PostgreSQL, SQL Server, SQLite) and whether they are read-only. " +
			"For a connection that is already open, also the server version. Hosts, users and passwords are never shown.",
		InputSchema: object(map[string]any{}),
		Annotations: readOnly,
	},
	{
		Name:  "list_objects",
		Title: "List catalog objects",
		Description: "Walk a connection's catalog one level at a time, as rimor's explorer does. " +
			"With just a connection: its databases (and the server version). With a database: its schemas. " +
			"With a schema: its tables, views, materialized views, functions, procedures and sequences, optionally only one kind. " +
			"SQLite has neither databases nor schemas: the connection alone lists its tables, views, indexes and triggers. " +
			"Built-in schemas are left out; schemas with nothing in them are listed separately. For large databases search_objects is quicker.",
		InputSchema: object(map[string]any{
			"connection": str(connDesc),
			"database":   str(dbDesc),
			"schema":     str("Schema to list the objects of. Requires database on PostgreSQL and SQL Server."),
			"kind": map[string]any{"type": "string", "description": "Only objects of this kind, when listing a schema.",
				"enum": []string{"table", "view", "materialized view", "function", "procedure", "sequence", "index", "trigger"}},
		}, "connection"),
		Annotations: readOnly,
	},
	{
		Name:  "search_objects",
		Title: "Search names",
		Description: "Find tables, views, functions, procedures and columns whose names contain the text, ignoring case, " +
			"in one database. Columns come with their table and type. Tables and views come first; at most 200 matches.",
		InputSchema: object(map[string]any{
			"connection": str(connDesc),
			"database":   str(dbDesc),
			"text":       str("Part of a name, e.g. \"deliv\" or \"customer\". Not a pattern: % and _ match themselves."),
		}, "connection", "text"),
		Annotations: readOnly,
	},
	{
		Name:  "describe",
		Title: "Describe a table",
		Description: "A table or view in full: columns with type, nullability, default and comment; primary key; indexes; " +
			"its foreign keys and the foreign keys of other tables that reference it.",
		InputSchema: object(map[string]any{
			"connection": str(connDesc),
			"database":   str(dbDesc),
			"schema":     str("Schema of the table; omit for the default one (public, dbo). Ignored for SQLite."),
			"name":       str("Table or view name, unquoted."),
		}, "connection", "name"),
		Annotations: readOnly,
	},
	{
		Name:  "open_query",
		Title: "Open a query tab",
		Description: "Open a new query tab in rimor with this SQL, running on the given connection and database, and show it to the user. " +
			"It is NOT executed: the user reviews it and runs it. Any statement is allowed (SELECT, DML, DDL); " +
			"put several in one tab separated by semicolons if they belong together. Returns the tab's id.",
		InputSchema: object(map[string]any{
			"connection":   str(connDesc),
			"database":     str(dbDesc),
			"sql":          str("The SQL, formatted for a person to read."),
			"title":        str("Short tab name, e.g. \"expired-deliveries\"."),
			"want_results": wantResults,
		}, "connection", "sql"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
	},
	{
		Name:  "update_query",
		Title: "Revise a query tab",
		Description: "Replace the SQL of a tab you opened, e.g. when the user asks for a change; the user can undo it. " +
			"Refused if the user has edited the tab since: then open a new tab. Like open_query, it never executes anything. " +
			"Give connection (and database) to change where the tab runs; that ends any permission to read its results.",
		InputSchema: object(map[string]any{
			"tab":          map[string]any{"type": "integer", "description": "Tab id from open_query or list_tabs."},
			"sql":          str("The new SQL."),
			"connection":   str(connDesc),
			"database":     str(dbDesc),
			"want_results": wantResults,
		}, "tab", "sql"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
	},
	{
		Name:  "list_tabs",
		Title: "List query tabs",
		Description: "The open query tabs: id, title, connection and database, whether you opened it, which one the user is looking at, " +
			"and whether you may read its results.",
		InputSchema: object(map[string]any{}),
		Annotations: readOnly,
	},
	{
		Name:  "read_tab",
		Title: "Read a query tab",
		Description: "A tab's SQL and context, the text the user has selected in it, and the outcome of its last run: " +
			"running, failed (with the database's message and code, and where in the SQL), or done (columns, row count, rows affected). " +
			"Result rows are never included: see read_results. Without tab, the tab the user is looking at.",
		InputSchema: object(map[string]any{
			"tab": map[string]any{"type": "integer", "description": "Tab id; omit for the current tab."},
		}),
		Annotations: readOnly,
	},
	{
		Name:  "wait_for_run",
		Title: "Wait for the user to run a tab",
		Description: "Wait until the user runs the tab and the run ends (or the run in progress ends), then report it as read_tab does, " +
			"with the first rows if you may read the tab's results. Waits at most 60 seconds; if the user has not run it by then, " +
			"it says so and you can call it again. A run that ended before the call is not waited for: read it with read_tab or read_results.",
		InputSchema: object(map[string]any{
			"tab":             map[string]any{"type": "integer", "description": "Tab id; omit for the current tab."},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "description": "How long to wait; default and most 60."},
		}),
		Annotations: readOnly,
	},
	{
		Name:  "read_results",
		Title: "Read a tab's result rows",
		Description: "Rows of the tab's last result, a page at a time, with the run's outcome. Only for tabs whose results the user lets you read; " +
			"otherwise the error says how to ask. Values are text, or null. At most 500 rows (100 by default) and about 64 KB per page; " +
			"cells over 1 KB are cut and end in …. next_offset, when present, is where the next page starts; it is also present while rows are still arriving.",
		InputSchema: object(map[string]any{
			"tab":    map[string]any{"type": "integer", "description": "Tab id; omit for the current tab."},
			"offset": map[string]any{"type": "integer", "minimum": 0, "description": "First row, from 0."},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "Rows to return; default 100."},
		}),
		Annotations: readOnly,
	},
}

var wantResults = map[string]any{"type": "boolean",
	"description": "Ask the user to let you read this tab's result rows; rimor shows the request, and only the user can allow it."}
