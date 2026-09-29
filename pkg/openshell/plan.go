package openshell

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/armoriq/armoriq-openshell-middleware/pkg/iapclient"
)

// ConfigDeclaredTools is the tool set a sandbox is created to use:
//
//	config:
//	  agent_id: "..."
//	  declared_tools: ["github_list_issues", "github_create_issue"]
//
// This is the plan. Without it we can only ask whether a policy permits a tool,
// which is a weaker question than whether the sandbox said it would use it.
const ConfigDeclaredTools = "declared_tools"

// CodeNotInPlan is drift: a tool the sandbox never declared. It is a separate
// code from a policy denial because they mean different things to whoever reads
// the audit. Policy said no, versus you never said you would do this.
const CodeNotInPlan = "tool_not_in_plan"

// DeclaredToolsFromConfig reads the declared plan. An empty result means the
// policy declared none, and only the policy check applies.
func DeclaredToolsFromConfig(cfg *structpb.Struct) []string {
	v, ok := cfg.GetFields()[ConfigDeclaredTools]
	if !ok {
		return nil
	}
	list := v.GetListValue()
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list.GetValues()))
	for _, item := range list.GetValues() {
		if s := strings.TrimSpace(item.GetStringValue()); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// InPlan reports whether a tool is one the sandbox declared.
func InPlan(declared []string, tool string) bool {
	for _, d := range declared {
		if d == tool {
			return true
		}
	}
	return false
}

// planFingerprint changes when the declared plan or the agent changes, which is
// what makes a policy update on a live sandbox mint a fresh token rather than
// keep serving the previous task's plan id.
func planFingerprint(agentID string, declared []string) string {
	h := sha256.New()
	h.Write([]byte(agentID))
	h.Write([]byte{0})
	for _, d := range declared {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// planCache holds one minted token per sandbox.
//
// Minting is a round trip, and doing it per request would double our latency
// against a budget we already share with policy evaluation. The lock is held
// only around map access, never across the mint: the control plane keys
// issuance by plan hash and is idempotent, so two concurrent mints for the same
// plan are harmless and preferable to serialising every request for a sandbox.
type planCache struct {
	mu       sync.Mutex
	entries  map[string]planEntry
	inflight map[string]bool
	ttl      time.Duration
}

type planEntry struct {
	fingerprint string
	token       iapclient.Token
	renewAt     time.Time
}

func newPlanCache(ttl time.Duration) *planCache {
	return &planCache{
		entries:  make(map[string]planEntry),
		inflight: make(map[string]bool),
		ttl:      ttl,
	}
}

// claim reports whether the caller should start minting for this plan. It
// returns false when a mint is already running, so a burst of requests from one
// sandbox produces one mint rather than one per request.
func (c *planCache) claim(sandboxID, fingerprint string) bool {
	k := sandboxID + "/" + fingerprint
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[k] {
		return false
	}
	c.inflight[k] = true
	return true
}

func (c *planCache) release(sandboxID, fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, sandboxID+"/"+fingerprint)
}

// get returns a cached token when it is still fresh and still describes this
// plan.
func (c *planCache) get(sandboxID, fingerprint string) (iapclient.Token, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sandboxID]
	if !ok || e.fingerprint != fingerprint || time.Now().After(e.renewAt) {
		return iapclient.Token{}, false
	}
	return e.token, true
}

func (c *planCache) put(sandboxID, fingerprint string, t iapclient.Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[sandboxID] = planEntry{
		fingerprint: fingerprint,
		token:       t,
		renewAt:     time.Now().Add(c.ttl),
	}
}

// forget drops a sandbox's token, so the next request mints a new one.
func (c *planCache) forget(sandboxID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sandboxID)
}
