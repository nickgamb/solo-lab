package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// WorkloadProfile is a group of workloads' assurance rules: what a sign-in
// must prove to reach them, whichever IdP it came through, and which of the
// chain's IdPs may vouch for their users. A rule it leaves unset is its
// IdentityContinuity's assurancePolicy. The assurance gate enforces it at the
// workloads' policy points; requests it can't meet are refused (fail
// closed). Lives beside its IdentityContinuity, owned by whoever owns
// identity risk.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wlp
// +kubebuilder:printcolumn:name=CRITICALITY,type=string,JSONPath=`.spec.criticality`
// +kubebuilder:printcolumn:name=MODE,type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name=MINIMUM,type=string,JSONPath=`.spec.assurance.minimum`
// +kubebuilder:printcolumn:name=PHASE,type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name=SERVING,type=string,JSONPath=`.status.serving`
// +kubebuilder:printcolumn:name=AGE,type=date,JSONPath=`.metadata.creationTimestamp`
type WorkloadProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadProfileSpec   `json:"spec"`
	Status WorkloadProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WorkloadProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadProfile `json:"items"`
}

type WorkloadProfileSpec struct {
	// The IdentityContinuity (in this namespace) whose sign-ins these
	// workloads take.
	// +kubebuilder:validation:MinLength=1
	Continuity string `json:"continuity"`
	// What these workloads say they are, in a sentence.
	// +kubebuilder:validation:MaxLength=512
	Description string `json:"description,omitempty"`
	// Business impact.
	// +kubebuilder:validation:Enum=Critical;High;Standard
	Criticality string `json:"criticality"`
	// What the assurance gate does with these rules. Enforce: a request
	// they refuse is refused. ReportOnly: it is let through, and the gate
	// logs and returns what it would have decided (would-deny), to see a
	// rule's effect before enforcing it. Off: every request is let through.
	// +kubebuilder:validation:Enum=Enforce;ReportOnly;Off
	// +kubebuilder:default=Enforce
	Mode string `json:"mode,omitempty"`
	// What the business owes for them: regulations, policies, contracts.
	// Free-form tags, for reporting.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=64
	Obligations []string `json:"obligations,omitempty"`
	// Who answers for them.
	// +kubebuilder:validation:MaxLength=128
	Owner string `json:"owner,omitempty"`
	// The workloads, by mesh identity: what the Observatory maps their
	// identity dependencies from.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	Workloads []WorkloadRef `json:"workloads,omitempty"`
	// The broker clients users sign in through to reach them.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	Clients []string `json:"clients,omitempty"`
	// The chain's IdPs that may vouch for a session here (by tier name).
	// Unset: the assurancePolicy's.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	AllowedIdPs []string `json:"allowedIdPs,omitempty"`
	// Whether the broker's break-glass accounts (its local tier, platform
	// admins) may reach them. Unset: the assurancePolicy's.
	// +optional
	AllowBreakGlass *bool `json:"allowBreakGlass,omitempty"`
	// Any: a session from an allowed IdP, for as long as it lasts.
	// ActiveIdPOnly: only sessions from the IdP signing people in now; a
	// session from one the chain has moved off is refused until the user
	// signs in again. Unset: the assurancePolicy's.
	// +kubebuilder:validation:Enum=Any;ActiveIdPOnly
	// +optional
	Sessions string `json:"sessions,omitempty"`
	// +optional
	Assurance ProfileAssurance `json:"assurance,omitempty"`
}

type WorkloadRef struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	ServiceAccount string `json:"serviceAccount"`
}

// ProfileAssurance is what a sign-in must prove (NIST SP 800-63B), judged by
// what its upstream IdP asserted (the IdP's tier assurance). Without the
// evidence a requirement needs, the request is refused. Each field unset is
// the assurancePolicy's.
type ProfileAssurance struct {
	// +kubebuilder:validation:Enum=AAL1;AAL2;AAL3
	// +optional
	Minimum string `json:"minimum,omitempty"`
	// The authenticator must resist phishing (FIDO2/WebAuthn, smart card).
	// +optional
	PhishingResistant *bool `json:"phishingResistant,omitempty"`
	// How long ago the user may have authenticated at the IdP (its
	// auth_time), e.g. "12h".
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

type WorkloadProfileStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Available: sign-ins through the IdP serving now can meet the profile,
	// at the profile's preferred IdP. Degraded: they can, on a later IdP in
	// the chain. FailedClosed: they can't; requests are refused until the
	// chain moves to an IdP that can, or the user's IdP steps them up.
	// +kubebuilder:validation:Enum=Available;Degraded;FailedClosed
	Phase string `json:"phase,omitempty"`
	// The IdP signing people in now (the IdentityContinuity's active tier).
	Serving string `json:"serving,omitempty"`
	// The most a sign-in through it can prove.
	ServingLevel string `json:"servingLevel,omitempty"`
	// The chain's IdPs, in order, whose sign-ins can meet the profile.
	EligibleIdPs []string `json:"eligibleIdPs,omitempty"`
	// The broker clients users reach these workloads through, as the broker
	// has them registered.
	// +listType=map
	// +listMapKey=clientID
	Clients []ClientRegistration `json:"clients,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ClientRegistration struct {
	ClientID string `json:"clientID"`
	Found    bool   `json:"found"`
	// Where the broker sends its users back to.
	RedirectURIs []string `json:"redirectURIs,omitempty"`
	// The audiences its access tokens carry (its audience mappers).
	Audiences []string `json:"audiences,omitempty"`
	// Its tokens carry the upstream's assurance (continuity-assurance scope):
	// without it, no request through it can prove more than nothing.
	AssuranceScope bool `json:"assuranceScope"`
}

func init() {
	SchemeBuilder.Register(&WorkloadProfile{}, &WorkloadProfileList{})
}
