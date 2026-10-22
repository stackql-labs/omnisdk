package stackqldoc

import (
	"slices"
	"time"
)

type retrySpec struct {
	MaxAttempts    int      `yaml:"max_attempts"`
	InitialDelayMS int      `yaml:"initial_delay_ms"`
	MaxDelayMS     int      `yaml:"max_delay_ms"`
	Factor         float64  `yaml:"multiplier"`
	Style          string   `yaml:"algorithm"`
	Verbs          []string `yaml:"retryable_methods"`
	Conditions     struct {
		Codes []int `yaml:"status_codes"`
	} `yaml:"retryable_conditions"`
}

func (r *retrySpec) Attempts() int { return r.MaxAttempts }
func (r *retrySpec) InitialDelay() time.Duration {
	return time.Duration(r.InitialDelayMS) * time.Millisecond
}
func (r *retrySpec) MaxDelay() time.Duration { return time.Duration(r.MaxDelayMS) * time.Millisecond }
func (r *retrySpec) Multiplier() float64     { return r.Factor }
func (r *retrySpec) Algorithm() string       { return r.Style }
func (r *retrySpec) Methods() []string       { return slices.Clone(r.Verbs) }
func (r *retrySpec) Statuses() []int         { return slices.Clone(r.Conditions.Codes) }
