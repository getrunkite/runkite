package finops

import (
	"encoding/json"
	"fmt"
	"time"
)

// Clone returns a deep copy of cfg (nil-safe).
func Clone(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := &Config{
		Alerts:       cfg.Alerts,
		OnHardBreach: cfg.OnHardBreach,
		Reservation: ReservationConfig{
			USDPerRun:    cfg.Reservation.USDPerRun,
			TokensPerRun: cfg.Reservation.TokensPerRun,
			HoldTTL:      cfg.Reservation.HoldTTL,
		},
		Routing: RoutingConfig{
			Enabled: cfg.Routing.Enabled,
			SoftPct: cfg.Routing.SoftPct,
		},
	}
	if len(cfg.Pricebook) > 0 {
		out.Pricebook = make(Pricebook, len(cfg.Pricebook))
		for k, v := range cfg.Pricebook {
			out.Pricebook[k] = v
		}
	} else {
		out.Pricebook = Pricebook{}
	}
	out.Budgets.Tenants = cloneCaps(cfg.Budgets.Tenants)
	out.Budgets.Agents = cloneCaps(cfg.Budgets.Agents)
	if len(cfg.Reservation.Agents) > 0 {
		out.Reservation.Agents = make(map[string]ReservationAmount, len(cfg.Reservation.Agents))
		for k, v := range cfg.Reservation.Agents {
			out.Reservation.Agents[k] = v
		}
	}
	if len(cfg.Routing.Aliases) > 0 {
		out.Routing.Aliases = make(map[string][]string, len(cfg.Routing.Aliases))
		for k, v := range cfg.Routing.Aliases {
			out.Routing.Aliases[k] = append([]string(nil), v...)
		}
	}
	return out
}

