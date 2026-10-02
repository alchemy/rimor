package db

import (
	"context"
	"database/sql"
	"sync"
)

// Pool holds the open handles of one saved connection, one per database:
// Postgres cannot switch databases on a connection, and keeping SQL Server
// the same keeps catalog queries free of three-part names.
//
// It is safe for concurrent use; loads run in background commands.
type Pool struct {
	cfg   Config
	intro Introspector

	mu       sync.Mutex
	dbs      map[string]*sql.DB // keyed by database; "" is the DSN default
	sessions map[*sql.Conn]bool // handed out by Session, closed with the pool
}

func NewPool(cfg Config) *Pool {
	return &Pool{cfg: cfg, intro: IntrospectorFor(cfg.Driver), dbs: map[string]*sql.DB{}, sessions: map[*sql.Conn]bool{}}
}

func (p *Pool) Config() Config { return p.cfg }

// Connected reports whether any database is open.
func (p *Pool) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dbs) > 0
}

// DB returns the handle for a database, connecting on first use.
func (p *Pool) DB(ctx context.Context, database string) (*sql.DB, error) {
	if !p.cfg.Driver.HasDatabases() {
		database = ""
	}
	p.mu.Lock()
	conn := p.dbs[database]
	p.mu.Unlock()
	if conn != nil {
		return conn, nil
	}

	conn, err := OpenDatabase(ctx, p.cfg, database)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.dbs[database]; existing != nil {
		conn.Close() // another load connected first
		return existing, nil
	}
	p.dbs[database] = conn
	return conn, nil
}

// Children lists the children of parent (nil for the top level) using the
// handle of the database the parent lives in. Children inherit it.
func (p *Pool) Children(ctx context.Context, parent *Object) ([]Object, error) {
	database := ""
	if parent != nil {
		database = parent.Database
	}
	conn, err := p.DB(ctx, database)
	if err != nil {
		return nil, err
	}
	objs, err := p.intro.Children(ctx, conn, parent)
	for i := range objs {
		if objs[i].Database == "" {
			objs[i].Database = database
		}
	}
	return objs, err
}

// Close closes every session and open handle. The pool can be used again
// afterwards; sessions handed out before fail with sql.ErrConnDone.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for s := range p.sessions {
		// Closing waits for the session's open rows; the UI stops fetches
		// first, but never block on one that is still winding down.
		go s.Close()
		delete(p.sessions, s)
	}
	for name, conn := range p.dbs {
		conn.Close()
		delete(p.dbs, name)
	}
}
