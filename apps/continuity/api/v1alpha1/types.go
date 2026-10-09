package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// IdentityContinuity keeps a broker (Keycloak) signing users in through the
// first healthy tier of an ordered chain of upstream IdPs, with the broker's
// own accounts as a break-glass tier. The broker stays the only issuer
// relying parties trust; upstreams only authenticate.
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
	// S&V's standard user profile on the broker: the attributes the
	// directory sync maps every IdP's profile into. Optional.
	Profile *Profile `json:"profile,omitempty"`
	// The scheduled directory sync: the primary IdP's profile into the
	// broker, then the broker's out to every failover IdP. Optional.
	Sync *Sync `json:"sync,omitempty"`
	// The assurance rules every WorkloadProfile following this chain has,
	// unless it sets its own.
	// +kubebuilder:default={}
	AssurancePolicy AssurancePolicy `json:"assurancePolicy,omitempty"`
}

// AssurancePolicy is the global assurance rules: what a sign-in must prove
// to reach a workload, and which IdPs may vouch, where a workload's profile
// doesn't say.
type AssurancePolicy struct {
	// +kubebuilder:validation:Enum=AAL1;AAL2;AAL3
	// +kubebuilder:default=AAL1
	Minimum           string `json:"minimum,omitempty"`
	PhishingResistant bool   `json:"phishingResistant,omitempty"`
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
	// The chain's IdPs that may vouch for a session. Empty: every upstream.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	AllowedIdPs []string `json:"allowedIdPs,omitempty"`
	// The broker's break-glass accounts may reach workloads.
	AllowBreakGlass bool `json:"allowBreakGlass,omitempty"`
	// +kubebuilder:validation:Enum=Any;ActiveIdPOnly
	// +kubebuilder:default=Any
	Sessions string `json:"sessions,omitempty"`
}

// Profile is the broker's standard user profile.
type Profile struct {
	// Attributes beyond the built-in username, email, firstName and lastName,
	// added to the realm's user profile. Users can view but not edit them:
	// their values come from the primary IdP.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	Attributes []ProfileAttribute `json:"attributes,omitempty"`
	// The groups in the shape: the broker keeps them, and each IdP's own
	// groups (or roles) map into them by name (tiers[].groups). Membership
	// always comes from an IdP, never from the broker: at sign-in from the
	// IdP's groups claim, and in the directory sync from the primary.
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`
	Groups []string `json:"groups,omitempty"`
	// The workforce's email domains (e.g. sterling.lab). The directory sync
	// gives the broker an account for each user of the primary IdP whose
	// verified email is under one of them; empty, it creates none.
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	Domains []string `json:"domains,omitempty"`
}

type ProfileAttribute struct {
	// The attribute's name in the broker's user profile.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9_.-]*$`
	// +kubebuilder:validation:MaxLength=64
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	// The value's type, checked by the broker when a value is written:
	// string, integer, number, boolean (true or false), date (yyyy-mm-dd),
	// email or uri.
	// +kubebuilder:validation:Enum=string;integer;number;boolean;date;email;uri
	// +kubebuilder:default=string
	Type string `json:"type,omitempty"`
	// Holds a list of values; otherwise one.
	Multivalued bool `json:"multivalued,omitempty"`
}

// Sync schedules the directory sync. For each employee the broker has: the
// primary IdP's record (the chain's first tier) is read into the broker's
// profile, then the broker's profile is written to each failover IdP,
// creating the user there (with an enrollment email) if the primary has them.
// Passwords and credentials are never read or written, the username is never
// written, and no user is deleted.
// +kubebuilder:validation:XValidation:rule="(has(self.suspend) && self.suspend) || (has(self.schedule) && size(self.schedule) > 0)",message="a schedule, unless suspended"
type Sync struct {
	// Standard cron, in UTC (e.g. "0 2 * * *" daily at 02:00). Empty only
	// while suspended.
	// +optional
	Schedule string `json:"schedule,omitempty"`
	Suspend  bool   `json:"suspend,omitempty"`
	// A Secret with client-id and client-secret of the broker's realm client
	// the sync writes users as (view-users and manage-users; nothing else).
	// +kubebuilder:default={name: "continuity-sync"}
	CredentialsRef LocalRef `json:"credentialsRef,omitempty"`
}

