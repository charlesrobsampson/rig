package domain_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
)

type enum interface {
	~string
	Valid() bool
}

func checkEnum[T enum](t *testing.T, valid []T) {
	t.Helper()
	for _, v := range valid {
		if !v.Valid() {
			t.Errorf("%q reports invalid", v)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %q: %v", v, err)
		}
		if want := strconv.Quote(string(v)); string(b) != want {
			t.Errorf("marshal %q = %s, want %s", v, b, want)
		}
		var got T
		if err := json.Unmarshal(b, &got); err != nil || got != v {
			t.Errorf("round trip %q = %q, %v", v, got, err)
		}
	}
	for _, bad := range []string{"", "bogus", " " + string(valid[0]), "UPPER"} {
		var got T
		if err := json.Unmarshal([]byte(strconv.Quote(bad)), &got); err == nil {
			t.Errorf("unmarshal %q succeeded as %q", bad, got)
		}
	}
	var zero T
	if zero.Valid() {
		t.Error("zero value reports valid")
	}
	if _, err := json.Marshal(zero); err == nil {
		t.Error("marshal of the zero value succeeded")
	}
}

func TestEnumsRoundTripAndRejectUnknownValues(t *testing.T) {
	t.Run("Phase", func(t *testing.T) {
		checkEnum(t, []domain.Phase{domain.PhasePlan, domain.PhaseImplement, domain.PhaseTest, domain.PhaseReview, domain.PhasePush, domain.PhaseDone})
	})
	t.Run("ServiceStatus", func(t *testing.T) {
		checkEnum(t, []domain.ServiceStatus{domain.ServicePending, domain.ServiceProvisioned, domain.ServiceRunning, domain.ServiceStopped, domain.ServiceError})
	})
	t.Run("AgentState", func(t *testing.T) {
		checkEnum(t, []domain.AgentState{domain.AgentWorking, domain.AgentBlocked, domain.AgentIdle, domain.AgentUnknown, domain.AgentExited})
	})
	t.Run("PushStatus", func(t *testing.T) {
		checkEnum(t, []domain.PushStatus{domain.PushPending, domain.PushDone, domain.PushFailed})
	})
	t.Run("PushMode", func(t *testing.T) {
		checkEnum(t, []domain.PushMode{domain.PushNone, domain.PushBranch, domain.PushPR, domain.PushMerge})
	})
	t.Run("Sandbox", func(t *testing.T) {
		checkEnum(t, []domain.Sandbox{domain.SandboxContainer, domain.SandboxHost})
	})
	t.Run("NetworkMode", func(t *testing.T) {
		checkEnum(t, []domain.NetworkMode{domain.NetworkBridge, domain.NetworkInternal})
	})
	t.Run("AuthType", func(t *testing.T) {
		checkEnum(t, []domain.AuthType{domain.AuthHostLogin, domain.AuthOAuthToken, domain.AuthAPIKey, domain.AuthAuthToken, domain.AuthBedrock, domain.AuthVertex, domain.AuthFoundry})
	})
	t.Run("CheckoutKind", func(t *testing.T) {
		checkEnum(t, []domain.CheckoutKind{domain.CheckoutClone, domain.CheckoutWorktree})
	})
	t.Run("Gate", func(t *testing.T) {
		checkEnum(t, []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval, domain.GateManualTestSignoff})
	})
}

func TestEnumStringFormsMatchTheFormatsSpec(t *testing.T) {
	tests := []struct {
		got  string
		want string
	}{
		{string(domain.PushBranch), "push-branch"},
		{string(domain.PushPR), "pr"},
		{string(domain.GatePlanApproval), "plan_approval"},
		{string(domain.GateManualTestSignoff), "manual_test_signoff"},
		{string(domain.SandboxContainer), "container"},
		{string(domain.NetworkInternal), "internal"},
		{string(domain.AgentExited), "exited"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("string form %q, want %q", tt.got, tt.want)
		}
	}
}
