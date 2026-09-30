package agent

import "context"

// workspaceScopeSchemas removes scope-planning tools once the application has
// an authoritative scope. Keeping them in the registry is useful for dispatch
// and tests; hiding their schemas prevents the planner from spending another
// step retrying a scope decision the server will reject anyway.
func workspaceScopeSchemas(ctx context.Context, schemas []Tool, reg *Registry) []Tool {
	present, locked := WorkspaceScopeLocked(ctx)
	if !present || !locked {
		return schemas
	}
	filtered := make([]Tool, 0, len(schemas))
	for _, schema := range schemas {
		phase := reg.Phase(schema.Function.Name)
		if phase == ToolPhaseScopePreparation || phase == ToolPhaseScopeCommit {
			continue
		}
		filtered = append(filtered, schema)
	}
	return filtered
}
