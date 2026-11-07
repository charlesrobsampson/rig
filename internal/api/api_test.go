package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/domain"
)

func encode(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := api.NewEncoder(&buf).Write(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestWireFormatMatchesTheFormatsSpec(t *testing.T) {
	boom := &api.Error{Code: api.CodeNotFound, Message: "no such ticket"}
	blocked := &api.Error{Code: api.CodeBlocked, Message: "gate not approved", Details: []string{"plan_approval"}}
	success, err := api.Success("7", map[string]int{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	item, err := api.StreamItem("9", map[string]string{"line": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		msg  any
		want string
	}{
		{"request with params", api.Request{V: 1, ID: "1", Method: api.MethodTicketList, Params: json.RawMessage(`{}`)}, `{"v":1,"id":"1","method":"ticket.list","params":{}}` + "\n"},
		{"request without params", api.Request{V: 1, ID: "2", Method: api.MethodStatus}, `{"v":1,"id":"2","method":"status"}` + "\n"},
		{"success", success, `{"v":1,"id":"7","ok":true,"result":{"n":1}}` + "\n"},
		{"failure", api.Failure("3", boom), `{"v":1,"id":"3","ok":false,"error":{"code":"not_found","message":"no such ticket"}}` + "\n"},
		{"failure with details", api.Failure("4", blocked), `{"v":1,"id":"4","ok":false,"error":{"code":"blocked","message":"gate not approved","details":["plan_approval"]}}` + "\n"},
		{"stream item", item, `{"v":1,"id":"9","stream":{"line":"hi"}}` + "\n"},
		{"end of stream", api.EndOfStream("9"), `{"v":1,"id":"9","ok":true,"end":true}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encode(t, tt.msg); got != tt.want {
				t.Errorf("wire = %s\nwant   %s", got, tt.want)
			}
		})
	}
}

func TestErrorCodesSerializeExactlyAsSpecified(t *testing.T) {
	want := map[api.ErrorCode]string{
		api.CodeInvalid:     "invalid",
		api.CodeNotFound:    "not_found",
		api.CodeConflict:    "conflict",
		api.CodeBlocked:     "blocked",
		api.CodeUnavailable: "unavailable",
		api.CodeNotRunning:  "not_running",
		api.CodeInternal:    "internal",
	}
	for code, text := range want {
		if string(code) != text {
			t.Errorf("code %q, want %q", code, text)
		}
	}
	if len(want) != 7 {
		t.Fatal("update this table when a code is added")
	}
}

func TestMessagesRoundTripThroughTheCodec(t *testing.T) {
	var buf bytes.Buffer
	enc := api.NewEncoder(&buf)
	reqs := []api.Request{
		{V: 1, ID: "a", Method: api.MethodHello, Params: json.RawMessage(`{"client":"rig-cli","version":"x"}`)},
		{V: 1, ID: "b", Method: api.MethodTicketGet, Params: json.RawMessage(`{"id":"ticket-1"}`)},
		{V: 1, ID: "c", Method: api.MethodDaemonStop},
	}
	for _, r := range reqs {
		if err := enc.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	dec := api.NewDecoder(&buf)
	for i, want := range reqs {
		got, err := dec.ReadRequest()
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if got.ID != want.ID || got.Method != want.Method || string(got.Params) != string(want.Params) || got.V != 1 {
			t.Errorf("request %d = %+v, want %+v", i, got, want)
		}
	}
	if _, err := dec.ReadRequest(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last message: %v, want io.EOF", err)
	}
}

func TestResponsesDecodeIntoTheirKinds(t *testing.T) {
	ok, _ := api.Success("1", map[string]string{"k": "v"})
	item, _ := api.StreamItem("2", map[string]int{"n": 1})
	var buf bytes.Buffer
	enc := api.NewEncoder(&buf)
	for _, m := range []api.Response{ok, api.Failure("3", &api.Error{Code: api.CodeInternal, Message: "x"}), item, api.EndOfStream("2")} {
		if err := enc.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	dec := api.NewDecoder(&buf)
	kinds := []struct {
		name        string
		stream, end bool
		failed      bool
	}{{"success", false, false, false}, {"failure", false, false, true}, {"stream item", true, false, false}, {"end", false, true, false}}
	for _, k := range kinds {
		r, err := dec.ReadResponse()
		if err != nil {
			t.Fatalf("%s: %v", k.name, err)
		}
		if r.IsStream() != k.stream || r.IsEnd() != k.end || (r.Err() != nil) != k.failed {
			t.Errorf("%s: stream=%v end=%v err=%v", k.name, r.IsStream(), r.IsEnd(), r.Err())
		}
	}
}

func TestResponseErrIsTheStructuredError(t *testing.T) {
	r := api.Failure("1", &api.Error{Code: api.CodeBlocked, Message: "nope", Details: []string{"a", "b"}})
	var apiErr *api.Error
	if !errors.As(r.Err(), &apiErr) || apiErr.Code != api.CodeBlocked || !reflect.DeepEqual(apiErr.Details, []string{"a", "b"}) {
		t.Fatalf("Err() = %v", r.Err())
	}
	if !strings.Contains(r.Err().Error(), "blocked") || !strings.Contains(r.Err().Error(), "nope") {
		t.Errorf("error text %q should include the code and message", r.Err())
	}
	if ok, _ := api.Success("1", nil); ok.Err() != nil {
		t.Error("a success has no error")
	}
}

func TestValidateRejectsMalformedRequests(t *testing.T) {
	tests := []struct {
		name string
		req  api.Request
		want string // substring of the message; empty means valid
	}{
		{"valid", api.Request{V: 1, ID: "1", Method: api.MethodStatus}, ""},
		{"every documented method is accepted", api.Request{V: 1, ID: "1", Method: api.MethodEventsSubscribe}, ""},
		{"missing version", api.Request{ID: "1", Method: api.MethodStatus}, "version"},
		{"newer protocol major", api.Request{V: 2, ID: "1", Method: api.MethodStatus}, "version 2"},
		{"missing id", api.Request{V: 1, Method: api.MethodStatus}, "id"},
		{"missing method", api.Request{V: 1, ID: "1"}, "method"},
		{"unknown method", api.Request{V: 1, ID: "1", Method: "ticket.explode"}, "ticket.explode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Code != api.CodeInvalid || !strings.Contains(err.Message, tt.want) {
				t.Fatalf("Validate = %+v, want an invalid error mentioning %q", err, tt.want)
			}
		})
	}
}

func TestEveryDocumentedMethodIsKnownAndOnlyStreamingOnesStream(t *testing.T) {
	documented := []string{
		"hello", "status", "config.get", "ticket.list", "ticket.get", "ticket.rescan", "ticket.new",
		"ticket.start", "ticket.stop", "gate.list", "gate.approve", "phase.set", "transition.request",
		"agent.attach", "agent.message", "agent.event", "contract.list", "contract.accept",
		"contract.reject", "review.diff", "push.run", "tests.render", "logs.stream", "events.subscribe",
		"reconcile.run", "daemon.stop",
	}
	for _, m := range documented {
		if !api.KnownMethod(m) {
			t.Errorf("%s is documented but unknown", m)
		}
		if want := m == "events.subscribe" || m == "logs.stream"; api.IsStreaming(m) != want {
			t.Errorf("IsStreaming(%s) = %v, want %v", m, api.IsStreaming(m), want)
		}
	}
	if api.KnownMethod("nope") {
		t.Error("an undocumented method was accepted")
	}
}

func TestOversizedLinesAreRejectedWithoutPoisoningTheStream(t *testing.T) {
	var buf bytes.Buffer
	huge := `{"v":1,"id":"x","method":"status","params":"` + strings.Repeat("a", api.MaxLineBytes) + `"}` + "\n"
	buf.WriteString(huge)
	if err := api.NewEncoder(&buf).Write(api.Request{V: 1, ID: "after", Method: api.MethodStatus}); err != nil {
		t.Fatal(err)
	}
	dec := api.NewDecoder(&buf)
	if _, err := dec.ReadRequest(); !errors.Is(err, api.ErrLineTooLong) {
		t.Fatalf("oversized line: %v, want ErrLineTooLong", err)
	}
	got, err := dec.ReadRequest()
	if err != nil || got.ID != "after" {
		t.Fatalf("the message after an oversized one = %+v, %v", got, err)
	}
}

func TestEncoderRefusesToWriteAMessageItsPeerWouldReject(t *testing.T) {
	big := api.Request{V: 1, ID: "1", Method: api.MethodStatus, Params: json.RawMessage(`"` + strings.Repeat("a", api.MaxLineBytes) + `"`)}
	var buf bytes.Buffer
	if err := api.NewEncoder(&buf).Write(big); !errors.Is(err, api.ErrLineTooLong) {
		t.Fatalf("error = %v, want ErrLineTooLong", err)
	}
	if buf.Len() != 0 {
		t.Error("a partial message was written")
	}
}

func TestDecoderErrorsOnBrokenInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"no trailing newline", `{"v":1,"id":"1","method":"status"}`, io.ErrUnexpectedEOF},
		{"empty input", ``, io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := api.NewDecoder(strings.NewReader(tt.input)).ReadRequest(); !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
	if _, err := api.NewDecoder(strings.NewReader("{not json}\n")).ReadRequest(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("malformed JSON error = %v", err)
	}
}

func TestHandshake(t *testing.T) {
	if got := encode(t, api.HelloParams{Client: "rig-cli", Version: "dev"}); got != `{"client":"rig-cli","version":"dev"}`+"\n" {
		t.Errorf("hello params wire = %s", got)
	}
	result := api.HelloResult{Protocol: api.ProtocolVersion, Server: "rigd 0.1"}
	if got := encode(t, result); got != `{"protocol":1,"server":"rigd 0.1"}`+"\n" {
		t.Errorf("hello result wire = %s", got)
	}
	tests := []struct {
		name    string
		server  int
		wantErr bool
	}{{"same major", 1, false}, {"server is newer", 2, true}, {"server is older", 0, true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := api.HelloResult{Protocol: tt.server}.Compatible()
			if (err != nil) != tt.wantErr {
				t.Errorf("Compatible() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEventsRoundTripWithTypedData(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ev, err := api.NewEvent(api.EventPhaseChanged, "ticket-412", "", at, api.PhaseChangedData{From: domain.PhasePlan, To: domain.PhaseImplement, By: "human"})
	if err != nil {
		t.Fatal(err)
	}
	wire := encode(t, ev)
	want := `{"type":"phase.changed","ticket":"ticket-412","service":"","at":"2026-11-06T12:00:00Z","data":{"from":"plan","to":"implement","by":"human","reason":""}}` + "\n"
	if wire != want {
		t.Fatalf("event wire = %s\nwant       %s", wire, want)
	}
	var back api.Event
	if err := json.Unmarshal([]byte(wire), &back); err != nil {
		t.Fatal(err)
	}
	var data api.PhaseChangedData
	if err := back.DecodeData(&data); err != nil || data.To != domain.PhaseImplement || back.Type != api.EventPhaseChanged {
		t.Fatalf("decoded = %+v, %+v, %v", back, data, err)
	}
}

func TestEveryEventTypeHasTheSpecifiedName(t *testing.T) {
	want := map[api.EventType]string{
		api.EventTicketCreated:     "ticket.created",
		api.EventTicketStarted:     "ticket.started",
		api.EventTicketStopped:     "ticket.stopped",
		api.EventServiceStatus:     "service.status",
		api.EventPhaseChanged:      "phase.changed",
		api.EventGateApproved:      "gate.approved",
		api.EventAgentState:        "agent.state",
		api.EventContractPending:   "contract.pending",
		api.EventContractViolation: "contract.violation",
		api.EventContractResolved:  "contract.resolved",
		api.EventEgressDenied:      "egress.denied",
		api.EventError:             "error",
		api.EventSubscribed:        "subscribed",
	}
	for typ, name := range want {
		if string(typ) != name {
			t.Errorf("event type %q, want %q", typ, name)
		}
	}
}

func TestEventDataWireShapes(t *testing.T) {
	tests := []struct {
		name string
		data any
		want string
	}{
		{"service status", api.ServiceStatusData{Status: domain.ServiceRunning}, `{"status":"running"}`},
		{"gate approved", api.GateApprovedData{Gate: domain.GatePlanApproval, Service: "api"}, `{"gate":"plan_approval","service":"api"}`},
		{"agent state", api.AgentStateData{State: domain.AgentBlocked, Previous: domain.AgentWorking}, `{"state":"blocked","previous":"working"}`},
		{"contract pending", api.ContractPendingData{Proposal: "P-backend-1"}, `{"proposal":"P-backend-1"}`},
		{"contract violation", api.ContractViolationData{Detail: "edited agreed"}, `{"detail":"edited agreed"}`},
		{"contract resolved", api.ContractResolvedData{Proposal: "P-backend-1", Outcome: "accepted"}, `{"proposal":"P-backend-1","outcome":"accepted"}`},
		{"egress denied", api.EgressDeniedData{Host: "example.com", Port: 443, Reason: "not in allowlist"}, `{"host":"example.com","port":443,"reason":"not in allowlist"}`},
		{"error", api.ErrorData{Message: "boom"}, `{"message":"boom"}`},
		{"empty data", api.EmptyData{}, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.data)
			if err != nil || string(raw) != tt.want {
				t.Errorf("wire = %s, %v; want %s", raw, err, tt.want)
			}
		})
	}
}

func TestUnknownEventTypesPassThroughForForwardCompatibility(t *testing.T) {
	var ev api.Event
	if err := json.Unmarshal([]byte(`{"type":"future.thing","ticket":"t","service":"","at":"2026-11-06T12:00:00Z","data":{"x":1}}`), &ev); err != nil {
		t.Fatalf("an unknown event type must still decode: %v", err)
	}
	if ev.Type != "future.thing" {
		t.Errorf("type = %q", ev.Type)
	}
}

func TestEventsTravelAsStreamItems(t *testing.T) {
	ev, _ := api.NewEvent(api.EventTicketStarted, "t", "", time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), api.EmptyData{})
	item, err := api.StreamItem("sub-1", ev)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := api.NewEncoder(&buf).Write(item); err != nil {
		t.Fatal(err)
	}
	r, err := api.NewDecoder(&buf).ReadResponse()
	if err != nil || !r.IsStream() {
		t.Fatalf("response = %+v, %v", r, err)
	}
	var back api.Event
	if err := json.Unmarshal(r.Stream, &back); err != nil || back.Type != api.EventTicketStarted {
		t.Errorf("stream payload = %+v, %v", back, err)
	}
}

func TestTicketPayloadsHandleBrokenTickets(t *testing.T) {
	broken := api.TicketSummary{ID: "ticket-9", Title: "x", Error: "ticket.yaml: bad", Services: []api.ServiceSummary{}}
	raw, err := json.Marshal(api.TicketListResult{Tickets: []api.TicketSummary{broken}})
	if err != nil {
		t.Fatalf("a broken ticket (no phase, no service states) must still serialize: %v", err)
	}
	if !strings.Contains(string(raw), `"error":"ticket.yaml: bad"`) || strings.Contains(string(raw), `"phase"`) {
		t.Errorf("wire = %s", raw)
	}
	healthy := api.TicketSummary{ID: "t", Phase: domain.PhaseImplement, Services: []api.ServiceSummary{{Name: "api", Status: domain.ServiceRunning, AgentState: domain.AgentIdle, HostPort: 41000}}}
	raw, err = json.Marshal(healthy)
	if err != nil || !strings.Contains(string(raw), `"host_port":41000`) || !strings.Contains(string(raw), `"phase":"implement"`) {
		t.Errorf("wire = %s, %v", raw, err)
	}
}

func TestStatusAndGetShapes(t *testing.T) {
	raw, _ := json.Marshal(api.StatusResult{Version: "dev", PID: 42, UptimeSeconds: 90})
	if string(raw) != `{"version":"dev","pid":42,"uptime_seconds":90}` {
		t.Errorf("status wire = %s", raw)
	}
	raw, _ = json.Marshal(api.TicketGetParams{ID: "ticket-1"})
	if string(raw) != `{"id":"ticket-1"}` {
		t.Errorf("ticket.get params wire = %s", raw)
	}
}
