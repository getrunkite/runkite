package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// simFile is one fixture YAML document. Unknown keys at this level or
// under a case/expect/policy_effect fail parse so a typo cannot silently
// pass.
type simFile struct {
	Agent  string    `yaml:"agent"`
	Tenant string    `yaml:"tenant"`
	Cases  []simCase `yaml:"cases"`
}

type simCase struct {
	ID     string     `yaml:"id"`
	Agent  string     `yaml:"agent"`
	Input  any        `yaml:"input"`
	Expect *simExpect `yaml:"expect"`
}

type simExpect struct {
	Status        string             `yaml:"status"`
	PolicyEffects *[]simPolicyEffect `yaml:"policy_effects"`
}

type simPolicyEffect struct {
	Connector  string `yaml:"connector"`
	Tool       string `yaml:"tool"`
	Effect     string `yaml:"effect"`
	ReasonCode string `yaml:"reason_code"`
}

func parseSimFile(path string) (*simFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var out simFile
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := out.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &out, nil
}

func (f *simFile) validate() error {
	if len(f.Cases) == 0 {
		return fmt.Errorf("no cases")
	}
	seen := map[string]struct{}{}
	for i, c := range f.Cases {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("case %d: missing id", i)
		}
		if _, ok := seen[c.ID]; ok {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if c.agent(f.Agent) == "" {
			return fmt.Errorf("case %q: agent is required (file-level or per-case)", c.ID)
		}
		if c.Expect != nil {
			if err := c.Expect.validate(c.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *simExpect) validate(caseID string) error {
	if e.PolicyEffects == nil {
		return nil
	}
	for i, pe := range *e.PolicyEffects {
		if pe.Connector == "" || pe.Tool == "" || pe.Effect == "" {
			return fmt.Errorf("case %q: policy_effects[%d] needs connector, tool, and effect", caseID, i)
		}
	}
	return nil
}

func (c simCase) agent(fileAgent string) string {
	if strings.TrimSpace(c.Agent) != "" {
		return c.Agent
	}
	return fileAgent
}
