package ssh

import (
	"fmt"
	"strings"
)

// unknownCommandMessage answers a first word that is neither a verb nor a
// slug: the error on one line, with the nearest verb when the word is a near
// miss of one, then one line naming where help lives. The banner is not
// repeated, since 80 lines of it would bury the error.
func unknownCommandMessage(word, apex string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "hostthis: unknown command %q", word)
	if verb, ok := nearestVerb(word); ok {
		fmt.Fprintf(&b, ", did you mean %q?", verb)
	}
	fmt.Fprintf(&b, "\nrun `ssh %s help` for the list of commands\n", apex)
	return b.String()
}

// nearestVerb returns the verb closest to word by edit distance (a swap of
// two neighbouring letters counts as one edit), when it is close enough to be
// a typo: one edit for a word of up to four letters, two for a longer one.
// Ties go to the alphabetically first verb, so the answer is stable.
func nearestVerb(word string) (string, bool) {
	word = strings.ToLower(word)
	limit := 2
	if len([]rune(word)) <= 4 {
		limit = 1
	}
	best, bestDist := "", limit+1
	for verb := range verbDescriptors {
		d := editDistance(word, verb)
		if d < bestDist || (d == bestDist && verb < best) {
			best, bestDist = verb, d
		}
	}
	return best, bestDist <= limit
}

// editDistance is the optimal string alignment distance between a and b, by
// rune: insertions, deletions, substitutions and adjacent transpositions.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	// rows[i][j] is the distance between ra[:i] and rb[:j].
	rows := make([][]int, len(ra)+1)
	for i := range rows {
		rows[i] = make([]int, len(rb)+1)
		rows[i][0] = i
	}
	for j := range rows[0] {
		rows[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d := min(rows[i-1][j]+1, rows[i][j-1]+1, rows[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d = min(d, rows[i-2][j-2]+1)
			}
			rows[i][j] = d
		}
	}
	return rows[len(ra)][len(rb)]
}
