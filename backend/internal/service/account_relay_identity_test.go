package service

import "testing"

func TestAccountIsRelayRecognizesRelayTypeWithoutMarker(t *testing.T) {
	account := &Account{Type: AccountTypeRelay}
	if !account.IsRelay() {
		t.Fatal("relay account type must identify a relay account without metadata marker")
	}
}

func TestAccountRelaySourcePriorityHidesSchedulerTranslation(t *testing.T) {
	withSnapshot := &Account{
		Type:  AccountTypeRelay,
		Extra: map[string]any{relaySourcePriorityKey: 7},
	}
	if got, ok := withSnapshot.RelaySourcePriority(); !ok || got != 7 {
		t.Fatalf("relay source priority = %d/%v, want 7/true", got, ok)
	}

	legacy := &Account{Type: AccountTypeRelay, Priority: relayNativePriorityBase}
	if got, ok := legacy.RelaySourcePriority(); !ok || got != 0 {
		t.Fatalf("legacy relay source priority = %d/%v, want 0/true", got, ok)
	}
}
