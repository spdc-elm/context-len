// Package policy defines the independently-controlled request and response
// interception gates used by an exchange.  A Policy value is deliberately
// small and serialisable: registries snapshot it when an exchange is created,
// so changing the global policy never changes an exchange already in flight.
package policy

import (
	"errors"
	"fmt"
	"sync"
)

// GateMode controls whether a side of an exchange is forwarded immediately or
// held for an explicit operator command.
type GateMode string

const (
	GatePass GateMode = "pass"
	GateHold GateMode = "hold"

	// Short aliases make policy literals pleasant while retaining the explicit
	// Gate* names for callers that want to avoid ambiguity.
	Pass GateMode = GatePass
	Hold GateMode = GateHold
)

// Policy is the global/default gate policy.  The two fields are independent;
// a request can be held while a response passes, and vice versa.
type Policy struct {
	RequestGate  GateMode `json:"request_gate"`
	ResponseGate GateMode `json:"response_gate"`
}

// Default returns the safe bypass policy used when no policy is configured.
func Default() Policy {
	return Policy{RequestGate: GatePass, ResponseGate: GatePass}
}

// New validates and returns a policy.  It is useful at configuration edges
// where accepting an invalid gate would otherwise fail much later at runtime.
func New(requestGate, responseGate GateMode) (Policy, error) {
	p := Policy{RequestGate: requestGate, ResponseGate: responseGate}.Normalize()
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Normalize fills zero-valued fields with bypass.  This permits decoding old
// or partial configuration while keeping an explicit hold value intact.
func (p Policy) Normalize() Policy {
	if p.RequestGate == "" {
		p.RequestGate = GatePass
	}
	if p.ResponseGate == "" {
		p.ResponseGate = GatePass
	}
	return p
}

// Validate reports invalid gate values.  Zero values are accepted because a
// zero Policy is interpreted as the default bypass policy by Normalize.
func (p Policy) Validate() error {
	if p.RequestGate != "" && p.RequestGate != GatePass && p.RequestGate != GateHold {
		return fmt.Errorf("policy: invalid request_gate %q", p.RequestGate)
	}
	if p.ResponseGate != "" && p.ResponseGate != GatePass && p.ResponseGate != GateHold {
		return fmt.Errorf("policy: invalid response_gate %q", p.ResponseGate)
	}
	return nil
}

// IsZero reports whether neither gate was specified.
func (p Policy) IsZero() bool { return p.RequestGate == "" && p.ResponseGate == "" }

// Equal compares the normalised effective values of two policies.
func (p Policy) Equal(other Policy) bool {
	p, other = p.Normalize(), other.Normalize()
	return p == other
}

var ErrInvalidPolicy = errors.New("policy: invalid policy")

// Store is a concurrency-safe holder for the policy applied to new
// exchanges.  Existing exchanges retain the value returned by Get at create
// time and are not mutated by Set.
type Store struct {
	mu      sync.RWMutex
	current Policy
}

// NewStore creates a policy store.  A zero policy selects Default.  Invalid
// values are also replaced by Default; use New/Validate at a configuration
// boundary when an error is preferred.
func NewStore(initial Policy) *Store {
	if initial.IsZero() {
		initial = Default()
	}
	if err := initial.Validate(); err != nil {
		initial = Default()
	}
	return &Store{current: initial.Normalize()}
}

// Get returns the effective policy for a new exchange.
func (s *Store) Get() Policy {
	if s == nil {
		return Default()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Set validates and atomically replaces the policy used by future exchanges.
func (s *Store) Set(next Policy) error {
	if s == nil {
		return errors.New("policy: nil store")
	}
	if err := next.Validate(); err != nil {
		return errors.Join(ErrInvalidPolicy, err)
	}
	next = next.Normalize()
	s.mu.Lock()
	s.current = next
	s.mu.Unlock()
	return nil
}

// MustSet is a convenience for process setup where a bad static policy is a
// programmer/configuration error.  It panics rather than silently changing
// the requested interception behaviour.
func (s *Store) MustSet(next Policy) {
	if err := s.Set(next); err != nil {
		panic(err)
	}
}

// RequestHeld and ResponseHeld are readable helpers for callers implementing
// policy checks without comparing string literals.
func (p Policy) RequestHeld() bool  { return p.Normalize().RequestGate == GateHold }
func (p Policy) ResponseHeld() bool { return p.Normalize().ResponseGate == GateHold }
