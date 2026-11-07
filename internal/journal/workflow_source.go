package journal

// WorkflowSourceProvenance identifies the exact repository slice used to load
// a workflow configuration generation.
type WorkflowSourceProvenance struct {
	URL    string `json:"url"`
	Dir    string `json:"dir"`
	Commit string `json:"commit"`
}
