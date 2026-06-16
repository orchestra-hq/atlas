package anthropic

// ModelInfo is one entry of GET /v1/models (and the body of GET
// /v1/models/{id}). It follows the Anthropic model object — type, id,
// display_name, created_at — plus context_window, an Atlas extension that
// clients use to size requests to a model's real window (docs/m0-acceptance.md
// context-window handling; SDKs ignore the unknown field).
type ModelInfo struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	CreatedAt     string `json:"created_at"`
	ContextWindow int    `json:"context_window,omitempty"`
}

// NewModelInfo builds a model object with the constant "model" type.
func NewModelInfo(id, displayName, createdAt string, contextWindow int) ModelInfo {
	if displayName == "" {
		displayName = id
	}
	return ModelInfo{
		Type:          "model",
		ID:            id,
		DisplayName:   displayName,
		CreatedAt:     createdAt,
		ContextWindow: contextWindow,
	}
}

// ModelList is the GET /v1/models response: the Anthropic list shape. M0 has no
// pagination — every deployed model and alias fits in one page — so HasMore is
// always false.
type ModelList struct {
	Data    []ModelInfo `json:"data"`
	HasMore bool        `json:"has_more"`
	FirstID *string     `json:"first_id"`
	LastID  *string     `json:"last_id"`
}

// NewModelList wraps data as a single-page list, setting first_id/last_id.
func NewModelList(data []ModelInfo) ModelList {
	list := ModelList{Data: data, HasMore: false}
	if len(data) > 0 {
		list.FirstID = &data[0].ID
		list.LastID = &data[len(data)-1].ID
	}
	return list
}
