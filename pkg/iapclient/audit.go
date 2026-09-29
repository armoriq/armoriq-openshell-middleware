package iapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxBatch is their limit. A larger batch is rejected outright.
const maxBatch = 200

// AuditRow is one decision, in the shape POST /iap/audit/batch accepts.
//
// Field names are theirs. Status is one of success, failed, error, blocked or
// held, taken from the AuditStatus enum on their side.
type AuditRow struct {
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`

	StepIndex int            `json:"step_index"`
	Action    string         `json:"action"`
	Tool      string         `json:"tool"`
	Input     map[string]any `json:"input"`
	Output    map[string]any `json:"output,omitempty"`

	Status       string `json:"status"`
	ErrorMessage string `json:"error_message,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
	ExecutedAt   string `json:"executed_at"`
}

// PostAudit writes rows. It is never called on the path that decides a request.
func (c *Client) PostAudit(ctx context.Context, rows []AuditRow) error {
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > maxBatch {
		return fmt.Errorf("batch of %d exceeds the %d row limit", len(rows), maxBatch)
	}

	payload, err := json.Marshal(struct {
		Rows []AuditRow `json:"rows"`
	}{rows})
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/iap/audit/batch", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.key)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

// AuditBuffer collects rows and writes them in batches.
//
// Auditing must not slow a decision down and must not be able to fail one. Add
// never blocks and never returns an error: when the buffer is full the row is
// dropped and counted, because holding up a sandbox request to record what we
// did to it would be the wrong trade under a 500ms ceiling.
type AuditBuffer struct {
	client  *Client
	rows    chan AuditRow
	dropped chan struct{}
}

func NewAuditBuffer(c *Client, size int) *AuditBuffer {
	return &AuditBuffer{
		client:  c,
		rows:    make(chan AuditRow, size),
		dropped: make(chan struct{}, 1),
	}
}

// Add queues a row, or drops it. Safe from any goroutine.
func (b *AuditBuffer) Add(row AuditRow) {
	select {
	case b.rows <- row:
	default:
		select {
		case b.dropped <- struct{}{}:
		default:
		}
	}
}

// Run flushes on an interval until ctx is done, then flushes what is left.
func (b *AuditBuffer) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// A short deadline of its own: ctx is already cancelled, and the
			// rows collected so far are still worth one attempt.
			flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			b.flush(flush, onErr)
			cancel()
			return
		case <-t.C:
			b.flush(ctx, onErr)
		}
	}
}

func (b *AuditBuffer) flush(ctx context.Context, onErr func(error)) {
	select {
	case <-b.dropped:
		if onErr != nil {
			onErr(fmt.Errorf("audit buffer overflowed, some decisions were not recorded"))
		}
	default:
	}

	batch := make([]AuditRow, 0, maxBatch)
	for len(batch) < maxBatch {
		select {
		case row := <-b.rows:
			batch = append(batch, row)
		default:
			goto send
		}
	}
send:
	if len(batch) == 0 {
		return
	}
	if err := b.client.PostAudit(ctx, batch); err != nil && onErr != nil {
		onErr(fmt.Errorf("writing %d audit rows: %w", len(batch), err))
	}
}
