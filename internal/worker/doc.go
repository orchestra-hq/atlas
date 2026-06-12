// Package worker runs on each compute machine: hardware detection, engine
// supervision, and request execution. Workers dial out to the control
// plane and never listen for it (ADR-0003).
//
// Populated from phase 2 of docs/m0-build-plan.md.
package worker