func cloneCaps(in map[string]BudgetCap) map[string]BudgetCap {
	if len(in) == 0 {
		return map[string]BudgetCap{}
	}
	out := make(map[string]BudgetCap, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Merge applies overlay onto base. Overlay map keys replace/add; scalar
// sections replace when the overlay JSON set them to a non-zero /
// non-empty value (partial patch semantics matching Admin PUT bodies).
// Neither argument is mutated. Nil overlay → Clone(base).
func Merge(base, overlay *Config) *Config {
	if overlay == nil {
		return Clone(base)
	}
	if base == nil {
		return Clone(overlay)
	}
	out := Clone(base)
	for k, v := range overlay.Pricebook {
		out.Pricebook[k] = v
	}
	for k, v := range overlay.Budgets.Tenants {
		out.Budgets.Tenants[k] = v
	}
	for k, v := range overlay.Budgets.Agents {
		out.Budgets.Agents[k] = v
	}
	if overlay.Alerts.SoftPct > 0 {
		out.Alerts.SoftPct = overlay.Alerts.SoftPct
	}
	if overlay.Reservation.USDPerRun > 0 || overlay.Reservation.TokensPerRun > 0 ||
		overlay.Reservation.HoldTTL > 0 || len(overlay.Reservation.Agents) > 0 {
		if overlay.Reservation.USDPerRun > 0 {
			out.Reservation.USDPerRun = overlay.Reservation.USDPerRun
		}
		if overlay.Reservation.TokensPerRun > 0 {
			out.Reservation.TokensPerRun = overlay.Reservation.TokensPerRun
		}
		if overlay.Reservation.HoldTTL > 0 {
			out.Reservation.HoldTTL = overlay.Reservation.HoldTTL
		}
		for k, v := range overlay.Reservation.Agents {
			if out.Reservation.Agents == nil {
				out.Reservation.Agents = map[string]ReservationAmount{}
			}
			out.Reservation.Agents[k] = v
		}
	}
	if overlay.Routing.Enabled || overlay.Routing.SoftPct > 0 || len(overlay.Routing.Aliases) > 0 {
		if overlay.Routing.Enabled {
			out.Routing.Enabled = true
		}
		if overlay.Routing.SoftPct > 0 {
			out.Routing.SoftPct = overlay.Routing.SoftPct
		}
		for k, v := range overlay.Routing.Aliases {
			if out.Routing.Aliases == nil {
				out.Routing.Aliases = map[string][]string{}
			}
			out.Routing.Aliases[k] = append([]string(nil), v...)
		}
	}
	if overlay.OnHardBreach != "" {
		out.OnHardBreach = overlay.OnHardBreach
	}
	return out
}

// overlayWire is the Admin/file-shaped JSON (hold_ttl as Go duration string).
type overlayWire struct {
	Pricebook    map[string]ModelPrice `json:"pricebook,omitempty"`
	Budgets      *Budgets              `json:"budgets,omitempty"`
	Alerts       *AlertsConfig         `json:"alerts,omitempty"`
	Reservation  *overlayReservation   `json:"reservation,omitempty"`
	Routing      *RoutingConfig        `json:"routing,omitempty"`
	OnHardBreach string                `json:"on_hard_breach,omitempty"`
}

type overlayReservation struct {
	USDPerRun    float64                      `json:"usd_per_run,omitempty"`
	TokensPerRun int64                        `json:"tokens_per_run,omitempty"`
	HoldTTL      string                       `json:"hold_ttl,omitempty"`
	Agents       map[string]ReservationAmount `json:"agents,omitempty"`
}

// DecodeOverlayPayload unmarshals a partial Config from overlay JSON.
// Accepts reservation.hold_ttl as a Go duration string (e.g. "24h").
func DecodeOverlayPayload(raw json.RawMessage) (*Config, error) {
	empty := &Config{Pricebook: Pricebook{}, Budgets: Budgets{Tenants: map[string]BudgetCap{}, Agents: map[string]BudgetCap{}}}
	if len(raw) == 0 || string(raw) == "null" {
		return empty, nil
	}
	var wire overlayWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	cfg := empty
	if wire.Pricebook != nil {
		cfg.Pricebook = wire.Pricebook
	}
	if wire.Budgets != nil {
		cfg.Budgets = *wire.Budgets
		if cfg.Budgets.Tenants == nil {
			cfg.Budgets.Tenants = map[string]BudgetCap{}
		}
		if cfg.Budgets.Agents == nil {
			cfg.Budgets.Agents = map[string]BudgetCap{}
		}
	}
	if wire.Alerts != nil {
		cfg.Alerts = *wire.Alerts
	}
	if wire.Reservation != nil {
		cfg.Reservation.USDPerRun = wire.Reservation.USDPerRun
		cfg.Reservation.TokensPerRun = wire.Reservation.TokensPerRun
		cfg.Reservation.Agents = wire.Reservation.Agents
		if ttl := wire.Reservation.HoldTTL; ttl != "" && ttl != "0" {
			d, err := time.ParseDuration(ttl)
			if err != nil {
				return nil, fmt.Errorf("reservation.hold_ttl: %w", err)
			}
			if d > 0 {
				cfg.Reservation.HoldTTL = d
			}
		}
	}
	if wire.Routing != nil {
		cfg.Routing = *wire.Routing
	}
	cfg.OnHardBreach = wire.OnHardBreach
	return cfg, nil
}

// EncodeOverlayPayload marshals cfg for durable storage (hold_ttl as string).
func EncodeOverlayPayload(cfg *Config) (json.RawMessage, error) {
	if cfg == nil {
		return json.RawMessage(`{}`), nil
	}
	wire := overlayWire{
		Pricebook:    cfg.Pricebook,
		Budgets:      &cfg.Budgets,
		OnHardBreach: cfg.OnHardBreach,
	}
	if cfg.Alerts.SoftPct > 0 {
		a := cfg.Alerts
		wire.Alerts = &a
	}
	if cfg.Reservation.USDPerRun > 0 || cfg.Reservation.TokensPerRun > 0 ||
		cfg.Reservation.HoldTTL > 0 || len(cfg.Reservation.Agents) > 0 {
		wire.Reservation = &overlayReservation{
			USDPerRun:    cfg.Reservation.USDPerRun,
			TokensPerRun: cfg.Reservation.TokensPerRun,
			Agents:       cfg.Reservation.Agents,
		}
		if cfg.Reservation.HoldTTL > 0 {
			wire.Reservation.HoldTTL = cfg.Reservation.HoldTTL.String()
		}
	}
	if cfg.Routing.Enabled || cfg.Routing.SoftPct > 0 || len(cfg.Routing.Aliases) > 0 {
		r := cfg.Routing
		wire.Routing = &r
	}
	return json.Marshal(wire)
}
