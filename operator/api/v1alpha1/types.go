package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CodewireRelaySpec defines the desired state of a Codewire Relay instance.
type CodewireRelaySpec struct {
	// BaseURL is the public URL of the relay (e.g. https://acme.relay.codewire.sh).
	BaseURL string `json:"baseURL"`

	// AuthMode is the authentication mode: "token" or "none".
	// +kubebuilder:default=token
	AuthMode string `json:"authMode,omitempty"`

	// AuthToken is the shared auth token. Auto-generated if empty.
	AuthToken string `json:"authToken,omitempty"`

	// SSHListen is the SSH gateway listen address (default :2222).
	// +kubebuilder:default=":2222"
	SSHListen string `json:"sshListen,omitempty"`

	// Persistence configures the PVC for relay data.
	Persistence PersistenceSpec `json:"persistence,omitempty"`

	// Ingress configures the Ingress resource for HTTPS API access.
	Ingress *IngressSpec `json:"ingress,omitempty"`

	// SSH configures the SSH gateway service.
	SSH SSHSpec `json:"ssh,omitempty"`

	// Resources defines compute resources for the relay pod.
	Resources *ResourceSpec `json:"resources,omitempty"`

	// CredentialInjection configures injection of relay credentials into another namespace.
	CredentialInjection *CredentialInjectionSpec `json:"credentialInjection,omitempty"`

	// DNS configures automatic DNS record management.
	DNS *DNSSpec `json:"dns,omitempty"`

	// Image overrides the relay container image.
	Image *ImageSpec `json:"image,omitempty"`

	// OIDC configures OIDC authentication for the relay.
	// +optional
	OIDC *OIDCSpec `json:"oidc,omitempty"`
}

type PersistenceSpec struct {
	// Size of the PVC.
	// +kubebuilder:default="1Gi"
	Size string `json:"size,omitempty"`

	// StorageClass for the PVC.
	StorageClass string `json:"storageClass,omitempty"`
}

type IngressSpec struct {
	// ClassName is the IngressClass to use.
	ClassName string `json:"className,omitempty"`

	// Annotations for the Ingress.
	Annotations map[string]string `json:"annotations,omitempty"`
}

type SSHSpec struct {
	// Service configures the SSH gateway Kubernetes Service.
	Service SSHServiceSpec `json:"service,omitempty"`
}

type SSHServiceSpec struct {
	// Type of the Service (LoadBalancer, NodePort, ClusterIP).
	// +kubebuilder:default=LoadBalancer
	Type string `json:"type,omitempty"`

	// Annotations for the Service.
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ResourceSpec struct {
	Requests ResourceValues `json:"requests,omitempty"`
	Limits   ResourceValues `json:"limits,omitempty"`
}

type ResourceValues struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type CredentialInjectionSpec struct {
	// TargetNamespace is the namespace to inject credentials into.
	TargetNamespace string `json:"targetNamespace"`

	// SecretName is the name of the Secret to create in the target namespace.
	// +kubebuilder:default=codewire-relay-creds
	SecretName string `json:"secretName,omitempty"`
}

type DNSSpec struct {
	// Provider is the DNS provider (currently only "cloudflare").
	Provider string `json:"provider"`

	// ZoneID is the DNS zone ID.
	ZoneID string `json:"zoneID"`

	// APITokenSecretRef references a Secret containing the DNS provider API token.
	APITokenSecretRef SecretKeyRef `json:"apiTokenSecretRef"`
}

type SecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`

	// Key within the Secret.
	Key string `json:"key"`
}

type ImageSpec struct {
	// Repository is the container image repository.
	Repository string `json:"repository,omitempty"`

	// Tag is the container image tag.
	Tag string `json:"tag,omitempty"`
}

type OIDCSpec struct {
	// Issuer is the OIDC provider issuer URL (e.g. https://auth.codewire.sh).
	Issuer string `json:"issuer"`

	// ClientID is the registered OIDC client ID.
	ClientID string `json:"clientID"`

	// ClientSecretRef references a Secret containing the OIDC client secret.
	ClientSecretRef SecretKeyRef `json:"clientSecretRef"`

	// AllowedGroups restricts access to members of these groups. Empty = any authenticated user.
	// +optional
	AllowedGroups []string `json:"allowedGroups,omitempty"`
}

// CodewireRelayStatus defines the observed state of a Codewire Relay instance.
type CodewireRelayStatus struct {
	// Phase is the current lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Failed
	Phase string `json:"phase,omitempty"`

	// SSHEndpoint is the external SSH endpoint (ip:port).
	SSHEndpoint string `json:"sshEndpoint,omitempty"`

	// RelayURL is the public relay URL.
	RelayURL string `json:"relayURL,omitempty"`

	// ConnectedNodes is the number of currently connected nodes.
	ConnectedNodes int32 `json:"connectedNodes,omitempty"`

	// Conditions represent the latest available observations.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.relayURL`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.connectedNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CodewireRelay is the Schema for the codewirerelays API.
type CodewireRelay struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CodewireRelaySpec   `json:"spec,omitempty"`
	Status CodewireRelayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CodewireRelayList contains a list of CodewireRelay.
type CodewireRelayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CodewireRelay `json:"items"`
}
