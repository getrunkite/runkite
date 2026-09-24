package main

import (
	"fmt"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
)

// matchPolicyEffects compares fixture expect.policy_effects to Admin audit
// rows. Only tool.call deny/pending count; allows are ignored. Set equality:
// every expected tuple must match one unused audit row (reason_code only
// when the fixture named one), and leftover deny/pending fail the case.
func matchPolicyEffects(got []models.AuditEvent, want []simPolicyEffect) error {
	var relevant []models.AuditEvent
	for _, ev := range got {
		if ev.Action != policy.StageToolCall {
			continue
		}
		if ev.Decision != policy.EffectDeny && ev.Decision != policy.EffectPending {
			continue
		}
		relevant = append(relevant, ev)
	}
	used := make([]bool, len(relevant))
	for _, w := range want {
		idx := -1
		for i, ev := range relevant {
			if used[i] {
				continue
			}
			if ev.Connector != w.Connector || ev.Tool != w.Tool || ev.Decision != w.Effect {
				continue
			}
			if w.ReasonCode != "" && ev.ReasonCode != w.ReasonCode {
				continue
			}
			idx = i
			break
		}
		if idx < 0 {
			gotLabel := "allow"
			for _, ev := range relevant {
				if ev.Connector == w.Connector && ev.Tool == w.Tool {
					gotLabel = ev.Decision
					break
				}
			}
			if w.ReasonCode != "" {
				return fmt.Errorf("expected %s on %s/%s reason_code=%s, got %s", w.Effect, w.Connector, w.Tool, w.ReasonCode, gotLabel)
			}
			return fmt.Errorf("expected %s on %s/%s, got %s", w.Effect, w.Connector, w.Tool, gotLabel)
		}
		used[idx] = true
	}
	for i, ev := range relevant {
		if !used[i] {
			return fmt.Errorf("unexpected %s on %s/%s", ev.Decision, ev.Connector, ev.Tool)
		}
	}
	return nil
}
