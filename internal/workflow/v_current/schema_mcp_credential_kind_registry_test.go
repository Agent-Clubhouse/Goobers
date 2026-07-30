package vcurrent

func init() {
	constBackedEnums = append(constBackedEnums, enumRule{
		schema: "goober.schema.json",
		path:   "$defs/mcpCredentialRef/properties/kind/enum",
		source: "api/v1alpha1.MCPCredentialKind",
		want:   goConsts("api/v1alpha1/goober_types.go", "MCPCredentialKind"),
	})
}
