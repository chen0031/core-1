package gago

import (
	"fmt"
	"math/rand"
)

// Sample k unique integers in range [min, max) using reservoir sampling,
// specifically Vitter's Algorithm R.
func randomInts(k, min, max int, rng *rand.Rand) (ints []int) {
	ints = make([]int, k)
	for i := 0; i < k; i++ {
		ints[i] = i + min
	}
	for i := k; i < max-min; i++ {
		var j = rng.Intn(i + 1)
		if j < k {
			ints[j] = i + min
		}
	}
	return
}

// Sample k unique integers from a slice of n integers without replacement.
func sampleInts(ints []int, k int, rng *rand.Rand) ([]int, []int, error) {
	if k > len(ints) {
		return nil, nil, fmt.Errorf("Cannot sample %d elements from array of length %d", k, len(ints))
	}
	var (
		sample = make([]int, k)
		idxs   = make([]int, k)
	)
	for i, idx := range randomInts(k, 0, len(ints), rng) {
		sample[i] = ints[idx]
		idxs[i] = idx
	}
	return sample, idxs, nil
}

// Generate random weights that sum up to 1.
func randomWeights(size int) []float64 {
	var (
		weights = make([]float64, size)
		total   float64
	)
	for i := range weights {
		weights[i] = rand.Float64()
		total += weights[i]
	}
	var normalized = divide(weights, total)
	return normalized
}

const (
	letterBytes   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

func randString(n int, rng *rand.Rand) string {
	b := make([]byte, n)
	for i, cache, remain := n-1, rng.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = rng.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return string(b)
}
