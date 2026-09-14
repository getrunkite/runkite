package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// ReasonPolicyPredicate is the default reason_code when a predicate
// denies and the config row omitted reason_code. Pending with an empty
// code uses ReasonPolicyPending so Admin HITL stays on the existing path.
const ReasonPolicyPredicate = "policy_predicate"

// Predicate is one config-declared argument check. Effect is deny or
// pending only — predicates never grant access. Empty AgentID matches
// the whole tenant; empty Tool matches every tool on the connector.
// Missing When.Path does not match (not fail-closed).
type Predicate struct {
	ID         string
	TenantID   string
	AgentID    string
	Connector  string
	Tool       string
	Path       string
	Op         string
	Value      any
	Effect     string
	Reason     string
	ReasonCode string
}

// ValidatePredicate returns a normalized copy or an error if the row
// cannot be enforced. Callers skip invalid rows rather than fail-open.
func ValidatePredicate(p Predicate) (Predicate, error) {
	out := p
	out.ID = strings.TrimSpace(p.ID)
	out.TenantID = strings.TrimSpace(p.TenantID)
	out.AgentID = strings.TrimSpace(p.AgentID)
	out.Connector = strings.TrimSpace(p.Connector)
	out.Tool = strings.TrimSpace(p.Tool)
	out.Path = strings.TrimSpace(p.Path)
	out.Op = strings.ToLower(strings.TrimSpace(p.Op))
	out.Effect = strings.ToLower(strings.TrimSpace(p.Effect))
	out.Reason = strings.TrimSpace(p.Reason)
	out.ReasonCode = strings.TrimSpace(p.ReasonCode)
	if out.ID == "" {
		return Predicate{}, fmt.Errorf("predicate id is required")
	}
	if out.TenantID == "" || out.Connector == "" {
		return Predicate{}, fmt.Errorf("predicate %s: tenant_id and connector are required", out.ID)
	}
	if out.Path == "" {
		return Predicate{}, fmt.Errorf("predicate %s: when.path is required", out.ID)
	}
	switch out.Op {
	case "eq", "neq", "gt", "gte", "lt", "lte", "in", "contains", "exists":
	default:
		return Predicate{}, fmt.Errorf("predicate %s: unknown op %q", out.ID, p.Op)
	}
	if out.Effect != EffectDeny && out.Effect != EffectPending {
		return Predicate{}, fmt.Errorf("predicate %s: effect must be deny or pending", out.ID)
	}
	if out.Op == "in" {
		if _, ok := asSlice(out.Value); !ok {
			return Predicate{}, fmt.Errorf("predicate %s: in requires an array value", out.ID)
		}
	}
	return out, nil
}

func applyPredicates(rules []Predicate, in PolicyInput) *PolicyDecision {
	if in.Stage != StageToolCall || len(rules) == 0 {
		return nil
	}
	var firstDeny, firstPending *PolicyDecision
	for i := range rules {
		p := &rules[i]
		if !predicateMatchesScope(p, in) {
			continue
		}
		ok, present := evalPredicate(p, in)
		if !present || !ok {
			continue
		}
		dec := predicateDecision(p)
		if dec.Effect == EffectDeny && firstDeny == nil {
			cp := dec
			firstDeny = &cp
		}
		if dec.Effect == EffectPending && firstPending == nil {
			cp := dec
			firstPending = &cp
		}
	}
	if firstDeny != nil {
		return firstDeny
	}
	return firstPending
}

func predicateMatchesScope(p *Predicate, in PolicyInput) bool {
	if p.TenantID != strings.TrimSpace(in.TenantID) {
		return false
	}
	if p.Connector != strings.TrimSpace(in.Connector) {
		return false
	}
	if p.AgentID != "" && p.AgentID != strings.TrimSpace(in.AgentID) {
		return false
	}
	if p.Tool != "" && p.Tool != strings.TrimSpace(in.Tool) {
		return false
	}
	return true
}

func predicateDecision(p *Predicate) PolicyDecision {
	code := p.ReasonCode
	if code == "" {
		if p.Effect == EffectPending {
			code = ReasonPolicyPending
		} else {
			code = ReasonPolicyPredicate
		}
	}
	reason := p.Reason
	if reason == "" {
		reason = "denied by argument predicate"
	}
	return PolicyDecision{
		Effect:     p.Effect,
		Reason:     reason,
		ReasonCode: code,
		RuleID:     p.ID,
	}
}

// evalPredicate reports (matched, pathPresent). Missing path → (false, false).
func evalPredicate(p *Predicate, in PolicyInput) (matched, present bool) {
	got, ok := lookupPath(predicateArgs(in), p.Path)
	if p.Op == "exists" {
		return ok, true
	}
	if !ok {
		return false, false
	}
	switch p.Op {
	case "eq":
		return valuesEqual(got, p.Value), true
	case "neq":
		return !valuesEqual(got, p.Value), true
	case "gt", "gte", "lt", "lte":
		a, aOK := asFloat(got)
		b, bOK := asFloat(p.Value)
		if !aOK || !bOK {
			return false, true
		}
		switch p.Op {
		case "gt":
			return a > b, true
		case "gte":
			return a >= b, true
		case "lt":
			return a < b, true
		case "lte":
			return a <= b, true
		}
	case "in":
		sl, slOK := asSlice(p.Value)
		if !slOK {
			return false, true
		}
		for _, item := range sl {
			if valuesEqual(got, item) {
				return true, true
			}
		}
		return false, true
	case "contains":
		if gs, ok := got.(string); ok {
			want, ok := p.Value.(string)
			if !ok {
				return false, true
			}
			return strings.Contains(gs, want), true
		}
		if sl, ok := asSlice(got); ok {
			for _, item := range sl {
				if valuesEqual(item, p.Value) {
					return true, true
				}
			}
		}
		return false, true
	}
	return false, true
}

func predicateArgs(in PolicyInput) any {
	if in.Args != nil {
		return in.Args
	}
	if in.ArgsMeta != nil {
		return in.ArgsMeta
	}
	return map[string]any{}
}

func lookupPath(root any, path string) (any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil, false
		}
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func valuesEqual(a, b any) bool {
	if af, aOK := asFloat(a); aOK {
		if bf, bOK := asFloat(b); bOK {
			return af == bf
		}
		return false
	}
	if _, bOK := asFloat(b); bOK {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func asSlice(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	default:
		rv := reflect.ValueOf(v)
		if !rv.IsValid() || rv.Kind() != reflect.Slice {
			return nil, false
		}
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
}
