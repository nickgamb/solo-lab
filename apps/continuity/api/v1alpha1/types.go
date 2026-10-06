package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IdentityContinuity keeps a broker (Keycloak) signing users in through the
// first healthy tier of an ordered chain of upstream IdPs, with the broker's
// own accounts as a tier. The broker stays the only issuer relying parties
// trust; upstreams only authenticate.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=idc
// +kubebuilder:printcolumn:name=ACTIVE,type=string,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name=READY,type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name=AGE,type=date,JSONPath=`.metadata.creationTimestamp`
type IdentityContinuity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IdentityContinuitySpec   `json:"spec"`
	Status IdentityContinuityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IdentityContinuityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdentityContinuity `json:"items"`
}

type IdentityContinuitySpec struct {
	Broker Broker `json:"broker"`
	// Egress, when set, sends back-channel calls to upstreams outside
	// internalDomains through an egress waypoint (one ServiceEntry per tier).
	// +optional
	Egress *Egress `json:"egress,omitempty"`
	// Ordered: the first eligible, healthy tier is active.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Tiers []Tier `json:"tiers"`
	// +kubebuilder:default={}
	Health Health `json:"health,omitempty"`
	// Automatic moves back up the chain as soon as a higher tier is healthy
	// again; Manual stays on the current tier until it fails.
	// +kubebuilder:validation:Enum=Automatic;Manual
	// +kubebuilder:default=Automatic
	Failback string `json:"failback,omitempty"`
}

type Broker struct {
	Keycloak KeycloakBroker `json:"keycloak"`
}

type KeycloakBroker struct {
	// In-cluster base URL of Keycloak (admin API and token endpoint).
	URL   string `json:"url"`
	Realm string `json:"realm"`
	// Secret with client-id and client-secret of a service-account client
	// holding realm-management manage-identity-providers and manage-realm.
	CredentialsRef LocalRef `json:"credentialsRef"`
	// Browser flow whose Identity Provider Redirector points at the active tier.
	// +kubebuilder:default=continuity-browser
	BrowserFlow string `json:"browserFlow,omitempty"`
	// First-broker-login flow for every upstream (links to existing users).
	// +kubebuilder:default=continuity-first-broker-login
	FirstBrokerLoginFlow string `json:"firstBrokerLoginFlow,omitempty"`
}

type Egress struct {
	Namespace string `json:"namespace"`
	Waypoint  string `json:"waypoint"`
	// Hosts under these domains are in-cluster names and need no ServiceEntry.
	// +kubebuilder:default={lab,svc,cluster.local}
	InternalDomains []string `json:"internalDomains,omitempty"`
	// Other namespaces whose workloads call the upstreams on the broker's
	// behalf (e.g. the gateway that has an upstream vouch for its users):
	// the ServiceEntries are exported to them too, so their calls leave
	// through the same waypoint and a partition cuts them as well.
	ExportTo []string `json:"exportTo,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.type != 'oidc' || has(self.oidc)",message="oidc tiers need spec.oidc"
type Tier struct {
	// Also the Keycloak identity provider alias.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// +kubebuilder:validation:Enum=oidc;local
	Type string `json:"type"`
	// Disabled tiers are never active (a local one included) and their
	// Keycloak IdP is disabled.
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`
	// Drained tiers are taken out of rotation but keep working for sessions
	// already in flight.
	Drain bool `json:"drain,omitempty"`
	// +optional
	OIDC *OIDCUpstream `json:"oidc,omitempty"`
	// +kubebuilder:default={}
	FailoverWhen FailoverRules `json:"failoverWhen,omitempty"`
}

type OIDCUpstream struct {
	// Exactly as the upstream publishes it (Auth0's ends in "/").
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`
	// Falls back to the client-id key of clientSecretRef's Secret.
	ClientID string `json:"clientID,omitempty"`
	// A tier whose Secret or key is missing is NotConfigured: probed, never active.
	ClientSecretRef SecretKeyRef `json:"clientSecretRef"`
	// +kubebuilder:default={openid,email,profile}
	Scopes []string `json:"scopes,omitempty"`
	// Keep the upstream's tokens on each user's broker link, readable by the
	// user's own tokens (role broker/read-token): the gateway then uses the
	// upstream as the OpenID Provider for Cross App Access. Add
	// offline_access to scopes for a refresh token.
	StoreTokens bool `json:"storeTokens,omitempty"`
}

// FailoverRules: which probe failures count against a tier.
type FailoverRules struct {
	// +kubebuilder:default=true
	Unreachable *bool `json:"unreachable,omitempty"`
	// +kubebuilder:default=true
	ServerError *bool `json:"serverError,omitempty"`
	// +kubebuilder:default=true
	InvalidDiscovery *bool `json:"invalidDiscovery,omitempty"`
	// Must be below health.timeoutSeconds: a slower answer times out
	// (Unreachable) before its latency is known. Condition RulesEffective
	// is False while one isn't.
	// +kubebuilder:validation:Minimum=1
	// +optional
	LatencyAboveMs *int32 `json:"latencyAboveMs,omitempty"`
}

type Health struct {
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	UnhealthyThreshold int32 `json:"unhealthyThreshold,omitempty"`
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	HealthyThreshold int32 `json:"healthyThreshold,omitempty"`
}

type LocalRef struct {
	Name string `json:"name"`
}

type SecretKeyRef struct {
	Name string `json:"name"`
	// +kubebuilder:default=client-secret
	Key string `json:"key,omitempty"`
}

type IdentityContinuityStatus struct {
	ObservedGeneration int64         `json:"observedGeneration,omitempty"`
	Broker             *BrokerStatus `json:"broker,omitempty"`
	// The tier logins go to now (the one Keycloak's redirector points at, or
	// the local tier when it points nowhere).
	Active      string       `json:"active,omitempty"`
	ActiveSince *metav1.Time `json:"activeSince,omitempty"`
	// Where this instance's ServiceEntries are, so they are removed when
	// spec.egress changes or goes away.
	EgressNamespace string `json:"egressNamespace,omitempty"`
	// +listType=map
	// +listMapKey=name
	Tiers []TierStatus `json:"tiers,omitempty"`
	// Newest last, at most 20.
	Transitions []Transition `json:"transitions,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type BrokerStatus struct {
	// The realm's public issuer, from its discovery document.
	Issuer string `json:"issuer,omitempty"`
}

type TierStatus struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// oidc tiers: the callback the upstream client must allow,
	// <broker issuer>/broker/<name>/endpoint.
	RedirectURI string `json:"redirectURI,omitempty"`
	// Credentials present (always true for local tiers).
	Configured bool `json:"configured"`
	Healthy    bool `json:"healthy"`
	// A DENY AuthorizationPolicy labelled continuity.lab.solo.io/tier=<name>
	// targets the tier's ServiceEntry in the egress namespace. Informational:
	// selection only ever follows the probes.
	Partitioned          bool         `json:"partitioned"`
	LatencyMs            int64        `json:"latencyMs,omitempty"`
	LastProbe            *metav1.Time `json:"lastProbe,omitempty"`
	Reason               string       `json:"reason,omitempty"`
	Message              string       `json:"message,omitempty"`
	ConsecutiveFailures  int32        `json:"consecutiveFailures,omitempty"`
	ConsecutiveSuccesses int32        `json:"consecutiveSuccesses,omitempty"`
}

type Transition struct {
	Time   metav1.Time `json:"time"`
	From   string      `json:"from,omitempty"`
	To     string      `json:"to"`
	Reason string      `json:"reason"`
}

func init() {
	SchemeBuilder.Register(&IdentityContinuity{}, &IdentityContinuityList{})
}
