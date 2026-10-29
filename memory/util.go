package memory

import "sort"

func sortKV[T any](x []T, less func(a, b T) bool) {
	sort.Slice(x, func(i, j int) bool { return less(x[i], x[j]) })
}
