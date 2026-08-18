package service

import "testing"

func TestBuildAccountForCreateRequiresPositivePriority(t *testing.T) {
	for _, priority := range []int{-1, 0} {
		if _, err := buildAccountForCreate(&CreateAccountInput{Priority: priority}, nil); err == nil {
			t.Fatalf("priority %d was accepted", priority)
		}
	}
	if _, err := buildAccountForCreate(&CreateAccountInput{Priority: 1}, nil); err != nil {
		t.Fatalf("positive priority was rejected: %v", err)
	}
}
