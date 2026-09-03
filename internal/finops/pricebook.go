// Package finops provides optional model pricebooks and budget caps for
// run admission (F0) and usage_events USD estimates (F1 foundation).
package finops

import (
	"strings"
)

// ModelPrice is USD per 1k tokens for one model.
type ModelPrice struct {
	InputPer1k  float64 `json:"input_per_1k" yaml:"input_per_1k"`
	OutputPer1k float64 `json:"output_per_1k" yaml:"output_per_1k"`
}

// Pricebook maps model id → per-1k USD rates. Empty book: prefer
// reported cost_usd; otherwise usd_estimate stays 0 from tokens alone.
type Pricebook map[string]ModelPrice

// EstimateUSD returns a USD estimate for one usage observation.
// When costUSD > 0 it wins (runner-reported). Otherwise, if the model
// has a pricebook entry, tokens are priced; else 0.
func (p Pricebook) EstimateUSD(model string, tokensIn, tokensOut int64, costUSD float64) float64 {
	if costUSD > 0 {
		return costUSD
	}
	mp, ok := p.lookup(model)
	if !ok {
		return 0
	}
	usd := 0.0
	if tokensIn > 0 && mp.InputPer1k != 0 {
		usd += float64(tokensIn) / 1000.0 * mp.InputPer1k
	}
	if tokensOut > 0 && mp.OutputPer1k != 0 {
		usd += float64(tokensOut) / 1000.0 * mp.OutputPer1k
	}
	return usd
}

// HasModel reports whether model (or its provider-suffix soft match) has a
// pricebook row. Used to distinguish "intentionally free / tokens-only"
// (empty book, or row with $0 rates) from "admin forgot this model".
func (p Pricebook) HasModel(model string) bool {
	_, ok := p.lookup(model)
	return ok
}

func (p Pricebook) lookup(model string) (ModelPrice, bool) {
	if len(p) == 0 || model == "" {
		return ModelPrice{}, false
	}
	mp, ok := p[model]
	if ok {
		return mp, true
	}
	// Soft match: trim common provider prefixes (openai/gpt-4o-mini).
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		mp, ok = p[model[i+1:]]
		return mp, ok
	}
	return ModelPrice{}, false
}
