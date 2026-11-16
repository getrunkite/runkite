package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/policy"
)

const (
	maxPolicyFixtureBytes = 1 << 20
	maxPolicyFixtureDepth = 32
	maxPolicyFixtureCases = 256
	maxPolicyFixtureRules = 256
	maxPolicyFixtureArgs  = 64 << 10
)

type policyFixture struct {
	SchemaVersion int                    `json:"schema_version"`
	Policy        *policyFixtureSnapshot `json:"policy"`
	Cases         []policyFixtureCase    `json:"cases"`
}

type policyFixtureSnapshot struct {
	DefaultEffect string                     `json:"default_effect,omitempty"`
	Grants        []policy.Grant             `json:"grants,omitempty"`
	Predicates    []policyFixturePredicate   `json:"predicates,omitempty"`
	MandatoryHITL []policy.MandatoryHITLRule `json:"mandatory_hitl,omitempty"`
}

type policyFixturePredicate struct {
	ID         string             `json:"id"`
	TenantID   string             `json:"tenant_id"`
	AgentID    string             `json:"agent_id,omitempty"`
	Connector  string             `json:"connector"`
	Tool       string             `json:"tool,omitempty"`
	When       *policyFixtureWhen `json:"when"`
	Effect     string             `json:"effect"`
	ReasonCode string             `json:"reason_code,omitempty"`
}

type policyFixtureWhen struct {
	Path  string          `json:"path"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}

type policyFixtureCase struct {
	ID     string                   `json:"id"`
	Input  policyFixtureInput       `json:"input"`
	Expect policyFixtureExpectation `json:"expect"`
}

type policyFixtureInput struct {
	Stage     string          `json:"stage"`
	TenantID  string          `json:"tenant_id"`
	Principal string          `json:"principal"`
	AgentID   string          `json:"agent_id"`
	Connector string          `json:"connector,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
}

type policyFixtureExpectation struct {
	Effect     string  `json:"effect"`
	ReasonCode *string `json:"reason_code,omitempty"`
	RuleID     *string `json:"rule_id,omitempty"`
}

func parsePolicyFixture(raw []byte) (*policyFixture, error) {
	if len(raw) == 0 || len(raw) > maxPolicyFixtureBytes {
		return nil, fmt.Errorf("fixture must contain 1 through %d bytes", maxPolicyFixtureBytes)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("fixture must be UTF-8 JSON")
	}
	// Bound nesting before the unique-key decoder recursively walks any values.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid fixture JSON")
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > maxPolicyFixtureDepth {
					return nil, fmt.Errorf("fixture nesting exceeds %d", maxPolicyFixtureDepth)
				}
			case '}', ']':
				depth--
			}
		}
	}
	value, err := decodePolicyFixtureValue(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid or duplicate fixture JSON keys")
	}
	if err := checkPolicyFixtureKeys(value, reflect.TypeFor[policyFixture](), "fixture"); err != nil {
		return nil, err
	}
	var fixture policyFixture
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return nil, fmt.Errorf("fixture has an invalid field type")
	}
	if err := fixture.validate(); err != nil {
		return nil, err
	}
	return &fixture, nil
}

func decodePolicyFixtureValue(raw []byte) (any, error) { return jsonvalue.DecodeUnique(raw) }

