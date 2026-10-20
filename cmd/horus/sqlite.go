package main

// The SQLite driver is registered here, not in internal/store, so that the
// store package itself only depends on database/sql.
import _ "modernc.org/sqlite"

const sqliteDriver = "sqlite"
