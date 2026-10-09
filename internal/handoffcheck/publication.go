package handoffcheck

import "context"

type publicationSchemasContextKey struct{}

// WithPublicationSchemas stores the producing stage's own schema-bound
// application/json artifact slots (slot name -> compiled schema) on ctx, so the
// trusted harness can enforce them at publish_output time and again at the
// completion boundary. It is runner-owned plumbing, never agent-visible input.
func WithPublicationSchemas(ctx context.Context, schemas map[string]*Schema) context.Context {
	if len(schemas) == 0 {
		return ctx
	}
	copied := make(map[string]*Schema, len(schemas))
	for slot, schema := range schemas {
		copied[slot] = schema
	}
	return context.WithValue(ctx, publicationSchemasContextKey{}, copied)
}

// PublicationSchemasFromContext returns the schemas stored by
// WithPublicationSchemas, or nil when the stage declares none.
func PublicationSchemasFromContext(ctx context.Context) map[string]*Schema {
	if ctx == nil {
		return nil
	}
	schemas, _ := ctx.Value(publicationSchemasContextKey{}).(map[string]*Schema)
	return schemas
}
