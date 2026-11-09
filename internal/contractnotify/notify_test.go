package contractnotify_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/contract"
	"github.com/charlesrobsampson/rig/internal/contractnotify"
)

type sent struct{ service, text, from string }

type fakeSender struct {
	mu   sync.Mutex
	got  []sent
	fail map[string]error
}

func (f *fakeSender) Send(_ context.Context, p api.AgentMessageParams, from string) (api.AgentMessageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[p.Service]; err != nil {
		return api.AgentMessageResult{}, err
	}
	f.got = append(f.got, sent{p.Service, p.Text, from})
	return api.AgentMessageResult{Service: p.Service}, nil
}

func (f *fakeSender) to() []string {
	var out []string
	for _, s := range f.got {
		out = append(out, s.service)
	}
	sort.Strings(out)
	return out
}

func setup(t *testing.T, sender *fakeSender) *contractnotify.Notifier {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "demo"), 0o700)
	yaml := "id: demo\ntitle: Demo\nservices:\n  - name: api\n    repo: /r/api\n    branch: b\n  - name: web\n    repo: /r/web\n    branch: b\n  - name: worker\n    repo: /r/w\n    branch: b\n"
	os.WriteFile(filepath.Join(dir, "demo", "ticket.yaml"), []byte(yaml), 0o600)
	return &contractnotify.Notifier{Dir: dir, Sender: sender}
}

var prop = contract.Proposal{ID: "P-api-1", From: "api", What: "add a\nfield", Why: "y"}

func TestANewProposalGoesToEveryoneButItsAuthor(t *testing.T) {
	s := &fakeSender{}
	n := setup(t, s)
	n.NewProposal("demo", prop)
	n.Wait()
	if got := strings.Join(s.to(), ","); got != "web,worker" {
		t.Fatalf("sent to %s", got)
	}
	m := s.got[0]
	if m.from != "rig" || !strings.Contains(m.text, "P-api-1") || !strings.Contains(m.text, "add a field") || !strings.Contains(m.text, "api") || strings.Contains(m.text, "\n") {
		t.Errorf("message = %+v", m)
	}
}

func TestAnAcceptanceGoesToEveryService(t *testing.T) {
	s := &fakeSender{}
	n := setup(t, s)
	n.Resolved("demo", prop, "accepted", "")
	n.Wait()
	if got := strings.Join(s.to(), ","); got != "api,web,worker" {
		t.Fatalf("sent to %s", got)
	}
	if !strings.Contains(s.got[0].text, "accepted") {
		t.Errorf("text = %q", s.got[0].text)
	}
}

func TestARejectionGoesOnlyToTheProposerWithTheReason(t *testing.T) {
	s := &fakeSender{}
	n := setup(t, s)
	n.Resolved("demo", prop, "rejected", "too   big\nfor now")
	n.Wait()
	if len(s.got) != 1 || s.got[0].service != "api" || !strings.Contains(s.got[0].text, "rejected: too big for now.") {
		t.Errorf("sent = %+v", s.got)
	}
}

func TestADeliveryFailureDoesNotStopTheOthers(t *testing.T) {
	s := &fakeSender{fail: map[string]error{"web": errors.New("agent is waiting on a prompt")}}
	n := setup(t, s)
	n.NewProposal("demo", prop)
	n.Wait()
	if got := strings.Join(s.to(), ","); got != "worker" {
		t.Errorf("sent to %s", got)
	}
}

func TestAMissingTicketIsSurvived(t *testing.T) {
	s := &fakeSender{}
	n := setup(t, s)
	n.NewProposal("ghost", prop)
	n.Wait()
	if len(s.got) != 0 {
		t.Errorf("sent = %+v", s.got)
	}
}
