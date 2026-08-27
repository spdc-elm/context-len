// Package inspection contains projection-only inspectors for captured HTTP bodies.
//
// Inspection always operates on a private copy of the supplied bytes.  The
// resulting values are deliberately not suitable as transport input: the wire
// artifact remains the authority for forwarding.  In particular, JSON and SSE
// inspectors retain raw spans for every node/event so callers can display a
// useful projection without having to reconstruct the body.
package inspection
