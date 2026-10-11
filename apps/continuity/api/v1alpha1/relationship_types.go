package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// RelationshipModel is an OpenFGA authorization model, in OpenFGA's DSL: the
// types and relations the firm's entitlements are made of (who advises which
// book, which clients are in it, which agents act for whom). The continuity
// controller keeps one OpenFGA store per model: it writes the model when it
// changes and makes the store's relationships match the Relationships that
// name it, adding what's missing and deleting what no Relationship declares.
// The gateway asks that store on every tool call.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=relm
// +kubebuilder:printcolumn:name=STORE,type=string,JSONPath=`.status.storeID`
// +kubebuilder:printcolumn:name=MODEL,type=string,JSONPath=`.status.modelID`
// +kubebuilder:printcolumn:name=RELATIONSHIPS,type=integer,JSONPath=`.status.relationships`
// +kubebuilder:printcolumn:name=SYNCED,type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name=AGE,type=date,JSONPath=`.metadata.creationTimestamp`
type RelationshipModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RelationshipModelSpec   `json:"spec"`
	Status RelationshipModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type RelationshipModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RelationshipModel `json:"items"`
}

type RelationshipModelSpec struct {
	// What the model is for, in a sentence.
	// +kubebuilder:validation:MaxLength=512
	Description string `json:"description,omitempty"`
	// The model, in OpenFGA's DSL (schema 1.1).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	Model string `json:"model"`
}

type RelationshipModelStatus struct {
	// The OpenFGA store the model's relationships live in.
	StoreID string `json:"storeID,omitempty"`
	// The authorization model last written to it.
	ModelID string `json:"modelID,omitempty"`
	// How many relationships the store holds for it.
	Relationships int32 `json:"relationships,omitempty"`
	// When the store was last made to match.
	SyncedAt *metav1.Time `json:"syncedAt,omitempty"`
	// Synced: the store holds the model and exactly the declared
	// relationships; False names what OpenFGA refused.
	// +listType=map
	// +listMapKey=type
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// Relationship is one fact in a RelationshipModel's store, as OpenFGA writes
// it: a user (or a set of users) has a relation to an object. "Bob advises
// book sv-bob" is user user:bob@sterling.lab, relation advisor, object
// book:sv-bob. Group membership is not declared here: the gateway takes it
// from the verified token, so the IdP stays its source.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=rel
// +kubebuilder:printcolumn:name=USER,type=string,JSONPath=`.spec.user`
// +kubebuilder:printcolumn:name=RELATION,type=string,JSONPath=`.spec.relation`
// +kubebuilder:printcolumn:name=OBJECT,type=string,JSONPath=`.spec.object`
// +kubebuilder:printcolumn:name=MODEL,type=string,JSONPath=`.spec.model`
type Relationship struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RelationshipSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type RelationshipList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Relationship `json:"items"`
}

type RelationshipSpec struct {
	// The RelationshipModel (in this namespace) whose store holds it.
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// Who: type:id, or a set of users as type:id#relation
	// (group:advisors#member).
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*:[^\s#:]+(#[a-z_][a-z0-9_]*)?$`
	// +kubebuilder:validation:MaxLength=256
	User string `json:"user"`
	// The relation, as the model defines it on the object's type.
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=50
	Relation string `json:"relation"`
	// What: type:id.
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*:[^\s#:]+$`
	// +kubebuilder:validation:MaxLength=256
	Object string `json:"object"`
}

func init() {
	SchemeBuilder.Register(&RelationshipModel{}, &RelationshipModelList{}, &Relationship{}, &RelationshipList{})
}
