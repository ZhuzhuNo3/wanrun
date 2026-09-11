package runid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

const encodedLength = 32

var ErrInvalid = errors.New("invalid run ID")

// ID is the stable identity of one Transfer Lanes invocation.
type ID [encodedLength / 2]byte

// New returns a cryptographically random, filesystem-safe run identity.
func New() (ID, error) {
	for {
		var id ID
		if _, err := rand.Read(id[:]); err != nil {
			return ID{}, fmt.Errorf("generate run ID: %w", err)
		}
		if id != (ID{}) {
			return id, nil
		}
	}
}

// Parse accepts only the canonical lowercase hexadecimal directory name.
func Parse(value string) (ID, error) {
	if len(value) != encodedLength {
		return ID{}, ErrInvalid
	}
	for _, character := range value {
		if !isLowerHex(character) {
			return ID{}, ErrInvalid
		}
	}

	var id ID
	if _, err := hex.Decode(id[:], []byte(value)); err != nil || id == (ID{}) {
		return ID{}, ErrInvalid
	}
	return id, nil
}

func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

func isLowerHex(character rune) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f'
}
