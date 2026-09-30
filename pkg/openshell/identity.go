package openshell

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"
)

// Keys an operator may set on the middleware binding in a sandbox policy:
//
//	network_middlewares:
//	  armoriq:
//	    middleware: armoriq-intent
//	    config:
//	      agent_id: "..."
//	      user_email: "..."
const (
	ConfigAgentID   = "agent_id"
	ConfigUserEmail = "user_email"
)

// IdentityFromConfig reads the identity the sandbox policy declares.
//
// This is where identity comes from, and it costs nothing: OpenShell sends the
// binding's config with every evaluation. The alternative was a registry keyed
// by sandbox id, which would put a lookup on the critical path and need
// something to write the row at sandbox creation. Declaring it in the policy
// puts it in the hands of whoever creates the sandbox, who is the one who knows
// what it is for.
//
// It is a declaration, not a proof. Anyone who can write the sandbox policy can
// claim an agent, but that same person already chooses the filesystem and
// network rules for the sandbox, so this grants no authority they did not have.
func IdentityFromConfig(cfg *structpb.Struct) Identity {
	return Identity{
		AgentID:   configString(cfg, ConfigAgentID),
		UserEmail: configString(cfg, ConfigUserEmail),
	}
}

func configString(cfg *structpb.Struct, key string) string {
	v, ok := cfg.GetFields()[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

// ValidateIntentConfig is the schema for that config block.
//
// Unknown keys are refused rather than ignored. A policy saying `agentid`
// instead of `agent_id` would otherwise apply cleanly and leave every request
// from that sandbox with no identity, which is the kind of mistake that is only
// noticed once the wrong policy has been enforced for a while.
func ValidateIntentConfig(cfg *structpb.Struct) error {
	var unknown []string
	for k, v := range cfg.GetFields() {
		switch k {
		case ConfigAgentID, ConfigUserEmail:
			if _, ok := v.GetKind().(*structpb.Value_StringValue); !ok {
				return fmt.Errorf("%s must be a string", k)
			}
		case ConfigModelHosts:
			list := v.GetListValue()
			if list == nil {
				return fmt.Errorf("%s must be a list of host names", ConfigModelHosts)
			}
			for _, item := range list.GetValues() {
				if _, ok := item.GetKind().(*structpb.Value_StringValue); !ok {
					return fmt.Errorf("%s must contain only host names", ConfigModelHosts)
				}
			}
		case ConfigRequireCapturedPlan:
			if _, ok := v.GetKind().(*structpb.Value_BoolValue); !ok {
				return fmt.Errorf("%s must be true or false", k)
			}
		case ConfigDeclaredTools:
			list := v.GetListValue()
			if list == nil {
				return fmt.Errorf("%s must be a list of tool names", ConfigDeclaredTools)
			}
			for _, item := range list.GetValues() {
				if _, ok := item.GetKind().(*structpb.Value_StringValue); !ok {
					return fmt.Errorf("%s must contain only tool names", ConfigDeclaredTools)
				}
			}
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown config key %v, accepted keys are %s, %s, %s, %s and %s",
			unknown, ConfigAgentID, ConfigUserEmail, ConfigDeclaredTools, ConfigModelHosts, ConfigRequireCapturedPlan)
	}
	return nil
}
