package service

import "testing"

func TestValidateNativeAccountPriorityRequiresPositivePriority(t *testing.T) {
	for _, priority := range []int{-1, 0} {
		if err := validateNativeAccountPriority(priority); err == nil {
			t.Fatalf("priority %d was accepted", priority)
		}
	}
	if err := validateNativeAccountPriority(1); err != nil {
		t.Fatalf("positive priority was rejected: %v", err)
	}
}
