package onecollector

import (
	"errors"
	"math"
	"math/big"
	"sort"
)

// Segment represents Count replayed histogram observations of Value.
type Segment struct {
	Value float64
	Count uint64
}
type group struct {
	count         uint64
	lower, upper  float64
	low, high     *big.Int
	first, second Segment
	actual        *big.Int
}

func add(a, b *big.Int) *big.Int { return new(big.Int).Add(a, b) }
func sub(a, b *big.Int) *big.Int { return new(big.Int).Sub(a, b) }
func mul(a, b *big.Int) *big.Int { return new(big.Int).Mul(a, b) }
func abs(a *big.Int) *big.Int    { return new(big.Int).Abs(a) }
func integer(n uint64) *big.Int  { return new(big.Int).SetUint64(n) }
func clamp(n, lo, hi *big.Int) *big.Int {
	if n.Cmp(lo) < 0 {
		return new(big.Int).Set(lo)
	}
	if n.Cmp(hi) > 0 {
		return new(big.Int).Set(hi)
	}
	return new(big.Int).Set(n)
}
func units(v float64) *big.Int {
	bits := math.Float64bits(v)
	exponent := (bits >> 52) & 0x7ff
	n := integer(bits & 0x000fffffffffffff)
	if exponent != 0 {
		n.Add(n, new(big.Int).Lsh(big.NewInt(1), 52))
		n.Lsh(n, uint(exponent-1))
	}
	if bits>>63 != 0 {
		n.Neg(n)
	}
	return n
}
func floorDouble(n *big.Int) float64 {
	limit := units(math.MaxFloat64)
	if n.Cmp(limit) > 0 {
		return math.MaxFloat64
	}
	if n.Cmp(new(big.Int).Neg(limit)) < 0 {
		return math.Inf(-1)
	}
	magnitude := abs(n)
	shift := magnitude.BitLen() - 53
	if shift < 0 {
		shift = 0
	}
	significand := new(big.Int).Rsh(magnitude, uint(shift))
	if n.Sign() < 0 && new(big.Int).Lsh(new(big.Int).Set(significand), uint(shift)).Cmp(magnitude) != 0 {
		significand.Add(significand, big.NewInt(1))
	}
	value := math.Ldexp(float64(significand.Uint64()), shift-1074)
	if n.Sign() < 0 {
		return -value
	}
	return value
}
func tolerance(sum float64) *big.Int {
	magnitude := math.Abs(sum)
	next := math.Nextafter(magnitude, math.Inf(1))
	if math.IsInf(next, 0) {
		return sub(units(magnitude), units(math.Nextafter(magnitude, math.Inf(-1))))
	}
	return sub(units(next), units(magnitude))
}
func mix(g *group, desired *big.Int) {
	count := integer(g.count)
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(desired, count, r)
	if r.Sign() < 0 {
		q.Sub(q, big.NewInt(1))
	}
	first := floorDouble(q)
	firstUnits := units(first)
	distance := sub(desired, mul(firstUnits, count))
	if distance.Sign() == 0 {
		g.first, g.second, g.actual = Segment{first, g.count}, Segment{}, new(big.Int).Set(desired)
		return
	}
	second := math.Nextafter(first, math.Inf(1))
	step := sub(units(second), firstUnits)
	high, rounding := new(big.Int), new(big.Int)
	high.QuoRem(distance, step, rounding)
	comparison := new(big.Int).Lsh(rounding, 1).Cmp(step)
	if comparison > 0 || comparison == 0 && high.Bit(0) != 0 {
		high.Add(high, big.NewInt(1))
	}
	highCount := high.Uint64()
	g.first, g.second = Segment{first, g.count - highCount}, Segment{second, highCount}
	g.actual = add(mul(firstUnits, count), mul(step, high))
}

