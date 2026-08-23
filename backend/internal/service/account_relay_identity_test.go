package service

import "testing"

func TestAccountIsRelayRecognizesRelayTypeWithoutMarker(t *testing.T) {
	account := &Account{Type: AccountTypeRelay}
	if !account.IsRelay() {
		t.Fatal("relay account type must identify a relay account without metadata marker")
	}
}
