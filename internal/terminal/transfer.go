package terminal

import (
	"errors"
	"net/netip"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// Transfer identifies one interactive command and the network shown beside it.
type Transfer struct {
	number  transfernumber.Number
	network netip.Addr
}

func NewTransfer(number transfernumber.Number, network netip.Addr) (Transfer, error) {
	if number.Value() == 0 || !network.Is4() {
		return Transfer{}, errors.New("interactive transfer identity is invalid")
	}
	return Transfer{number: number, network: network}, nil
}

func (transfer Transfer) Number() transfernumber.Number { return transfer.number }
func (transfer Transfer) Network() netip.Addr           { return transfer.network }

// MouseMode selects whether Transfer Lanes scrolls its target viewport or passes declared mouse input on.
type MouseMode uint8

const (
	MouseScroll MouseMode = iota
	MouseTarget
)

func (mode MouseMode) String() string {
	if mode == MouseTarget {
		return "target"
	}
	return "scroll"
}
