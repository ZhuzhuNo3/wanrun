package transfernumber

import "testing"

func TestNumberAcceptsOnlyRunTransferRange(t *testing.T) {
	for _, value := range []int{1, Maximum} {
		number, err := New(value)
		if err != nil {
			t.Fatalf("New(%d): %v", value, err)
		}
		if number.Value() != uint8(value) {
			t.Fatalf("New(%d).Value() = %d", value, number.Value())
		}
	}
	for _, value := range []int{0, Maximum + 1} {
		if _, err := New(value); err == nil {
			t.Errorf("New(%d) succeeded", value)
		}
	}
}

func TestNumberIsComparable(t *testing.T) {
	number, err := New(7)
	if err != nil {
		t.Fatal(err)
	}
	values := map[Number]string{number: "transfer seven"}
	if values[number] != "transfer seven" {
		t.Fatal("number cannot be used as an identity key")
	}
}
