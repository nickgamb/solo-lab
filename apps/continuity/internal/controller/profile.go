package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// profileEvery: how often the profile is put back while the spec is
// unchanged (a realm re-import, a hand edit); a spec change applies on the
// next pass.
const profileEvery = time.Minute

type profileMark struct {
	hash    string
	at      time.Time
	invalid error // mappings skipped as invalid: reported on every pass
}

const noSchedule = "0 0 1 1 *"

// SyncJobName is the CronJob that runs the scheduled profile sync.
func SyncJobName(ic *v1.IdentityContinuity) string { return ic.Name + "-profile-sync" }

func profileAttrs(ic *v1.IdentityContinuity) []keycloak.Attr {
	if ic.Spec.Profile == nil {
		return nil
	}
	out := make([]keycloak.Attr, 0, len(ic.Spec.Profile.Attributes))
	for _, a := range ic.Spec.Profile.Attributes {
		out = append(out, keycloak.Attr{Name: a.Name, DisplayName: a.DisplayName, Type: a.Type, Multivalued: a.Multivalued})
	}
	return out
}

// Writable are the broker attributes a mapping may name: the profile's and
// the built-in ones. The sync never writes the username (keycloak.NeverSynced).
func Writable(ic *v1.IdentityContinuity) []string {
	out := slices.Clone(keycloak.Builtin)
	for _, a := range profileAttrs(ic) {
		out = append(out, a.Name)
	}
	return out
}

// Lists are the profile attributes that hold a list of values.
func Lists(ic *v1.IdentityContinuity) []string {
	var out []string
	for _, a := range profileAttrs(ic) {
		if a.Multivalued {
			out = append(out, a.Name)
		}
	}
	return out
}

// reconcileProfile keeps the realm's user profile matching spec.profile and
// reports attribute mappings that name an attribute the profile lacks.
func (r *Reconciler) reconcileProfile(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client) error {
	owner := client.ObjectKeyFromObject(ic).String()
	b, _ := json.Marshal([]any{ic.Spec.Profile, ic.Spec.Tiers})
	sum := sha256.Sum256(b)
	hash := fmt.Sprintf("%x", sum[:8])
	r.mu.Lock()
	last := r.profiles[client.ObjectKeyFromObject(ic)]
	r.mu.Unlock()
	if last.hash == hash && time.Since(last.at) < profileEvery {
		return last.invalid
	}
	if _, err := kc.EnsureProfile(ctx, owner, profileAttrs(ic)); err != nil {
		return err // retried on the next pass
	}
	var invalid []error
	writable := Writable(ic)
	for _, t := range ic.Spec.Tiers {
		for _, m := range t.Attributes {
			switch {
			case !slices.Contains(writable, m.Attribute):
				invalid = append(invalid, fmt.Errorf("IdP %s: %s maps to %q, not in the profile", t.Name, m.Path, m.Attribute))
			case slices.Contains(keycloak.NeverSynced, m.Attribute):
				invalid = append(invalid, fmt.Errorf("IdP %s: %q is never synced", t.Name, m.Attribute))
			}
		}
	}
	inv := errors.Join(invalid...)
	r.mu.Lock()
	r.profiles[client.ObjectKeyFromObject(ic)] = profileMark{hash: hash, at: time.Now(), invalid: inv}
	r.mu.Unlock()
	return inv
}

// cleanupProfile removes this instance's profile attributes. Users keep
// their values.
func cleanupProfile(ctx context.Context, ic *v1.IdentityContinuity, kc *keycloak.Client) error {
	_, err := kc.EnsureProfile(ctx, client.ObjectKeyFromObject(ic).String(), nil)
	return err
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
	if s := ic.Spec.Sync; s != nil {
		switch {
		case s.Schedule == "" && !s.Suspend:
			return errors.New("spec.sync.schedule is empty: set one, or suspend the sync")
		case s.Schedule != "":
			if err := checkCron(s.Schedule); err != nil {
				return fmt.Errorf("spec.sync.schedule %q: %w", s.Schedule, err)
			}
		}
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
	if s := ic.Spec.Sync; s != nil {
		suspend = s.Suspend
		if s.Schedule != "" {
			schedule = s.Schedule
		}
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

// cronFields are the five fields of a standard cron schedule and the values
// each takes (months and weekdays also by name).
var cronFields = []struct {
	name     string
	min, max int
	names    *regexp.Regexp
}{
	{"minute", 0, 59, nil}, {"hour", 0, 23, nil}, {"day of month", 1, 31, nil},
	{"month", 1, 12, regexp.MustCompile(`^(?i:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)$`)},
	{"day of week", 0, 7, regexp.MustCompile(`^(?i:sun|mon|tue|wed|thu|fri|sat)$`)},
}

// checkCron accepts what a CronJob's schedule does: five fields of numbers,
// ranges, lists and steps, "*" or "?", or a macro such as @daily.
func checkCron(s string) error {
	switch s {
	case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
		return nil
	}
	fs := strings.Fields(s)
	if len(fs) != len(cronFields) {
		return fmt.Errorf("%d fields, want 5 (minute hour day-of-month month day-of-week)", len(fs))
	}
	for i, f := range fs {
		lim := cronFields[i]
		value := func(v string) bool {
			if n, err := strconv.Atoi(v); err == nil {
				return n >= lim.min && n <= lim.max
			}
			return lim.names != nil && lim.names.MatchString(v)
		}
		for _, part := range strings.Split(f, ",") {
			rng, step, stepped := strings.Cut(part, "/")
			if stepped {
				if n, err := strconv.Atoi(step); err != nil || n < 1 {
					return fmt.Errorf("%s: bad step %q", lim.name, part)
				}
			}
			if rng == "*" || rng == "?" {
				continue
			}
			lo, hi, isRange := strings.Cut(rng, "-")
			if !value(lo) || isRange && !value(hi) {
				return fmt.Errorf("%s: %q is not in %d-%d", lim.name, part, lim.min, lim.max)
			}
		}
	}
	return nil
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
