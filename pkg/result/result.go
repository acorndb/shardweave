// Package result defines the logical result stream shared by the backend and merge layers.
package result

// Field is one result-set column.
type Field struct {
	Name    string
	Table   string
	DB      string
	Type    byte
	Flags   uint16
	Charset uint16
}

// Value is one logical cell value.
type Value any

// Row is one result-set row.
type Row []Value

// Stream is a logical result stream. Next returns io.EOF when exhausted.
type Stream interface {
	Fields() []Field
	Next() (Row, error)
	Close() error
}