// Encoding/json matches field names without case sensitivity. Validate exact
// schema keys first, while leaving business argument/value keys untouched.
func checkPolicyFixtureKeys(value any, typ reflect.Type, path string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if value == nil {
		return fmt.Errorf("%s must not be null", path)
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = field.Type
			}
		}
		// Sort keys so multiple unknown keys always produce the same error.
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			child, ok := fields[key]
			if !ok {
				return fmt.Errorf("%s: unsupported field %q", path, key)
			}
			if err := checkPolicyFixtureKeys(object[key], child, path+"."+key); err != nil {
				return err
			}
		}
	case reflect.Slice:
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		for i, child := range array {
			if err := checkPolicyFixtureKeys(child, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func fixtureText(value string, required bool) bool {
	return (!required || value != "") && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// Scope values preserve declared identity bytes. Trimming is used only to
// reject blank values, never to rewrite an identity before engine evaluation.
func fixtureScope(value string, required bool) bool {
	return (!required || value != "") && (value == "" || strings.TrimSpace(value) != "") && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func validFixtureTools(tools []string) bool {
	if len(tools) > 128 {
		return false
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if !fixtureScope(tool, true) || seen[tool] {
			return false
		}
		seen[tool] = true
	}
	return true
}

func (f *policyFixture) validate() error {
	if f.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if f.Policy == nil {
		return fmt.Errorf("policy must be an explicit object")
	}
	p := f.Policy
	if p.DefaultEffect != "" && p.DefaultEffect != policy.EffectAllow && p.DefaultEffect != policy.EffectDeny {
		return fmt.Errorf("policy.default_effect must be allow or deny")
	}
	if len(p.Grants) > maxPolicyFixtureRules || len(p.Predicates) > maxPolicyFixtureRules || len(p.MandatoryHITL) > maxPolicyFixtureRules {
		return fmt.Errorf("each policy rule list is limited to %d", maxPolicyFixtureRules)
	}
	grantKeys := map[string]bool{}
	for i, g := range p.Grants {
		if !fixtureText(g.ID, false) || !fixtureScope(g.TenantID, true) || !fixtureScope(g.AgentID, true) || !fixtureScope(g.Connector, true) {
			return fmt.Errorf("policy.grants[%d] has invalid identity/scope fields", i)
		}
		key := g.TenantID + "\x00" + g.AgentID + "\x00" + g.Connector
		if grantKeys[key] {
			return fmt.Errorf("policy.grants[%d] duplicates tenant/agent/connector scope", i)
		}
		grantKeys[key] = true
		if g.Tools != nil && (!validFixtureTools(g.Tools.Allow) || !validFixtureTools(g.Tools.Deny)) {
			return fmt.Errorf("policy.grants[%d] has invalid tool filters", i)
		}
	}
	predicateIDs := map[string]bool{}
	for i, predicate := range p.Predicates {
		if !fixtureText(predicate.ID, true) || predicateIDs[predicate.ID] || !fixtureScope(predicate.TenantID, true) || !fixtureScope(predicate.AgentID, false) || !fixtureScope(predicate.Connector, true) || !fixtureScope(predicate.Tool, false) || !fixtureText(predicate.ReasonCode, false) || !fixtureText(predicate.Effect, true) || predicate.When == nil {
			return fmt.Errorf("policy.predicates[%d] has invalid identity/scope/when fields", i)
		}
		predicateIDs[predicate.ID] = true
		if !fixtureText(predicate.When.Path, true) || !fixtureText(predicate.When.Op, true) || len(predicate.When.Value) > maxPolicyFixtureArgs {
			return fmt.Errorf("policy.predicates[%d] has invalid/bounded when fields", i)
		}
		if !strings.EqualFold(predicate.When.Op, "exists") && len(predicate.When.Value) == 0 {
			return fmt.Errorf("policy.predicates[%d] requires when.value", i)
		}
		if _, err := policy.ValidatePredicate(predicate.enginePredicate()); err != nil {
			return fmt.Errorf("policy.predicates[%d]: %v", i, err)
		}
	}
	hitlKeys := map[string]bool{}
	for i, r := range p.MandatoryHITL {
		if !fixtureText(r.ID, false) || !fixtureScope(r.TenantID, true) || !fixtureScope(r.AgentID, false) || !fixtureScope(r.Connector, true) || !validFixtureTools(r.Tools) {
			return fmt.Errorf("policy.mandatory_hitl[%d] has invalid fields", i)
		}
		// The restrictive engine still merges these scopes after trimming.
		key := strings.TrimSpace(r.TenantID) + "\x00" + strings.TrimSpace(r.AgentID) + "\x00" + strings.TrimSpace(r.Connector)
		if hitlKeys[key] {
			return fmt.Errorf("policy.mandatory_hitl[%d] duplicates scope", i)
		}
		hitlKeys[key] = true
	}
	if len(f.Cases) == 0 || len(f.Cases) > maxPolicyFixtureCases {
		return fmt.Errorf("cases must contain 1 through %d entries", maxPolicyFixtureCases)
	}
	seen := map[string]bool{}
	for i, c := range f.Cases {
		if !fixtureText(c.ID, true) || seen[c.ID] {
			return fmt.Errorf("cases[%d] has an invalid/duplicate id", i)
		}
		seen[c.ID] = true
		in := c.Input
		if !fixtureScope(in.TenantID, true) || !fixtureScope(in.Principal, true) || !fixtureScope(in.AgentID, true) || !fixtureScope(in.Connector, false) || !fixtureScope(in.Tool, false) {
			return fmt.Errorf("cases[%d].input has invalid identity/scope fields", i)
		}
		switch in.Stage {
		case policy.StageToolCall:
			if in.Connector == "" || in.Tool == "" || len(in.Args) == 0 || len(in.Args) > maxPolicyFixtureArgs || bytes.TrimSpace(in.Args)[0] != '{' {
				return fmt.Errorf("cases[%d]: tool.call requires connector, tool and bounded object args", i)
			}
		case policy.StageConnectorSession:
			if in.Connector == "" || in.Tool != "" || len(in.Args) != 0 {
				return fmt.Errorf("cases[%d]: connector.session requires connector, without tool/args", i)
			}
		case policy.StageRunCreate:
			if in.Connector != "" || in.Tool != "" || len(in.Args) != 0 {
				return fmt.Errorf("cases[%d]: run.create does not accept connector/tool/args", i)
			}
		default:
			return fmt.Errorf("cases[%d].input.stage is unsupported", i)
		}
		if c.Expect.Effect != policy.EffectAllow && c.Expect.Effect != policy.EffectDeny && c.Expect.Effect != policy.EffectPending {
			return fmt.Errorf("cases[%d].expect.effect must be allow, deny or pending", i)
		}
		if c.Expect.ReasonCode != nil && !fixtureText(*c.Expect.ReasonCode, false) || c.Expect.RuleID != nil && !fixtureText(*c.Expect.RuleID, false) {
			return fmt.Errorf("cases[%d].expect has invalid metadata", i)
		}
	}
	return nil
}

func (p policyFixturePredicate) enginePredicate() policy.Predicate {
	result := policy.Predicate{ID: p.ID, TenantID: p.TenantID, AgentID: p.AgentID, Connector: p.Connector, Tool: p.Tool, Effect: p.Effect, ReasonCode: p.ReasonCode}
	if p.When != nil {
		result.Path = p.When.Path
		result.Op = p.When.Op
		if len(p.When.Value) > 0 {
			result.Value, _ = decodePolicyFixtureValue(p.When.Value)
		}
	}
	return result
}

func (f *policyFixture) engineConfig() policy.Config {
	cfg := policy.Config{ForceEnable: true, DefaultEffect: f.Policy.DefaultEffect, Grants: f.Policy.Grants, MandatoryHITL: f.Policy.MandatoryHITL}
	for _, p := range f.Policy.Predicates {
		cfg.Predicates = append(cfg.Predicates, p.enginePredicate())
	}
	return cfg
}
