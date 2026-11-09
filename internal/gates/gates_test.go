package gates_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/gates"
	"github.com/charlesrobsampson/rig/internal/state"
)

func global(gs ...domain.Gate) config.Global {
	return config.Global{Defaults: config.Defaults{Policy: domain.Policy{Gates: gs}}}
}

func svc(name string, gs []domain.Gate) domain.Service {
	return domain.Service{Name: name, Policy: domain.Policy{Gates: gs}}
}

func ticketOf(services ...domain.Service) domain.Ticket {
	return domain.Ticket{ID: "t", Services: services}
}

func approval(gate domain.Gate, service string) state.Approval {
	return state.Approval{Gate: gate, Service: service, By: "human", At: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
}

func TestGateForTransition(t *testing.T) {
	tests := []struct {
		from, to domain.Phase
		want     domain.Gate
		guarded  bool
	}{
		{domain.PhasePlan, domain.PhaseImplement, domain.GatePlanApproval, true},
		{domain.PhaseTest, domain.PhaseReview, domain.GateManualTestSignoff, true},
		{domain.PhaseReview, domain.PhasePush, domain.GateReviewApproval, true},
		{domain.PhaseImplement, domain.PhaseTest, "", false},
		{domain.PhasePush, domain.PhaseDone, "", false},
		{domain.PhaseReview, domain.PhaseImplement, "", false}, // backward moves are never gated
		{domain.PhasePush, domain.PhasePlan, "", false},
	}
	for _, tt := range tests {
		got, ok := gates.GateFor(tt.from, tt.to)
		if got != tt.want || ok != tt.guarded {
			t.Errorf("GateFor(%s, %s) = %q, %v; want %q, %v", tt.from, tt.to, got, ok, tt.want, tt.guarded)
		}
	}
}

func TestGuardedByPhaseNamesTheGateThatLeavesIt(t *testing.T) {
	g, ok := gates.GuardingExit(domain.PhasePlan)
	if !ok || g != domain.GatePlanApproval {
		t.Errorf("GuardingExit(plan) = %q, %v", g, ok)
	}
	if _, ok := gates.GuardingExit(domain.PhaseImplement); ok {
		t.Error("implement has no exit gate")
	}
	if _, ok := gates.GuardingExit(domain.PhaseDone); ok {
		t.Error("done has no exit gate")
	}
}

func TestWhichServicesRequireWhichGates(t *testing.T) {
	strict := []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval}
	tests := []struct {
		name   string
		global config.Global
		ticket domain.Ticket
		want   map[domain.Gate][]string
	}{
		{
			name:   "the strict default applies to every service",
			global: global(strict...),
			ticket: ticketOf(svc("api", nil), svc("web", nil)),
			want: map[domain.Gate][]string{
				domain.GatePlanApproval:   {"api", "web"},
				domain.GateReviewApproval: {"api", "web"},
			},
		},
		{
			name:   "an empty list on a service means that service has no gates",
			global: global(strict...),
			ticket: ticketOf(svc("api", nil), svc("greenfield", []domain.Gate{})),
			want: map[domain.Gate][]string{
				domain.GatePlanApproval:   {"api"},
				domain.GateReviewApproval: {"api"},
			},
		},
		{
			name:   "a service can add a gate the default lacks",
			global: global(domain.GatePlanApproval),
			ticket: ticketOf(svc("api", nil), svc("risky", []domain.Gate{domain.GateManualTestSignoff})),
			want: map[domain.Gate][]string{
				domain.GatePlanApproval:      {"api"},
				domain.GateManualTestSignoff: {"risky"},
			},
		},
		{
			name:   "the ticket-level list replaces the global one for services that say nothing",
			global: global(strict...),
			ticket: func() domain.Ticket {
				t := ticketOf(svc("api", nil))
				t.Policy.Gates = []domain.Gate{domain.GateManualTestSignoff}
				return t
			}(),
			want: map[domain.Gate][]string{domain.GateManualTestSignoff: {"api"}},
		},
		{
			name:   "no gates anywhere",
			global: global(),
			ticket: ticketOf(svc("api", nil)),
			want:   map[domain.Gate][]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[domain.Gate][]string{}
			for _, r := range gates.Requirements(tt.global, tt.ticket, state.State{}) {
				got[r.Gate] = append(got[r.Gate], r.Service)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("requirements = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApprovalsSatisfyRequirements(t *testing.T) {
	g := global(domain.GatePlanApproval)
	tk := ticketOf(svc("api", nil), svc("web", nil))
	tests := []struct {
		name      string
		approvals []state.Approval
		want      []string // services still missing plan_approval
	}{
		{"nothing approved", nil, []string{"api", "web"}},
		{"one service approved", []state.Approval{approval(domain.GatePlanApproval, "api")}, []string{"web"}},
		{"both services approved separately", []state.Approval{approval(domain.GatePlanApproval, "api"), approval(domain.GatePlanApproval, "web")}, nil},
		{"a blanket approval covers every service", []state.Approval{approval(domain.GatePlanApproval, "")}, nil},
		{"another gate's approval does not count", []state.Approval{approval(domain.GateReviewApproval, "")}, []string{"api", "web"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := gates.Requirements(g, tk, state.State{Approvals: tt.approvals})
			got := gates.Missing(reqs, domain.GatePlanApproval)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("missing = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequirementsCarryWhoApprovedAndWhen(t *testing.T) {
	g := global(domain.GatePlanApproval)
	tk := ticketOf(svc("api", nil))
	reqs := gates.Requirements(g, tk, state.State{Approvals: []state.Approval{approval(domain.GatePlanApproval, "")}})
	if len(reqs) != 1 || !reqs[0].Approved || reqs[0].By != "human" || reqs[0].At.IsZero() {
		t.Errorf("requirements = %+v", reqs)
	}
}

func TestBackwardMovesInvalidateLaterApprovals(t *testing.T) {
	tests := []struct {
		to   domain.Phase
		want []domain.Gate
	}{
		{domain.PhasePlan, []domain.Gate{domain.GatePlanApproval, domain.GateManualTestSignoff, domain.GateReviewApproval}},
		{domain.PhaseImplement, []domain.Gate{domain.GateManualTestSignoff, domain.GateReviewApproval}},
		{domain.PhaseTest, []domain.Gate{domain.GateManualTestSignoff, domain.GateReviewApproval}},
		{domain.PhaseReview, []domain.Gate{domain.GateReviewApproval}},
		{domain.PhasePush, nil},
		{domain.PhaseDone, nil},
	}
	for _, tt := range tests {
		got := gates.InvalidatedByMovingTo(tt.to)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("InvalidatedByMovingTo(%s) = %v, want %v", tt.to, got, tt.want)
		}
	}
}

func TestAwaitingNamesTheGateAHumanStillOwes(t *testing.T) {
	two := ticketOf(svc("a", nil), svc("b", nil))
	tests := []struct {
		name  string
		g     config.Global
		phase domain.Phase
		appr  []state.Approval
		want  domain.Gate
	}{
		{"plan, nothing approved", global(domain.GatePlanApproval), domain.PhasePlan, nil, domain.GatePlanApproval},
		{"plan, one of two approved", global(domain.GatePlanApproval), domain.PhasePlan, []state.Approval{approval(domain.GatePlanApproval, "a")}, domain.GatePlanApproval},
		{"plan, all approved", global(domain.GatePlanApproval), domain.PhasePlan, []state.Approval{approval(domain.GatePlanApproval, "")}, ""},
		{"plan, gate not required", global(), domain.PhasePlan, nil, ""},
		{"only the gate leaving the current phase counts", global(domain.GateReviewApproval), domain.PhasePlan, nil, ""},
		{"review waits on its own gate", global(domain.GatePlanApproval, domain.GateReviewApproval), domain.PhaseReview, nil, domain.GateReviewApproval},
		{"a phase with no gate", global(domain.GatePlanApproval), domain.PhaseImplement, nil, ""},
		{"done", global(domain.GatePlanApproval), domain.PhaseDone, nil, ""},
	}
	for _, tt := range tests {
		got, ok := gates.Awaiting(tt.g, two, state.State{Phase: tt.phase, Approvals: tt.appr})
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("%s: Awaiting = %q, %v; want %q", tt.name, got, ok, tt.want)
		}
	}
}
