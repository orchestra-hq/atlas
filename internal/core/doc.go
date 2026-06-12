// Package core holds Atlas's internal representation of conversations:
// messages, content blocks, tools, thinking, and stop reasons. The wire
// formats under internal/api translate to and from these types so that
// engine adapters and the gateway never depend on a vendor's JSON shape.
//
// Populated from phase 2 of docs/m0-build-plan.md.
package core