// Replay retains O(bucket count) segments, not O(observation count). Sum fitting
// uses exact binary64 units and the historical one-ULP tolerance.
func Replay(count uint64, counts []uint64, bounds []float64, sum, min, max *float64, nonNegative bool) ([]Segment, error) {
	fail := func(reason string) ([]Segment, error) { return nil, errors.New("onecollector: histogram_" + reason) }
	if err := validateHistogramBuckets(count, counts, bounds); err != nil {
		return nil, err
	}
	if err := validateHistogramStatistics(count, sum, min, max, nonNegative); err != nil {
		return nil, err
	}
	if count == 0 {
		return []Segment{}, nil
	}
	counts = append([]uint64(nil), counts...)
	result := []Segment{}
	reserve := func(v float64) bool {
		i := sort.SearchFloat64s(bounds, v)
		if counts[i] == 0 {
			return false
		}
		counts[i]--
		result = append(result, Segment{v, 1})
		return true
	}
	if min != nil && !reserve(*min) {
		return fail("extremum_bucket_mismatch")
	}
	if max != nil && (min == nil || *max != *min) && !reserve(*max) {
		return fail("extremum_bucket_mismatch")
	}
	groups, err := histogramGroups(counts, bounds, min, max, nonNegative)
	if err != nil {
		return nil, err
	}
	if err := fitHistogram(groups, result, sum); err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.first.Count != 0 {
			result = append(result, g.first)
		}
		if g.second.Count != 0 {
			result = append(result, g.second)
		}
	}
	return result, nil
}

func validateHistogramBuckets(count uint64, counts []uint64, bounds []float64) error {
	if len(counts) != len(bounds)+1 {
		return errors.New("onecollector: histogram_bucket_length_mismatch")
	}
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) || i > 0 && b <= bounds[i-1] {
			return errors.New("onecollector: histogram_invalid_bounds")
		}
	}
	total := new(big.Int)
	for _, n := range counts {
		total.Add(total, integer(n))
	}
	if total.Cmp(integer(count)) != 0 {
		return errors.New("onecollector: histogram_count_mismatch")
	}
	return nil
}

func validateHistogramStatistics(count uint64, sum, min, max *float64, nonNegative bool) error {
	for _, v := range []*float64{sum, min, max} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return errors.New("onecollector: histogram_nonfinite_statistic")
		}
	}
	if count == 0 {
		if min != nil || max != nil || sum != nil && *sum != 0 {
			return errors.New("onecollector: histogram_invalid_empty_statistics")
		}
		return nil
	}
	if min != nil && max != nil && *min > *max {
		return errors.New("onecollector: histogram_invalid_extrema_or_domain")
	}
	if nonNegative {
		for _, v := range []*float64{sum, min, max} {
			if v != nil && *v < 0 {
				return errors.New("onecollector: histogram_invalid_extrema_or_domain")
			}
		}
	}
	return nil
}

func histogramGroups(counts []uint64, bounds []float64, min, max *float64, nonNegative bool) ([]*group, error) {
	groups := []*group{}
	for i, n := range counts {
		if n == 0 {
			continue
		}
		lo, hi := math.Inf(-1), math.Inf(1)
		if i > 0 {
			lo = math.Nextafter(bounds[i-1], math.Inf(1))
		}
		if i < len(bounds) {
			hi = bounds[i]
		}
		if nonNegative {
			lo = math.Max(lo, 0)
		}
		if min != nil {
			lo = math.Max(lo, *min)
		}
		if max != nil {
			hi = math.Min(hi, *max)
		}
		if lo > hi || math.IsInf(lo, 1) || math.IsInf(hi, -1) {
			return nil, errors.New("onecollector: histogram_empty_bucket_range")
		}
		groups = append(groups, &group{count: n, lower: lo, upper: hi})
	}
	return groups, nil
}

func fitHistogram(groups []*group, result []Segment, sum *float64) error {
	fixed := new(big.Int)
	for _, s := range result {
		fixed.Add(fixed, units(s.Value))
	}
	target, tol := new(big.Int), new(big.Int)
	if sum != nil {
		target, tol = units(*sum), tolerance(*sum)
	}
	if sum != nil {
		tightenHistogramBounds(groups, fixed, target, tol)
	}
	lowerSum, upperSum := new(big.Int).Set(fixed), new(big.Int).Set(fixed)
	for _, g := range groups {
		if math.IsInf(g.lower, 0) || math.IsInf(g.upper, 0) {
			return errors.New("onecollector: histogram_missing_finite_bound")
		}
		if g.lower > g.upper {
			return errors.New("onecollector: histogram_infeasible_sum")
		}
		g.low, g.high = units(g.lower), units(g.upper)
		lowerSum.Add(lowerSum, mul(integer(g.count), g.low))
		upperSum.Add(upperSum, mul(integer(g.count), g.high))
	}
	if sum == nil {
		for _, g := range groups {
			mix(g, mul(integer(g.count), g.low))
		}
		return nil
	}
	return distributeHistogramSum(groups, fixed, target, tol, lowerSum, upperSum)
}

