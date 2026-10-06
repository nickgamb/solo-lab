package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
	"github.com/nickgamb/solo-lab/apps/continuity/internal/keycloak"
)

// profileEvery: how often the profile is put back unchanged in the spec
// (a realm re-import, a hand edit); a spec change applies on the next pass.
const profileEvery = time.Minute

type profileMark struct {
	hash    string
	at      time.Time
	invalid error // mappings skipped as invalid: reported on every pass
}

const noSchedule = "0 0 1 1 *"

// ProfileScope is the client scope that carries the profile in tokens.
func ProfileScope(ic *v1.IdentityContinuity) string { return ic.Name + "-profile" }

// SyncJobName is the CronJob that runs the scheduled profile sync.
func SyncJobName(ic *v1.IdentityContinuity) string { return ic.Name + "-profile-sync" }

func profileAttrs(ic *v1.IdentityContinuity) []keycloak.Attr {
	if ic.Spec.Profile == nil {
		return nil
	}
	out := make([]keycloak.Attr, 0, len(ic.Spec.Profile.Attributes))
	for _, a := range ic.Spec.Profile.Attributes {
		out = append(out, keycloak.Attr{Name: a.Name, DisplayName: a.DisplayName, Multivalued: a.Multivalued})
	}
	return out
}

// Writable are the attributes a mapping may fill: the profile's and the
// built-in names (identity keys only at first sign-in).
func Writable(ic *v1.IdentityContinuity) []string {
	out := slices.Clone(keycloak.Builtin)
	for _, a := range profileAttrs(ic) {
		out = append(out, a.Name)
	}
	return out
}

// reconcileProfile keeps the realm's user profile, each tier's claim mappers
// and the profile client scope matching the spec. idps are the tiers whose
// IdP exists in Keycloak now.
func (r *Reconciler) reconcileProfile(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client, idps map[string]bool) error {
	owner := client.ObjectKeyFromObject(ic).String()
	b, _ := json.Marshal([]any{ic.Spec.Profile, ic.Spec.Tiers, idps})
	sum := sha256.Sum256(b)
	hash := fmt.Sprintf("%x", sum[:8])
	r.mu.Lock()
	last := r.profiles[client.ObjectKeyFromObject(ic)]
	r.mu.Unlock()
	if last.hash == hash && time.Since(last.at) < profileEvery {
		return last.invalid
	}

	var errs, invalid []error
	attrs := profileAttrs(ic)
	if _, err := kc.EnsureProfile(ctx, owner, attrs); err != nil {
		return err // mappers into attributes the profile lacks would be dropped
	}
	writable := Writable(ic)
	first := true
	for _, t := range ic.Spec.Tiers {
		if t.Type != "oidc" {
			continue
		}
		follow := first // the chain's first upstream wins at sign-in
		first = false
		if !idps[t.Name] {
			continue
		}
		var ms []keycloak.ClaimMapper
		for _, m := range t.Claims {
			if !slices.Contains(writable, m.Attribute) {
				invalid = append(invalid, fmt.Errorf("tier %s: claim %s maps to %q, not in the profile", t.Name, m.Claim, m.Attribute))
				continue
			}
			ms = append(ms, keycloak.ClaimMapper{Claim: m.Claim, Attribute: m.Attribute})
		}
		if _, err := kc.SyncClaimMappers(ctx, t.Name, ms, follow); err != nil {
			errs = append(errs, fmt.Errorf("tier %s mappers: %w", t.Name, err))
		}
	}
	var clients []string
	if ic.Spec.Profile != nil {
		clients = ic.Spec.Profile.TokenClients
	}
	if len(attrs) == 0 || len(clients) == 0 {
		errs = append(errs, kc.DeleteProfileScope(ctx, owner, ProfileScope(ic)))
	} else {
		errs = append(errs, kc.EnsureProfileScope(ctx, owner, ProfileScope(ic), attrs, clients))
	}
	inv := errors.Join(invalid...)
	if err := errors.Join(errs...); err != nil {
		return errors.Join(err, inv) // Keycloak errors: retried on the next pass
	}
	r.mu.Lock()
	r.profiles[client.ObjectKeyFromObject(ic)] = profileMark{hash: hash, at: time.Now(), invalid: inv}
	r.mu.Unlock()
	return inv
}

