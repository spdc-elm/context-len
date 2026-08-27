package policy

import (
	"sync"
	"testing"
)

func TestPolicyDefaultsAndIndependentGates(t *testing.T) {
	if got := Default(); got.RequestGate != GatePass || got.ResponseGate != GatePass {
		t.Fatalf("default = %#v", got)
	}
	p, err := New(GateHold, GatePass)
	if err != nil {
		t.Fatal(err)
	}
	if !p.RequestHeld() || p.ResponseHeld() {
		t.Fatalf("independent gates lost: %#v", p)
	}
	if !p.Equal(Policy{RequestGate: GateHold, ResponseGate: GatePass}) {
		t.Fatal("normalised equality failed")
	}
}

func TestPolicyValidation(t *testing.T) {
	if _, err := New(GateMode("pause"), GatePass); err == nil {
		t.Fatal("invalid request gate accepted")
	}
	if err := (Policy{RequestGate: GatePass, ResponseGate: GateMode("pause")}).Validate(); err == nil {
		t.Fatal("invalid response gate accepted")
	}
	if got := (Policy{}).Normalize(); got != Default() {
		t.Fatalf("zero policy did not normalize to default: %#v", got)
	}
}

func TestStoreSnapshotsPolicyForFutureExchanges(t *testing.T) {
	s := NewStore(Policy{RequestGate: GateHold, ResponseGate: GatePass})
	before := s.Get()
	if err := s.Set(Policy{RequestGate: GatePass, ResponseGate: GateHold}); err != nil {
		t.Fatal(err)
	}
	if before.RequestGate != GateHold || before.ResponseGate != GatePass {
		t.Fatalf("old value changed: %#v", before)
	}
	if got := s.Get(); got.RequestGate != GatePass || got.ResponseGate != GateHold {
		t.Fatalf("new value = %#v", got)
	}
}

func TestStoreConcurrentGetSet(t *testing.T) {
	s := NewStore(Default())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if i%2 == 0 {
					_ = s.Set(Policy{RequestGate: GatePass, ResponseGate: GateHold})
				} else {
					_ = s.Set(Policy{RequestGate: GateHold, ResponseGate: GatePass})
				}
				_ = s.Get()
			}
		}(i)
	}
	wg.Wait()
	got := s.Get()
	if !got.Equal(Policy{RequestGate: GatePass, ResponseGate: GateHold}) && !got.Equal(Policy{RequestGate: GateHold, ResponseGate: GatePass}) {
		t.Fatalf("invalid concurrent result: %#v", got)
	}
}
