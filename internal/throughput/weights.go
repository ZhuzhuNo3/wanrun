package throughput

import (
	"errors"
	"fmt"
	"math/big"
)

const normalizedScale = uint64(1_000_000)

// RelativeWeights derives exact positive ratios. When successfulOnly is true,
// failed targets are omitted; otherwise any failed target rejects the group.
func RelativeWeights(results []Observation, successfulOnly bool) ([]Weight, error) {
	seen := make(map[Target]struct{}, len(results))
	valid := make([]Observation, 0, len(results))
	for _, result := range results {
		if !result.target.valid() {
			return nil, errors.New("throughput observation identity is invalid")
		}
		if _, duplicate := seen[result.target]; duplicate {
			return nil, errors.New("throughput observation identity is repeated")
		}
		seen[result.target] = struct{}{}
		if result.err != nil {
			if successfulOnly {
				continue
			}
			return nil, result.err
		}
		valid = append(valid, result)
	}
	if len(valid) < 2 {
		if successfulOnly {
			return nil, nil
		}
		return nil, errors.New("relative weights require at least two successful observations")
	}
	duration := valid[0].duration
	for _, result := range valid {
		if result.bytes == 0 || result.duration != duration || duration <= 0 {
			return nil, errors.New("throughput observations are not comparable")
		}
	}
	slowest := valid[0].bytes
	for _, result := range valid[1:] {
		if result.bytes < slowest {
			slowest = result.bytes
		}
	}
	scaled := make([]*big.Int, len(valid))
	for index, result := range valid {
		numerator := new(big.Int).Mul(new(big.Int).SetUint64(result.bytes), new(big.Int).SetUint64(normalizedScale))
		value, remainder := new(big.Int), new(big.Int)
		value.QuoRem(numerator, new(big.Int).SetUint64(slowest), remainder)
		if new(big.Int).Lsh(remainder, 1).Cmp(new(big.Int).SetUint64(slowest)) >= 0 {
			value.Add(value, big.NewInt(1))
		}
		if !value.IsUint64() || value.Sign() <= 0 {
			return nil, fmt.Errorf("throughput weight %d is outside supported range", index)
		}
		scaled[index] = value
	}
	divisor := new(big.Int).Set(scaled[0])
	for _, value := range scaled[1:] {
		divisor.GCD(nil, nil, divisor, value)
	}
	weights := make([]Weight, len(valid))
	for index, value := range scaled {
		weights[index] = Weight{target: valid[index].target, value: new(big.Int).Quo(value, divisor).Uint64()}
	}
	return weights, nil
}
