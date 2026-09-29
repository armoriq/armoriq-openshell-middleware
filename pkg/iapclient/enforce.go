// Package iapclient calls the ArmorIQ control plane for an enforcement
// decision.
//
// This is the same endpoint the customer SDK uses, POST /iap/sdk/enforce, and
// not the proxy path: the proxy verifies a step and carries CSRG proofs, which
// is a different contract. Their normalizer collapses the authoring vocabulary
// to a terminal set before it reaches any client, so the only two fields worth
// reading back are `allowed` and `enforcementAction`, and an action we do not
// recognise means block.
package iapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Decision is the terminal enforcement vocabulary. The control plane guarantees
// only these three reach a client.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionBlock Decision = "block"
	DecisionHold  Decision = "hold"
)

// ErrUnauthorized means the API key was rejected. It is separated because an
// operator reads it as a deployment fault, not as a policy denial.
var ErrUnauthorized = errors.New("iap: api key rejected")

// Request is one enforcement question. Tool is required: the endpoint rejects an
// empty one with 400, so traffic we cannot name cannot be asked about this way.
type Request struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	UserEmail string         `json:"user_email,omitempty"`

	// IntentToken carries the declared plan. Without it the control plane has no
	// plan id, and an approval can never be attributed to the call, so a hold is
	// raised and never clears.
	IntentToken map[string]any `json:"intent_token,omitempty"`
}

// Result is what the control plane decided.
type Result struct {
	Allowed bool     `json:"allowed"`
	Action  Decision `json:"enforcementAction"`
	Reason  string   `json:"reason,omitempty"`
	Message string   `json:"message,omitempty"`

	RequiresApproval bool `json:"requiresApproval,omitempty"`

	MatchedPolicy struct {
		PolicyID string `json:"policyId,omitempty"`
		Name     string `json:"name,omitempty"`
	} `json:"matchedPolicy,omitempty"`

	Delegation struct {
		PlanID         string `json:"planId,omitempty"`
		RequesterEmail string `json:"requesterEmail,omitempty"`
	} `json:"delegationContext,omitempty"`
}

// Allow reports whether the call may proceed.
//
// Both fields have to agree. The control plane derives allowed strictly from
// the action so that a gate can never come back alongside allowed true, and
// checking both here means a future change on either side fails closed instead
// of opening a hole quietly.
func (r Result) Allow() bool { return r.Allowed && r.Action == DecisionAllow }

// Client talks to one control plane with one API key.
type Client struct {
	base string
	key  string
	hc   *http.Client
}

// New builds a client. The timeout has to fit inside the middleware's own
// binding budget, so it is deliberately short and the caller's context still
// wins if it is shorter.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		base: baseURL,
		key:  apiKey,
		hc: &http.Client{
			Timeout: timeout,
			// Connections are pooled on purpose. A fresh TLS handshake per
			// request does not fit in the budget we are held to.
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   2 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}
}

// Enforce asks for a decision. Any error means we did not get one, and the
// caller is expected to fail closed.
func (c *Client) Enforce(ctx context.Context, in Request) (Result, error) {
	var out Result

	payload, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/iap/sdk/enforce", bytes.NewReader(payload))
	if err != nil {
		return out, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.key)
	if in.UserEmail != "" {
		req.Header.Set("x-user-email", in.UserEmail)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()

	// Bounded because a decision is small and the response is not something we
	// want to let grow unchecked inside a 500ms budget.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("read: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return out, ErrUnauthorized
	default:
		return out, fmt.Errorf("status %d: %s", resp.StatusCode, snippet(body))
	}

	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// snippet keeps an unexpected response readable in a log line without pasting a
// whole error page into it.
func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// Warm establishes the TLS connection before a real decision needs it.
//
// The first call out of a cold process pays DNS and a TLS handshake, which was
// measured at 367ms against a 400ms budget while a warm call was 87ms. Under a
// fail closed ceiling that difference is the gap between a decision and a
// denied request, so the connection is opened at startup and kept open rather
// than built on the critical path.
func (c *Client) Warm(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/iap/public-key", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The body has to be drained for the connection to go back to the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// KeepWarm re-warms on an interval and returns when ctx is done.
//
// Idle connections are dropped after IdleConnTimeout, so a sandbox that goes
// quiet for a few minutes would pay the cold cost again on its next tool call.
func (c *Client) KeepWarm(ctx context.Context, every time.Duration, onErr func(error)) {
	if err := c.Warm(ctx); err != nil && onErr != nil {
		onErr(err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Warm(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// Clone returns a client sharing this one's connection pool but with its own
// timeout. Minting a plan token is far slower than asking for a decision, and
// it happens off the request path, so it must not be held to the same budget.
func (c *Client) Clone(timeout time.Duration) *Client {
	return &Client{base: c.base, key: c.key, hc: &http.Client{
		Timeout:   timeout,
		Transport: c.hc.Transport,
	}}
}
