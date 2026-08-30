package store

// WhereForTest exposes the dynamic WHERE of an EventFilter to tests.
func WhereForTest(f EventFilter) (string, []any) { return f.where() }

// ClipForTest / EscapeLikeForTest expose the filter-value helpers.
func ClipForTest(s string, n int) string { return clip(s, n) }
func EscapeLikeForTest(s string) string  { return escapeLike(s) }
