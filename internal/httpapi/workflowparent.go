package httpapi

// WorkflowParentPrincipalIssuer is independent of ordinary pods and children.
const WorkflowParentPrincipalIssuer = "goobers/workflow-parent"

// WorkflowParentPrincipal binds one physical parent execution contract.
type WorkflowParentPrincipal struct{ ContractDigest string }
