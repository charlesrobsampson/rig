package policy_test

import (
	"reflect"
	"testing"

	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/policy"
)

func baseGlobal() config.Global {
	return config.Global{
		Defaults: config.Defaults{
			Harness:         "global-harness",
			Sandbox:         domain.SandboxContainer,
			TerminalBackend: "herdr",
			NetworkMode:     domain.NetworkBridge,
			Policy: domain.Policy{
				Gates: []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval},
				Push:  domain.PushPolicy{Mode: domain.PushPR},
			},
		},
		Auth: config.Auth{Default: "global-profile"},
	}
}

func resolve(g config.Global, t domain.Ticket, s domain.Service, f policy.Flags) policy.Effective {
	return policy.Resolve(g, t, s, f)
}

func TestResolveGates(t *testing.T) {
	plan := []domain.Gate{domain.GatePlanApproval}
	signoff := []domain.Gate{domain.GateManualTestSignoff}
	none := []domain.Gate{}
	global := []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval}

	tests := []struct {
		name                  string
		ticket, service, flag []domain.Gate
		want                  []domain.Gate
	}{
		{"only the global default", nil, nil, nil, global},
		{"ticket overrides global", plan, nil, nil, plan},
		{"service overrides ticket", plan, signoff, nil, signoff},
		{"flag overrides service", plan, signoff, plan, plan},
		{"empty service list means no gates, not inherit", plan, none, nil, none},
		{"empty ticket list overrides global when the service is silent", none, nil, nil, none},
		{"empty flag list overrides everything", plan, signoff, none, none},
		{"service falls through a silent ticket", nil, signoff, nil, signoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ticket := domain.Ticket{Policy: domain.Policy{Gates: tt.ticket}}
			svc := domain.Service{Policy: domain.Policy{Gates: tt.service}}
			got := resolve(baseGlobal(), ticket, svc, policy.Flags{Gates: tt.flag}).Gates
			if got == nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("gates = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolvePushMode(t *testing.T) {
	tests := []struct {
		name                  string
		ticket, service, flag domain.PushMode
		want                  domain.PushMode
	}{
		{"only the global default", "", "", "", domain.PushPR},
		{"ticket overrides global", domain.PushBranch, "", "", domain.PushBranch},
		{"service overrides ticket", domain.PushBranch, domain.PushMerge, "", domain.PushMerge},
		{"flag overrides service", domain.PushBranch, domain.PushMerge, domain.PushNone, domain.PushNone},
		{"service falls through a silent ticket", "", domain.PushMerge, "", domain.PushMerge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ticket := domain.Ticket{Policy: domain.Policy{Push: domain.PushPolicy{Mode: tt.ticket}}}
			svc := domain.Service{Policy: domain.Policy{Push: domain.PushPolicy{Mode: tt.service}}}
			if got := resolve(baseGlobal(), ticket, svc, policy.Flags{PushMode: tt.flag}).PushMode; got != tt.want {
				t.Errorf("push mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSandbox(t *testing.T) {
	tests := []struct {
		name                  string
		ticket, service, flag domain.Sandbox
		want                  domain.Sandbox
	}{
		{"only the global default", "", "", "", domain.SandboxContainer},
		{"ticket overrides global", domain.SandboxHost, "", "", domain.SandboxHost},
		{"service overrides ticket", domain.SandboxHost, domain.SandboxContainer, "", domain.SandboxContainer},
		{"flag overrides service", domain.SandboxContainer, domain.SandboxContainer, domain.SandboxHost, domain.SandboxHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(baseGlobal(), domain.Ticket{Sandbox: tt.ticket}, domain.Service{Sandbox: tt.service}, policy.Flags{Sandbox: tt.flag}).Sandbox
			if got != tt.want {
				t.Errorf("sandbox = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveHarness(t *testing.T) {
	tests := []struct {
		name                  string
		ticket, service, flag string
		want                  string
	}{
		{"only the global default", "", "", "", "global-harness"},
		{"ticket overrides global", "ticket-harness", "", "", "ticket-harness"},
		{"service overrides ticket", "ticket-harness", "service-harness", "", "service-harness"},
		{"flag overrides service", "ticket-harness", "service-harness", "flag-harness", "flag-harness"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(baseGlobal(), domain.Ticket{Harness: tt.ticket}, domain.Service{Harness: tt.service}, policy.Flags{Harness: tt.flag}).Harness
			if got != tt.want {
				t.Errorf("harness = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveNetworkModeIsPerTicket(t *testing.T) {
	tests := []struct {
		name         string
		ticket, flag domain.NetworkMode
		want         domain.NetworkMode
	}{
		{"only the global default", "", "", domain.NetworkBridge},
		{"ticket overrides global", domain.NetworkInternal, "", domain.NetworkInternal},
		{"flag overrides ticket", domain.NetworkInternal, domain.NetworkBridge, domain.NetworkBridge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ticket := domain.Ticket{Docker: domain.Docker{NetworkMode: tt.ticket}}
			got := resolve(baseGlobal(), ticket, domain.Service{}, policy.Flags{NetworkMode: tt.flag}).NetworkMode
			if got != tt.want {
				t.Errorf("network mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTerminalBackend(t *testing.T) {
	tests := []struct {
		name string
		flag string
		want string
	}{
		{"global default", "", "herdr"},
		{"flag overrides global", "tmux", "tmux"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(baseGlobal(), domain.Ticket{}, domain.Service{}, policy.Flags{TerminalBackend: tt.flag}).TerminalBackend
			if got != tt.want {
				t.Errorf("terminal backend = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveAuthProfileName(t *testing.T) {
	tests := []struct {
		name            string
		global          string
		ticket, service string
		want            string
	}{
		{"global default", "g", "", "", "g"},
		{"ticket overrides global", "g", "t", "", "t"},
		{"service overrides ticket", "g", "t", "s", "s"},
		{"no profile anywhere", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := baseGlobal()
			g.Auth.Default = tt.global
			got := resolve(g, domain.Ticket{Auth: tt.ticket}, domain.Service{Auth: tt.service}, policy.Flags{}).Auth
			if got != tt.want {
				t.Errorf("auth = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveGivesEachServiceItsOwnAnswer(t *testing.T) {
	ticket := domain.Ticket{Services: []domain.Service{
		{Name: "backend", Policy: domain.Policy{Gates: []domain.Gate{domain.GatePlanApproval, domain.GateReviewApproval}, Push: domain.PushPolicy{Mode: domain.PushPR}}},
		{Name: "frontend", Policy: domain.Policy{Gates: []domain.Gate{}, Push: domain.PushPolicy{Mode: domain.PushMerge}}},
	}}
	backend := resolve(baseGlobal(), ticket, ticket.Services[0], policy.Flags{})
	frontend := resolve(baseGlobal(), ticket, ticket.Services[1], policy.Flags{})
	if len(backend.Gates) != 2 || backend.PushMode != domain.PushPR {
		t.Errorf("backend = %+v", backend)
	}
	if len(frontend.Gates) != 0 || frontend.PushMode != domain.PushMerge {
		t.Errorf("frontend = %+v", frontend)
	}
}

func TestResolveReturnsCopiesOfGateLists(t *testing.T) {
	g := baseGlobal()
	got := resolve(g, domain.Ticket{}, domain.Service{}, policy.Flags{})
	got.Gates[0] = domain.GateManualTestSignoff
	if g.Defaults.Policy.Gates[0] != domain.GatePlanApproval {
		t.Error("mutating the result changed the global config")
	}
}
