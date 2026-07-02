package dbutil

import "strings"

// likeEscaper escapes the SQL LIKE metacharacters. The escape character
// itself (\) is escaped first so a literal backslash in the input is not
// silently doubled into an escape for the following character.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike escapes the SQL LIKE metacharacters %, _, and the escape
// character \ in s, for use in a LIKE pattern whose clause declares
// ESCAPE '\'. The caller composes the pattern (e.g. adding % wildcards)
// and must pair it with the ESCAPE '\' suffix on the LIKE clause.
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}
