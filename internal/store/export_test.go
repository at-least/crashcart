package store

// WhereForTest exposes the dynamic WHERE of an EventFilter to tests.
func WhereForTest(f EventFilter) (string, []any) { return f.where() }
