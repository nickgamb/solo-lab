package controller

import (
	"slices"
	"testing"

	v1 "github.com/nickgamb/solo-lab/apps/continuity/api/v1alpha1"
)

func TestSyncCronJob(t *testing.T) {
	ic := &v1.IdentityContinuity{}
	ic.Namespace, ic.Name, ic.UID = "sv-identity", "sterling-vance", "u1"
	ic.Spec.Sync = &v1.Sync{Schedule: "0 */6 * * *", Suspend: true}
	r := &Reconciler{SyncImage: "img", SyncCAConfigMap: "lab-ca-bundle"}
	cj := r.syncCronJob(ic)
	s := cj.Spec
	if s.Schedule != "0 */6 * * *" || *s.TimeZone != "Etc/UTC" || !*s.Suspend || s.ConcurrencyPolicy != "Forbid" {
		t.Fatalf("schedule %+v", s)
	}
	if o := cj.OwnerReferences; len(o) != 1 || o[0].UID != "u1" || !*o[0].Controller {
		t.Errorf("owner %v: deleting the instance must delete the CronJob", o)
	}
	p := s.JobTemplate.Spec.Template.Spec
	c := p.Containers[0]
	if p.ServiceAccountName != "continuity-sync" || c.Image != "img" || !slices.Contains(c.Args, "--instance=sv-identity/sterling-vance") {
		t.Errorf("pod %+v", p)
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation || !*p.SecurityContext.RunAsNonRoot {
		t.Error("the sync runs non-root, read-only, without privilege escalation")
	}
	if *s.JobTemplate.Spec.BackoffLimit != 0 {
		t.Error("a failed run waits for the next schedule; it is not retried")
	}
	if cj.Labels[LabelInstance] != "sv-identity.sterling-vance" {
		t.Errorf("labels %v", cj.Labels)
	}
}

func TestSyncCronJobForDirectoryOnly(t *testing.T) {
	ic := &v1.IdentityContinuity{}
	ic.Name = "x"
	ic.Spec.Tiers = []v1.Tier{{Name: "gluu", Type: "oidc", Directory: &v1.Directory{Type: "scim"}}}
	if !hasDirectory(ic) {
		t.Fatal("a directory keeps the CronJob (for tests)")
	}
	cj := (&Reconciler{SyncImage: "img"}).syncCronJob(ic)
	if !*cj.Spec.Suspend {
		t.Error("without spec.sync the CronJob never runs on its own")
	}
}

func TestWritable(t *testing.T) {
	ic := &v1.IdentityContinuity{}
	ic.Spec.Profile = &v1.Profile{Attributes: []v1.ProfileAttribute{{Name: "department"}}}
	w := Writable(ic)
	for _, a := range []string{"firstName", "lastName", "department"} {
		if !slices.Contains(w, a) {
			t.Errorf("%s not writable", a)
		}
	}
}
