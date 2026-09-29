package iapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PlanStep is one declared step. The middleware declares a plan as a set of
// tools rather than an ordered script, because nothing tells us what order the
// agent intends to work in. Position is filled so the shape matches what the
// control plane expects.
type PlanStep struct {
	StepID    string         `json:"step_id"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Position  int            `json:"position"`
}

// Plan is the declared intent for a sandbox.
type Plan struct {
	Version  string         `json:"version"`
	Metadata map[string]any `json:"metadata"`
	Steps    []PlanStep     `json:"steps"`
}

// PlanFromTools builds a plan from a declared tool list.
func PlanFromTools(tools []string, metadata map[string]any) Plan {
	steps := make([]PlanStep, 0, len(tools))
	for i, t := range tools {
		steps = append(steps, PlanStep{
			StepID:    fmt.Sprintf("declared-%d", i),
			Tool:      t,
			Arguments: map[string]any{},
			Position:  i,
		})
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return Plan{Version: "1.0", Metadata: metadata, Steps: steps}
}

// TokenRequest mints an intent token over a declared plan.
type TokenRequest struct {
	UserID    string `json:"user_id"`
	AgentID   string `json:"agent_id"`
	UserEmail string `json:"user_email,omitempty"`
	Plan      Plan   `json:"plan"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}

// Token is a minted intent token.
//
// Raw is the whole response plus the plan, which is the shape enforce expects
// back as intent_token. The control plane reads plan_id off it to attribute an
// approval, so passing the parts rather than the whole would silently break
// hold.
type Token struct {
	PlanID   string
	PlanHash string
	Raw      map[string]any
}

// IssueToken declares a plan and returns the token minted over it.
//
// Issuance is idempotent on the control plane and keyed by plan hash, so
// re-minting the same plan is cheap and returns the same identity.
func (c *Client) IssueToken(ctx context.Context, in TokenRequest) (Token, error) {
	var out Token

	if in.ExpiresIn == 0 {
		in.ExpiresIn = 900
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/iap/sdk/token", bytes.NewReader(payload))
	if err != nil {
		return out, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+c.key)

	resp, err := c.hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, fmt.Errorf("read: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusUnauthorized:
		return out, ErrUnauthorized
	default:
		return out, fmt.Errorf("status %d: %s", resp.StatusCode, snippet(body))
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return out, fmt.Errorf("decode: %w", err)
	}
	// success is reported in the body, not only the status.
	if ok, present := raw["success"].(bool); present && !ok {
		return out, fmt.Errorf("token refused: %v", raw["message"])
	}

	planID, _ := raw["plan_id"].(string)
	if planID == "" {
		return out, fmt.Errorf("response carried no plan_id, so no approval could ever attach")
	}
	planHash, _ := raw["plan_hash"].(string)

	// The plan goes back with the token. enforce reads plan_id off this object.
	raw["plan"] = in.Plan

	return Token{PlanID: planID, PlanHash: planHash, Raw: raw}, nil
}