// AttributeMapping pairs a broker profile attribute with the IdP's own
// attribute. The primary IdP's is read into the broker's; a failover's is
// written from it.
type AttributeMapping struct {
	// The broker's profile attribute (built-in or spec.profile.attributes).
	// +kubebuilder:validation:MinLength=1
	Attribute string `json:"attribute"`
	// The attribute in the IdP's user record: a dot path ("name.givenName",
	// "user_metadata.department"), a SCIM extension attribute by its schema
	// URN ("urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department"),
	// or a SCIM filtered value ("phoneNumbers[type eq \"work\"].value").
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`
}

// Directory is where the sync reads and writes an IdP's users.
// +kubebuilder:validation:XValidation:rule="self.type != 'auth0' || has(self.audience)",message="an auth0 directory needs audience (the Management API identifier)"
type Directory struct {
	// scim (SCIM 2.0: Gluu, and most enterprise IdPs), auth0 (Management
	// API v2) or keycloak (admin API, one realm).
	// +kubebuilder:validation:Enum=scim;auth0;keycloak
	Type string `json:"type"`
	// The API's base URL: .../scim/v2, https://<tenant>/api/v2, or
	// https://<host>/admin/realms/<realm>. Plain http only to a cluster
	// Service (the mesh encrypts it), e.g. a Keycloak whose admin API isn't
	// published.
	// +kubebuilder:validation:Pattern=`^(https://|http://[a-z0-9.-]+\.svc(\.cluster\.local)?(:[0-9]+)?(/|$))`
	URL string `json:"url"`
	// A Secret with client-secret (and client-id, unless clientID is set):
	// an OAuth client allowed the client_credentials grant at the tier's
	// token endpoint, allowed to read users and, for a failover, update them.
	CredentialsRef LocalRef `json:"credentialsRef"`
	ClientID       string   `json:"clientID,omitempty"`
	// Scopes to request (e.g. "https://jans.io/scim/users.read
	// https://jans.io/scim/users.write").
	Scopes   []string `json:"scopes,omitempty"`
	Audience string   `json:"audience,omitempty"`
}

type Broker struct {
	Keycloak KeycloakBroker `json:"keycloak"`
}

type KeycloakBroker struct {
	// In-cluster base URL of Keycloak (admin API and token endpoint): a
	// cluster Service, e.g. http://keycloak.sv-identity.svc.
	// +kubebuilder:validation:Pattern=`^https?://[a-z0-9.-]+\.svc(\.cluster\.local)?(:[0-9]+)?/?$`
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
	// This IdP's attributes, paired with the broker's profile for the
	// directory sync. oidc tiers only.
	// +listType=map
	// +listMapKey=attribute
	// +kubebuilder:validation:MaxItems=64
	Attributes []AttributeMapping `json:"attributes,omitempty"`
	// Where the directory sync reads and writes this IdP's users. oidc
	// tiers only.
	Directory *Directory `json:"directory,omitempty"`
	// How this IdP's groups (or roles) map into the shape's groups
	// (spec.profile.groups), by name. oidc tiers only.
	// +optional
	Groups *TierGroups `json:"groups,omitempty"`
	// +kubebuilder:default={}
	FailoverWhen FailoverRules `json:"failoverWhen,omitempty"`
	// What a sign-in through this IdP proves, for workload profiles that
	// require an assurance level. Unset: every sign-in counts as AAL1.
	// +optional
	Assurance *TierAssurance `json:"assurance,omitempty"`
}

// TierGroups is where an IdP says which groups a user is in.
type TierGroups struct {
	// The ID token claim listing the user's groups or roles (e.g. groups, or
	// a namespaced claim such as https://example.com/groups). At sign-in the
	// broker sets the user's shape groups from it, every time.
	// +kubebuilder:validation:MinLength=1
	Claim string `json:"claim"`
}

// TierAssurance maps what an IdP asserts about a sign-in (its acr, or one of
// its amr values, which the broker keeps on the session) to a NIST SP 800-63B
// authenticator assurance level. A sign-in is the highest level any of its
// values maps to; one that asserts none of them is default.
type TierAssurance struct {
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	Levels []AssuranceLevel `json:"levels,omitempty"`
	// The level of a sign-in whose acr and amr match no entry: what this
	// IdP's sign-in policy for S&V's client guarantees on its own (a local
	// tier's only level).
	// +kubebuilder:validation:Enum=AAL1;AAL2;AAL3
	// +kubebuilder:default=AAL1
	Default string `json:"default,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.acr) != has(self.amr)",message="exactly one of acr or amr"
type AssuranceLevel struct {
	// The IdP's acr value, exactly (e.g. "aal2", "phr",
	// "http://schemas.openid.net/pape/policies/2007/06/multi-factor").
	// +kubebuilder:validation:MaxLength=256
	ACR string `json:"acr,omitempty"`
	// One amr value (RFC 8176, e.g. "mfa", "otp", "hwk").
	// +kubebuilder:validation:MaxLength=64
	AMR string `json:"amr,omitempty"`
	// +kubebuilder:validation:Enum=AAL1;AAL2;AAL3
	Level string `json:"level"`
	// The authenticator resists phishing (FIDO2/WebAuthn, smart card): what
	// AAL3 requires, and what a profile can require at any level.
	PhishingResistant bool `json:"phishingResistant,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.clientAuth == 'private_key_jwt' || has(self.clientSecretRef)",message="clientSecretRef is required unless clientAuth is private_key_jwt"
// +kubebuilder:validation:XValidation:rule="self.clientAuth != 'private_key_jwt' || (has(self.clientID) && size(self.clientID) > 0)",message="clientAuth private_key_jwt needs clientID"
type OIDCUpstream struct {
	// Exactly as the upstream publishes it (Auth0's ends in "/").
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`
	// Falls back to the client-id key of clientSecretRef's Secret.
	ClientID string `json:"clientID,omitempty"`
	// How the broker authenticates to the upstream's token endpoint.
	// private_key_jwt (RFC 7523) signs a client assertion with the realm's
	// active key for clientAssertionSigningAlg (a key kept for that alone,
	// published in the realm's JWKS for the upstream to register): no shared
	// secret.
	// +kubebuilder:validation:Enum=client_secret_post;client_secret_basic;private_key_jwt
	// +kubebuilder:default=client_secret_post
	ClientAuth string `json:"clientAuth,omitempty"`
	// The algorithm of the realm key kept for client assertions alone (the
	// token-signing key uses RS256, so it is never used here).
	// +kubebuilder:validation:Enum=PS256
	// +kubebuilder:default=PS256
	ClientAssertionSigningAlg string `json:"clientAssertionSigningAlg,omitempty"`
	// The client secret, for client_secret_post and client_secret_basic. A
	// tier whose Secret or key is missing is NotConfigured: probed, never
	// active.
	ClientSecretRef *SecretKeyRef `json:"clientSecretRef,omitempty"`
	// +kubebuilder:default={openid,email,profile}
	Scopes []string `json:"scopes,omitempty"`
	// Keep the upstream's tokens on each user's broker link, for a client the
	// realm allows to read them (Identity Brokering API v2): S&V's egress has
	// the upstream vouch for its users in Cross App Access. Without
	// offline_access in scopes, the stored refresh token ends with the
	// user's session at the upstream.
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
	Sync        *SyncStatus  `json:"sync,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type BrokerStatus struct {
	// The realm's public issuer, from its discovery document.
	Issuer string `json:"issuer,omitempty"`
	// The broker's clients that sign people in through a browser
	// (authorization code), and where they send people back: the apps that
	// sign in through this broker, whether the edge or the app itself runs
	// the sign-in.
	// +listType=map
	// +listMapKey=clientID
	SignIn []SignInClient `json:"signIn,omitempty"`
}

type SignInClient struct {
	ClientID     string   `json:"clientID"`
	RedirectURIs []string `json:"redirectURIs,omitempty"`
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
	// oidc tiers: S&V's registration at this IdP checked against what the
	// IdP publishes, and whether it accepts the broker's callback. Run every
	// 10 minutes, on every spec change, and when the instance's
	// continuity.lab.solo.io/check-trust annotation changes. Informational:
	// selection only ever follows the probes.
	// +optional
	Trust *TrustStatus `json:"trust,omitempty"`
}

type TrustStatus struct {
	CheckedAt metav1.Time `json:"checkedAt"`
	// The spec generation and check-trust annotation the checks ran for.
	Generation int64  `json:"generation,omitempty"`
	Requested  string `json:"requested,omitempty"`
	// +listType=map
	// +listMapKey=name
	Checks []TrustCheck `json:"checks,omitempty"`
}

// TrustCheck is one property of S&V's registration at an IdP. Unknown: the
// IdP doesn't publish what the check needs (many omit some discovery
// fields), which is not a failure.
type TrustCheck struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Pass;Fail;Unknown
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
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

// SyncStatus records the scheduled sync's last run.
type SyncStatus struct {
	// The CronJob that runs it.
	CronJob     string       `json:"cronJob,omitempty"`
	LastRun     *metav1.Time `json:"lastRun,omitempty"`
	LastSuccess *metav1.Time `json:"lastSuccess,omitempty"`
	// In the last run: users read, users whose broker profile changed,
	// failover accounts written and created, broker accounts provisioned
	// from the primary, and failures.
	Users       int32  `json:"users,omitempty"`
	Provisioned int32  `json:"provisioned,omitempty"`
	Updated     int32  `json:"updated,omitempty"`
	Written     int32  `json:"written,omitempty"`
	Created     int32  `json:"created,omitempty"`
	Failed      int32  `json:"failed,omitempty"`
	Message     string `json:"message,omitempty"`
	// The attribute paths each IdP's directory has, as last read (names
	// only, never values).
	Schemas map[string][]string `json:"schemas,omitempty"`
}
