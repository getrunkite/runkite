package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/getrunkite/runkite/internal/policy"
)

const (
	policyFixtureInvalid = 2
	policyFixtureIOError = 3
)

func cmdPolicyTest(args []string) { os.Exit(runPolicyTest(os.Stdin, os.Stdout, os.Stderr, args)) }

// runPolicyTest intentionally has no live config loader, client, store, hook,
// secret resolver or tracing initialization. Only explicitly declared static
// fixtures reach the existing in-process policy engine.
func runPolicyTest(stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("policy test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: runkite policy test -f fixtures.json

Evaluate declared static policy expectations
without executing tools or contacting a control plane. This is separate from
live runkite sim. No model, network, Vault, secret resolution or durable writes.

Flags:
  -f, --file   strict version-1 fixture JSON (required; - reads stdin)

Exit status: 0 all match; 1 expectation mismatch; 2 invalid fixture/usage;
3 input/output or internal failure. JSON results go to stdout, errors to stderr.
`)
	}
	path := fs.String("file", "", "fixture JSON")
	fs.StringVar(path, "f", "", "fixture JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return policyFixtureInvalid
	}
	if *path == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "policy test: exactly one -f fixture is required")
		return policyFixtureInvalid
	}
	reader := stdin
	if *path != "-" {
		file, err := os.Open(*path)
		if err != nil {
			fmt.Fprintln(stderr, "policy test: cannot open fixture")
			return policyFixtureIOError
		}
		defer file.Close()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxPolicyFixtureBytes+1))
	if err != nil {
		fmt.Fprintln(stderr, "policy test: cannot read fixture")
		return policyFixtureIOError
	}
	fixture, err := parsePolicyFixture(raw)
	if err != nil {
		fmt.Fprintf(stderr, "policy test: %v\n", err)
		return policyFixtureInvalid
	}
	engine := policy.New(fixture.engineConfig())
	if engine == nil || engine.Revision() == "" {
		fmt.Fprintln(stderr, "policy test: cannot construct static policy")
		return policyFixtureIOError
	}
	report := policyFixtureReport{SchemaVersion: 1, PolicyRevision: engine.Revision(), Cases: make([]policyFixtureResult, 0, len(fixture.Cases))}
	status := 0
	for _, c := range fixture.Cases {
		input := policy.PolicyInput{Stage: c.Input.Stage, TenantID: c.Input.TenantID, Principal: c.Input.Principal, AgentID: c.Input.AgentID, Connector: c.Input.Connector, Tool: c.Input.Tool}
		if len(c.Input.Args) > 0 {
			input.Args, _ = decodePolicyFixtureValue(c.Input.Args)
		}
		decision := engine.Decide(context.Background(), input)
		actual := policyFixtureDecision{Effect: decision.Effect, ReasonCode: decision.ReasonCode, RuleID: decision.RuleID}
		passed := c.Expect.Effect == actual.Effect && (c.Expect.ReasonCode == nil || *c.Expect.ReasonCode == actual.ReasonCode) && (c.Expect.RuleID == nil || *c.Expect.RuleID == actual.RuleID)
		report.Cases = append(report.Cases, policyFixtureResult{ID: c.ID, Passed: passed, Expected: c.Expect, Actual: actual})
		if passed {
			report.Passed++
		} else {
			report.Failed++
			status = 1
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(stderr, "policy test: cannot write report")
		return policyFixtureIOError
	}
	return status
}

type policyFixtureDecision struct {
	Effect     string `json:"effect"`
	ReasonCode string `json:"reason_code"`
	RuleID     string `json:"rule_id"`
}

type policyFixtureResult struct {
	ID       string                   `json:"id"`
	Passed   bool                     `json:"passed"`
	Expected policyFixtureExpectation `json:"expected"`
	Actual   policyFixtureDecision    `json:"actual"`
}

type policyFixtureReport struct {
	SchemaVersion  int                   `json:"schema_version"`
	PolicyRevision string                `json:"policy_revision"`
	Passed         int                   `json:"passed"`
	Failed         int                   `json:"failed"`
	Cases          []policyFixtureResult `json:"cases"`
}
