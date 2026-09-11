// Package transfernumber defines the stable identity of one transfer within a run.
package transfernumber

import "fmt"

// Maximum is the largest number of transfers one run may contain.
const Maximum = 35

// Number is a comparable run-local transfer identity.
type Number struct {
	value uint8
}

// New validates a one-based transfer number.
func New(value int) (Number, error) {
	if value < 1 || value > Maximum {
		return Number{}, fmt.Errorf("transfer number %d is outside valid range 1..%d", value, Maximum)
	}
	return Number{value: uint8(value)}, nil
}

// Value returns the one-based numeric representation.
func (number Number) Value() uint8 {
	return number.value
}