// cleanupProfile removes this instance's profile attributes and scope (the
// claim mappers go with the IdPs). Users keep their values.
func cleanupProfile(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client) error {
	owner := client.ObjectKeyFromObject(ic).String()
	_, err := kc.EnsureProfile(ctx, owner, nil)
	return errors.Join(err, kc.DeleteProfileScope(ctx, owner, ProfileScope(ic)))
}

func hasDirectory(ic *v1.IdentityContinuity) bool {
	return slices.ContainsFunc(ic.Spec.Tiers, func(t v1.Tier) bool { return t.Directory != nil })
}

// reconcileSync keeps the profile-sync CronJob: present while spec.sync or
// any tier's directory is (a directory can be tested before a schedule is
// set), running on the schedule only with spec.sync.
func (r *Reconciler) reconcileSync(ctx context.Context, ic *v1.IdentityContinuity) error {
	name := SyncJobName(ic)
	if ic.Spec.Sync == nil && !hasDirectory(ic) {
		ic.Status.Sync = nil
		var cur batchv1.CronJob
		err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ic.Namespace, Name: name}, &cur)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return err
		case cur.Labels[LabelInstance] != instanceOf(ic):
			return nil
		}
		return client.IgnoreNotFound(r.Delete(ctx, &cur))
	}
	if r.SyncImage == "" {
		return errors.New("a sync or directory is set but the controller has no --sync-image")
	}
	var cur batchv1.CronJob
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ic.Namespace, Name: name}, &cur)
	if err == nil && cur.Labels[LabelInstance] != instanceOf(ic) {
		return fmt.Errorf("CronJob %s belongs to %q, not this instance", name, cur.Labels[LabelInstance])
	} else if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	u, err := toUnstructured(r.syncCronJob(ic))
	if err != nil {
		return err
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("continuity-controller"), client.ForceOwnership); err != nil {
		return err
	}
	if ic.Status.Sync == nil {
		ic.Status.Sync = &v1.SyncStatus{}
	}
	ic.Status.Sync.CronJob = name
	return nil
}

func (r *Reconciler) syncCronJob(ic *v1.IdentityContinuity) *batchv1.CronJob {
	labels := map[string]string{"app": "continuity-sync", LabelInstance: instanceOf(ic)}
	args := []string{"sync", "--instance=" + ic.Namespace + "/" + ic.Name}
	var mounts []corev1.VolumeMount
	var vols []corev1.Volume
	if r.SyncCAConfigMap != "" {
		args = append(args, "--ca-file=/etc/lab-ca/ca.crt")
		mounts = []corev1.VolumeMount{{Name: "lab-ca", MountPath: "/etc/lab-ca", ReadOnly: true}}
		vols = []corev1.Volume{{Name: "lab-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: r.SyncCAConfigMap}}}}}
	}
	t, f := true, false
	uid := int64(65532)
	// without spec.sync the CronJob only carries the job template (for
	// "run now" and directory tests): suspended, on a placeholder schedule
	schedule, suspend := noSchedule, true
	if ic.Spec.Sync != nil {
		schedule, suspend = ic.Spec.Sync.Schedule, ic.Spec.Sync.Suspend
	}
	cj := &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: SyncJobName(ic), Namespace: ic.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "IdentityContinuity",
				Name: ic.Name, UID: ic.UID, Controller: &t, BlockOwnerDeletion: &t}}},
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			TimeZone:                   ptr("Etc/UTC"),
			Suspend:                    &suspend,
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			StartingDeadlineSeconds:    ptr(int64(3600)),
			SuccessfulJobsHistoryLimit: ptr(int32(3)),
			FailedJobsHistoryLimit:     ptr(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit:            ptr(int32(0)),
					ActiveDeadlineSeconds:   ptr(int64(3600)),
					TTLSecondsAfterFinished: ptr(int32(7 * 24 * 3600)),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							ServiceAccountName: "continuity-sync",
							RestartPolicy:      corev1.RestartPolicyNever,
							SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &t, RunAsUser: &uid,
								SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
							Containers: []corev1.Container{{
								Name: "sync", Image: r.SyncImage, Args: args, VolumeMounts: mounts,
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
									Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
								},
								SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &f, ReadOnlyRootFilesystem: &t,
									Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
							}},
							Volumes: vols,
						},
					},
				},
			},
		},
	}
	return cj
}

func toUnstructured(o runtime.Object) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "spec", "jobTemplate", "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "jobTemplate", "spec", "template", "metadata", "creationTimestamp")
	return u, nil
}

func ptr[T any](v T) *T { return &v }
