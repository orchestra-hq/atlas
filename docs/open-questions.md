# Open questions

Decisions that need the project owner's call (or at least sign-off). When a question is resolved, the outcome is recorded in the doc (or ADR) that owns it and the question is deleted here — git history keeps the trail; this file only ever lists what is actually open.

_None open._ The last one — how workers get engine runtimes — is settled: M0 ships managed runtimes only (downloaded prebuilt llama.cpp binaries, a `uv`-managed vLLM venv), with the container path arriving at M1 behind the same provisioner interface. Recorded in [m0-build-plan.md](m0-build-plan.md#engine-runtime-provisioning) and implemented for llama.cpp in phase 2.
