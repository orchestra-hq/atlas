package modelmeta

import (
	"context"
	"fmt"
)

// inspectGGUF reads a GGUF file's header to derive capabilities without
// downloading the weights. Implemented in M8 Phase 1b; until then a .gguf target
// reports a clear "not yet" rather than mis-routing to the transformers path.
func inspectGGUF(_ context.Context, repo string, _ Options) (Result, error) {
	return Result{}, fmt.Errorf("modelmeta: GGUF inspection for %s is not implemented yet (M8 Phase 1b)", repo)
}
