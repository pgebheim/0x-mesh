package db

import (
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// dbReader is an interface that encapsulates read-only functionality.
type dbReader interface {
	leveldb.Reader
	Has(key []byte, ro *opt.ReadOptions) (ret bool, err error)
}

// dbWriter is an interface that encapsulates write/update functionality.
type dbWriter interface {
	Delete(key []byte, wo *opt.WriteOptions) error
	Put(key, value []byte, wo *opt.WriteOptions) error
}

// dbTransactor is an interface for opening transactions.
type dbTransactor interface {
	OpenTransaction() (*leveldb.Transaction, error)
}

// dbWriterTransactor combines dbWriter and dbTransactor.
type dbWriterTransactor interface {
	dbWriter
	dbTransactor
}
