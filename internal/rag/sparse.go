package rag

import (
	"hash/fnv"
	"sort"
	"strings"
	"unicode"
)

// shortcut: uniform term weights without corpus df/idf; upgrade to stored df/idf when lexical quality matters.
func sparseVector(text string) SparseVector {
	seen := make(map[uint32]struct{})
	count := 0
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(token)) < 2 {
			continue
		}
		if count == 512 {
			break
		}
		count++
		h := fnv.New32a()
		_, _ = h.Write([]byte(token))
		seen[h.Sum32()] = struct{}{}
	}
	indices := make([]uint32, 0, len(seen))
	for index := range seen {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	values := make([]float32, len(indices))
	for i := range values {
		values[i] = 1
	}
	return SparseVector{Indices: indices, Values: values}
}
