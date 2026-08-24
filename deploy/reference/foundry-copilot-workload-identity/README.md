# Copilot CLI with Azure Foundry workload identity

This scaffold runs Copilot CLI against an Azure Foundry/Azure OpenAI deployment
from AKS without a client secret. AKS projects a signed Kubernetes service
account token, Microsoft Entra exchanges it for a short-lived access token, and
Copilot sends that token only to the configured model endpoint.

The managed identity client ID is still required. It identifies the identity
being used, but it is not a credential and does not need secret storage.

## 1. Enable AKS workload identity

```bash
az aks update \
  --resource-group "$AKS_RESOURCE_GROUP" \
  --name "$AKS_CLUSTER_NAME" \
  --enable-oidc-issuer \
  --enable-workload-identity

OIDC_ISSUER="$(az aks show \
  --resource-group "$AKS_RESOURCE_GROUP" \
  --name "$AKS_CLUSTER_NAME" \
  --query oidcIssuerProfile.issuerUrl \
  --output tsv)"
```

## 2. Create and authorize the managed identity

```bash
az identity create \
  --resource-group "$IDENTITY_RESOURCE_GROUP" \
  --name "$IDENTITY_NAME"

CLIENT_ID="$(az identity show \
  --resource-group "$IDENTITY_RESOURCE_GROUP" \
  --name "$IDENTITY_NAME" \
  --query clientId \
  --output tsv)"

PRINCIPAL_ID="$(az identity show \
  --resource-group "$IDENTITY_RESOURCE_GROUP" \
  --name "$IDENTITY_NAME" \
  --query principalId \
  --output tsv)"

az role assignment create \
  --assignee-object-id "$PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "Cognitive Services OpenAI User" \
  --scope "$FOUNDRY_RESOURCE_ID"
```

`FOUNDRY_RESOURCE_ID` is the Azure resource ID of the Foundry/Azure OpenAI
resource, not the deployment URL.

## 3. Trust the Kubernetes service account

```bash
az identity federated-credential create \
  --resource-group "$IDENTITY_RESOURCE_GROUP" \
  --identity-name "$IDENTITY_NAME" \
  --name goobers-foundry-copilot \
  --issuer "$OIDC_ISSUER" \
  --subject system:serviceaccount:goobers-foundry:goobers-foundry-copilot \
  --audiences api://AzureADTokenExchange
```

Replace `CHANGE-ME-CLIENT-ID`, the endpoint, model ID, deployment name, and
container image in the manifests. The image must contain `bash`, `curl`, `jq`,
and `copilot`.

```bash
kubectl apply -k deploy/reference/foundry-copilot-workload-identity
kubectl logs -n goobers-foundry job/foundry-copilot-smoke-test
```

For real Goobers stage pods, copy the service account annotation, set
`serviceAccountName`, and add the `azure.workload.identity/use: "true"` pod
label. Acquire a fresh token before each Copilot process; Entra access tokens
are short-lived.

