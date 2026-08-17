package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestPrepareUsageLogInsert_RelayUsesNullAccountID(t *testing.T) {
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:    1,
		APIKeyID:  2,
		AccountID: 0,
		RequestID: "relay-request",
		Model:     "gpt-test",
	})
	if prepared.args[2] != nil {
		t.Fatalf("account_id argument = %#v, want nil", prepared.args[2])
	}
}
