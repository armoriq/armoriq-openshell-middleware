package openshell

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
)

// auditSink stands up a control plane that allows every enforce call and
// collects whatever audit rows are posted to it.
type auditSink struct {
	mu   sync.Mutex
	rows []iapclient.AuditRow
	srv  *httptest.Server
}

func newAuditSink(t *testing.T) *auditSink {
	t.Helper()
	s := &auditSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iap/audit/batch":
			var body struct {
				Rows []iapclient.AuditRow `json:"rows"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.rows = append(s.rows, body.Rows...)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"written": len(body.Rows)})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "enforcementAction": "allow"})
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *auditSink) collected() []iapclient.AuditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]iapclient.AuditRow(nil), s.rows...)
}

// verifierWithAudit returns a verifier whose buffer is flushed by calling the
// returned func, so the test never sleeps waiting on a ticker.
func verifierWithAudit(t *testing.T, s *auditSink, unnamed UnnamedAction, static Identity) (*IntentVerifier, func()) {
	t.Helper()
	c := iapclient.New(s.srv.URL, "k", 2*time.Second)
	buf := iapclient.NewAuditBuffer(c, 64)
	v, err := NewIntentVerifier(c, StaticIdentity(static), unnamed, 0)
	if err != nil {
		t.Fatal(err)
	}
	v.WithAudit(buf)

	return v, func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		// Run flushes once on the way out, which is the flush under test.
		go func() { buf.Run(ctx, time.Hour, nil); close(done) }()
		cancel()
		<-done
	}
}

// A denial we make on our own reaches the audit, because nothing else records
// it: the control plane was never asked.
func TestOurOwnDenialIsRecorded(t *testing.T) {
	sink := newAuditSink(t)
	v, flush := verifierWithAudit(t, sink, DenyUnnamed, Identity{AgentID: "agent-7"})

	plainGet := &HttpRequestEvaluation{
		Context: &RequestContext{SandboxId: "sbx-9"},
		Target:  &HttpRequestTarget{Scheme: "https", Host: "api.github.com", Method: "GET", Path: "/zen"},
	}
	if _, err := v.Verify(context.Background(), plainGet); err != nil {
		t.Fatal(err)
	}
	flush()

	rows := sink.collected()
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Status != "blocked" {
		t.Errorf("status = %q, want blocked", got.Status)
	}
	if got.SessionID != "sbx-9" {
		t.Errorf("session = %q, the sandbox is what attributes the row", got.SessionID)
	}
	// There is no tool, so the request itself is named rather than inventing one.
	if got.Tool != "GET api.github.com/zen" {
		t.Errorf("tool = %q", got.Tool)
	}
	if got.Output["reason_code"] != CodeUnnamed {
		t.Errorf("reason_code = %v, want %s", got.Output["reason_code"], CodeUnnamed)
	}
	if got.ExecutedAt == "" {
		t.Error("executed_at is required by their DTO")
	}
}

// A sandbox with no agent is denied before the control plane is asked, so that
// denial has to be recorded here too.
func TestTheNoAgentDenialIsRecorded(t *testing.T) {
	sink := newAuditSink(t)
	v, flush := verifierWithAudit(t, sink, DenyUnnamed, Identity{})

	if _, err := v.Verify(context.Background(), mcpCall("github_create_issue")); err != nil {
		t.Fatal(err)
	}
	flush()

	rows := sink.collected()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Output["reason_code"] != CodeNoPlan {
		t.Errorf("reason_code = %v, want %s", rows[0].Output["reason_code"], CodeNoPlan)
	}
	if rows[0].Tool != "github_create_issue" {
		t.Errorf("tool = %q", rows[0].Tool)
	}
}

// The control plane already writes a row for every enforce call. Writing one
// here as well would count the same decision twice.
func TestAnEnforcedDecisionIsNotRecordedTwice(t *testing.T) {
	sink := newAuditSink(t)
	v, flush := verifierWithAudit(t, sink, DenyUnnamed, Identity{AgentID: "agent-7"})

	req := mcpCall("github_create_issue")
	req.Config, _ = structpb.NewStruct(map[string]any{ConfigAgentID: "agent-7"})

	got, err := v.Verify(context.Background(), req)
	if err != nil || !got.Allow {
		t.Fatalf("verify: %+v %v", got, err)
	}
	flush()

	if rows := sink.collected(); len(rows) != 0 {
		t.Fatalf("the control plane already recorded this one, got %d extra rows: %+v", len(rows), rows)
	}
}

// Auditing must never be able to hold up or fail a decision, so a full buffer
// drops rows instead of blocking.
func TestAFullBufferDropsRatherThanBlocks(t *testing.T) {
	sink := newAuditSink(t)
	buf := iapclient.NewAuditBuffer(iapclient.New(sink.srv.URL, "k", time.Second), 2)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			buf.Add(iapclient.AuditRow{Tool: "t", Status: "blocked"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Add blocked when the buffer was full")
	}
}