func tightenHistogramBounds(groups []*group, fixed, target, tol *big.Int) {
	if len(groups) == 1 && groups[0].count == 1 {
		g := groups[0]
		if math.IsInf(g.lower, 0) {
			g.lower = -floorDouble(new(big.Int).Neg(sub(sub(target, tol), fixed)))
		}
		if math.IsInf(g.upper, 0) {
			g.upper = floorDouble(sub(add(target, tol), fixed))
		}
	}
	allLower, allUpper := true, true
	for _, g := range groups {
		allLower = allLower && !math.IsInf(g.lower, 0)
		allUpper = allUpper && !math.IsInf(g.upper, 0)
	}
	loTotal, hiTotal := new(big.Int).Set(fixed), new(big.Int).Set(fixed)
	for _, g := range groups {
		if allLower {
			loTotal.Add(loTotal, mul(integer(g.count), units(g.lower)))
		}
		if allUpper {
			hiTotal.Add(hiTotal, mul(integer(g.count), units(g.upper)))
		}
	}
	for _, g := range groups {
		if math.IsInf(g.upper, 0) && allLower {
			g.upper = floorDouble(add(sub(add(target, tol), loTotal), units(g.lower)))
		}
		if math.IsInf(g.lower, 0) && allUpper {
			g.lower = -floorDouble(new(big.Int).Neg(add(sub(sub(target, tol), hiTotal), units(g.upper))))
		}
	}
}

func distributeHistogramSum(groups []*group, fixed, target, tol, lowerSum, upperSum *big.Int) error {
	if target.Cmp(sub(lowerSum, tol)) < 0 || target.Cmp(add(upperSum, tol)) > 0 {
		return errors.New("onecollector: histogram_infeasible_sum")
	}
	increase := sub(clamp(target, lowerSum, upperSum), lowerSum)
	ordered := append([]*group(nil), groups...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return sub(ordered[i].high, ordered[i].low).Cmp(sub(ordered[j].high, ordered[j].low)) < 0
	})
	weight := new(big.Int)
	for _, g := range groups {
		weight.Add(weight, integer(g.count))
	}
	numerator := new(big.Int).Set(increase)
	for _, g := range ordered {
		width := sub(g.high, g.low)
		if numerator.Cmp(mul(width, weight)) <= 0 {
			break
		}
		numerator.Sub(numerator, mul(width, integer(g.count)))
		weight.Sub(weight, integer(g.count))
	}
	for _, g := range groups {
		addition := new(big.Int)
		if weight.Sign() != 0 {
			addition.Quo(mul(numerator, integer(g.count)), weight)
			maximum := mul(sub(g.high, g.low), integer(g.count))
			if addition.Cmp(maximum) > 0 {
				addition.Set(maximum)
			}
		}
		mix(g, add(mul(g.low, integer(g.count)), addition))
	}
	correct := func() *big.Int {
		actual := new(big.Int).Set(fixed)
		for _, g := range groups {
			actual.Add(actual, g.actual)
		}
		for _, g := range groups {
			residual := sub(target, actual)
			if residual.Sign() == 0 {
				break
			}
			old := *g
			mix(g, clamp(add(g.actual, residual), mul(integer(g.count), g.low), mul(integer(g.count), g.high)))
			change := sub(g.actual, old.actual)
			if abs(sub(residual, change)).Cmp(abs(residual)) < 0 {
				actual.Add(actual, change)
			} else {
				*g = old
			}
		}
		return actual
	}
	actual := correct()
	if abs(sub(actual, target)).Cmp(tol) > 0 {
		for _, g := range groups {
			mix(g, mul(integer(g.count), clamp(new(big.Int), g.low, g.high)))
		}
		actual = correct()
	}
	if abs(sub(actual, target)).Cmp(tol) > 0 {
		return errors.New("onecollector: histogram_compact_fit_failed")
	}
	return nil
}
