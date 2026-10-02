package intsplit

import (
	"math/big"
	"sort"
)

// LargestRemainder splits total proportionally by non-negative weights with a positive sum.
// It copies and sorts keys so equal remainders are resolved by key.
func LargestRemainder(total int64, keys []string, weight func(string) int64) map[string]int64 {
	out := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return out
	}

	sortedKeys := append([]string(nil), keys...)
	sort.Strings(sortedKeys)

	var weightTotal int64
	for _, key := range sortedKeys {
		weightTotal += weight(key)
	}

	type remainder struct {
		key   string
		value *big.Int
	}
	remainders := make([]remainder, 0, len(sortedKeys))
	var assigned int64
	divisor := big.NewInt(weightTotal)
	for _, key := range sortedKeys {
		product := new(big.Int).Mul(big.NewInt(total), big.NewInt(weight(key)))
		quotient, rem := new(big.Int), new(big.Int)
		quotient.QuoRem(product, divisor, rem)
		out[key] = quotient.Int64()
		assigned += out[key]
		remainders = append(remainders, remainder{key: key, value: rem})
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		if cmp := remainders[i].value.Cmp(remainders[j].value); cmp != 0 {
			return cmp > 0
		}
		return remainders[i].key < remainders[j].key
	})
	for i := int64(0); i < total-assigned; i++ {
		out[remainders[i%int64(len(remainders))].key]++
	}
	return out
}
