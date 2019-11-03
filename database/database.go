package database

import "github.com/jinzhu/gorm"

// Tx represents a database transaction.  It can either by read-only or
// read-write.  The transaction provides access to a sql database interface
// with an open transaction to use for writing generic data.
// It also provides methods for reading and writing the node's public data.
//
// As would be expected with a transaction, no changes will be saved to the
// database until it has been committed.  The transaction will only provide a
// view of the database at the time it was created.  Transactions should not be
// long running operations.
type Tx interface {
	// Commit commits all changes that have been made to the db or public data.
	// Depending on the backend implementation this could be to a cache that
	// is periodically synced to persistent storage or directly to persistent
	// storage.  In any case, all transactions which are started after the commit
	// finishes will include all changes made by this transaction.  Calling this
	// function on a managed transaction will result in a panic.
	Commit() error

	// Rollback undoes all changes that have been made to the db or public
	// data.  Calling this function on a managed transaction will result in
	// a panic.
	Rollback() error

	// Read returns the underlying sql database in a read-only mode so that
	// queries can be made against it.
	Read() *gorm.DB

	// Save will save the passed in model to the database. If it already exists
	// it will be overridden.
	Save(i interface{}) error

	// Update will update the given key to the value for the given model. The
	// where map can be used to impose extra conditions on which specific model
	// gets updated. The map key must be of the format "key = ?". This allows
	// for using alternative conditions such as "timestamp <= ?".
	Update(key string, value interface{}, where map[string]interface{}, model interface{}) error

	// Delete will delete all models of the given type from the database where
	// key == value. The key must be of the value
	Delete(key string, value interface{}, model interface{}) error

	// Migrate will auto-migrate the database to from any previous schema for this
	// model to the current schema.
	Migrate(model interface{}) error
}

// Database is an interface which exposes a minimal amount of functions methods
// needed to atomically read and write to the database.
type Database interface {
	// View invokes the passed function in the context of a managed
	// read-only transaction.  Any errors returned from the user-supplied
	// function are returned from this function.
	//
	// Calling Rollback or Commit on the transaction passed to the
	// user-supplied function will result in a panic.
	View(fn func(tx Tx) error) error

	// Update invokes the passed function in the context of a managed
	// read-write transaction.  Any errors returned from the user-supplied
	// function will cause the transaction to be rolled back and are
	// returned from this function.  Otherwise, the transaction is committed
	// when the user-supplied function returns a nil error.
	//
	// Calling Rollback or Commit on the transaction passed to the
	// user-supplied function will result in a panic.
	Update(fn func(tx Tx) error) error

	// Close cleanly shuts down the database and syncs all data.  It will
	// block until all database transactions have been finalized (rolled
	// back or committed).
	Close() error
}
