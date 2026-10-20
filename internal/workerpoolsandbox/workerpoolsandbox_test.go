// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package workerpoolsandbox

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	atev1alpha1 "go.ate.dev/substrate/pkg/api/v1alpha1"
)

func TestResolve(t *testing.T) {
	t.Parallel()
	t0 := metav1.NewTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(time.Minute))
	sc := func(name string, class atev1alpha1.SandboxClass, isDefault bool, created metav1.Time, mutate ...func(*atev1alpha1.SandboxConfig)) atev1alpha1.SandboxConfig {
		c := *makeSandboxConfig(name, class, isDefault)
		c.CreationTimestamp = created
		for _, m := range mutate {
			m(&c)
		}
		return c
	}
	deleting := func(c *atev1alpha1.SandboxConfig) { c.DeletionTimestamp = &t1 }
	protected := func(c *atev1alpha1.SandboxConfig) {
		c.Finalizers = append(c.Finalizers, atev1alpha1.SandboxConfigWorkerPoolProtectionFinalizer)
	}
	gvisor := func(ref string) atev1alpha1.WorkerPoolSandboxClass {
		e := atev1alpha1.WorkerPoolSandboxClass{Name: atev1alpha1.SandboxClassGvisor}
		if ref != "" {
			e.ConfigRef = &atev1alpha1.SandboxConfigReference{Name: ref}
		}
		return e
	}

	gv := atev1alpha1.SandboxClassGvisor

	tests := []struct {
		name    string
		entry   atev1alpha1.WorkerPoolSandboxClass
		inUse   string
		configs []atev1alpha1.SandboxConfig
		// want is the config the class uses, or "" for none.
		want string
		// wantReason is the False condition's reason, or "" for True.
		wantReason  string
		wantMessage string
	}{
		{
			name:        "explicit ref resolves even when another config is the default",
			entry:       gvisor("custom"),
			configs:     []atev1alpha1.SandboxConfig{sc("custom", gv, false, t0), sc("def", gv, true, t1)},
			want:        "custom",
			wantMessage: `resolved to SandboxConfig "custom"`,
		},
		{
			name:        "explicit ref not found",
			entry:       gvisor("missing"),
			configs:     []atev1alpha1.SandboxConfig{sc("def", gv, true, t0)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigNotFound,
			wantMessage: `SandboxConfig "missing" not found`,
		},
		{
			name:        "explicit ref not found keeps the config in use",
			entry:       gvisor("missing"),
			inUse:       "old",
			configs:     []atev1alpha1.SandboxConfig{sc("old", gv, false, t0)},
			want:        "old",
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigNotFound,
			wantMessage: `SandboxConfig "missing" not found; keeping "old"`,
		},
		{
			name:        "explicit ref of another class",
			entry:       gvisor("vm"),
			configs:     []atev1alpha1.SandboxConfig{sc("vm", atev1alpha1.SandboxClassMicroVM, false, t0)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxClassMismatch,
			wantMessage: `SandboxConfig "vm" is for sandbox class "microvm", not "gvisor"`,
		},
		{
			name:        "explicit ref of another class keeps the config in use",
			entry:       gvisor("vm"),
			inUse:       "old",
			configs:     []atev1alpha1.SandboxConfig{sc("vm", atev1alpha1.SandboxClassMicroVM, false, t0), sc("old", gv, false, t0)},
			want:        "old",
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxClassMismatch,
			wantMessage: `SandboxConfig "vm" is for sandbox class "microvm", not "gvisor"; keeping "old"`,
		},
		{
			name:        "explicit ref being deleted takes no new users",
			entry:       gvisor("dying"),
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, false, t0, deleting)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `SandboxConfig "dying" is being deleted`,
		},
		{
			name:        "explicit ref being deleted is kept but reported by a pool already using it",
			entry:       gvisor("dying"),
			inUse:       "dying",
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, false, t0, deleting, protected)},
			want:        "dying",
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `SandboxConfig "dying" is being deleted; keeping "dying"`,
		},
		{
			name:        "explicit ref being deleted and released is not kept by a pool using it",
			entry:       gvisor("dying"),
			inUse:       "dying",
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, false, t0, deleting)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `SandboxConfig "dying" is being deleted`,
		},
		{
			name:        "explicit ref being deleted keeps the previous config in use",
			entry:       gvisor("dying"),
			inUse:       "old",
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, false, t0, deleting), sc("old", gv, false, t0)},
			want:        "old",
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `SandboxConfig "dying" is being deleted; keeping "old"`,
		},
		{
			name:        "no default",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("plain", gv, false, t0)},
			wantReason:  atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
			wantMessage: `no SandboxConfig of sandbox class "gvisor" is annotated sandboxconfig.ate.dev/is-class-default=true`,
		},
		{
			name:        "default of another class is ignored",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("vm", atev1alpha1.SandboxClassMicroVM, true, t0)},
			wantReason:  atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
			wantMessage: `no SandboxConfig of sandbox class "gvisor" is annotated sandboxconfig.ate.dev/is-class-default=true`,
		},
		{
			name:  "annotation must be exactly true",
			entry: gvisor(""),
			configs: []atev1alpha1.SandboxConfig{sc("no", gv, false, t0, func(c *atev1alpha1.SandboxConfig) {
				c.Annotations = map[string]string{atev1alpha1.SandboxConfigClassDefaultAnnotation: "True"}
			})},
			wantReason:  atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
			wantMessage: `no SandboxConfig of sandbox class "gvisor" is annotated sandboxconfig.ate.dev/is-class-default=true`,
		},
		{
			name:        "default being deleted takes no new users",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, true, t0, deleting)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `the default SandboxConfig for sandbox class "gvisor" is being deleted`,
		},
		{
			name:        "several defaults being deleted are reported",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("dying-new", gv, true, t1, deleting), sc("dying-old", gv, true, t0, deleting)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `the default SandboxConfig for sandbox class "gvisor" is being deleted`,
		},
		{
			name:        "default being deleted is kept but reported by a pool already using it",
			entry:       gvisor(""),
			inUse:       "dying",
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, true, t0, deleting, protected)},
			want:        "dying",
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `the default SandboxConfig for sandbox class "gvisor" is being deleted; keeping "dying"`,
		},
		{
			name:        "default being deleted and released is not kept by a pool using it",
			entry:       gvisor(""),
			inUse:       "dying",
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, true, t0, deleting)},
			wantReason:  atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			wantMessage: `the default SandboxConfig for sandbox class "gvisor" is being deleted`,
		},
		{
			name:        "a live default replaces the deleting one in use, even when older",
			entry:       gvisor(""),
			inUse:       "dying",
			configs:     []atev1alpha1.SandboxConfig{sc("dying", gv, true, t1, deleting), sc("old", gv, true, t0)},
			want:        "old",
			wantMessage: `resolved to SandboxConfig "old"`,
		},
		{
			name:        "config in use that lost the default annotation is kept but reported",
			entry:       gvisor(""),
			inUse:       "plain",
			configs:     []atev1alpha1.SandboxConfig{sc("plain", gv, false, t0)},
			want:        "plain",
			wantReason:  atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
			wantMessage: `no SandboxConfig of sandbox class "gvisor" is annotated sandboxconfig.ate.dev/is-class-default=true; keeping "plain"`,
		},
		{
			name:        "config in use that no longer exists is not kept",
			entry:       gvisor(""),
			inUse:       "gone",
			configs:     []atev1alpha1.SandboxConfig{sc("plain", gv, false, t0)},
			wantReason:  atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
			wantMessage: `no SandboxConfig of sandbox class "gvisor" is annotated sandboxconfig.ate.dev/is-class-default=true`,
		},
		{
			name:        "config in use of another class is not kept",
			entry:       gvisor(""),
			inUse:       "vm",
			configs:     []atev1alpha1.SandboxConfig{sc("vm", atev1alpha1.SandboxClassMicroVM, true, t0)},
			wantReason:  atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
			wantMessage: `no SandboxConfig of sandbox class "gvisor" is annotated sandboxconfig.ate.dev/is-class-default=true`,
		},
		{
			name:        "newest default wins",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("a-old", gv, true, t0), sc("z-new", gv, true, t1), sc("plain", gv, false, t1)},
			want:        "z-new",
			wantMessage: `resolved to SandboxConfig "z-new"`,
		},
		{
			name:        "newest default wins regardless of list order",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("z-new", gv, true, t1), sc("a-old", gv, true, t0)},
			want:        "z-new",
			wantMessage: `resolved to SandboxConfig "z-new"`,
		},
		{
			name:        "creation time tie goes to the first name",
			entry:       gvisor(""),
			configs:     []atev1alpha1.SandboxConfig{sc("b", gv, true, t0), sc("a", gv, true, t0)},
			want:        "a",
			wantMessage: `resolved to SandboxConfig "a"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, cond := resolve(tc.entry, tc.inUse, tc.configs)
			gotName := ""
			if got != nil {
				gotName = got.Name
			}
			want := metav1.Condition{
				Type:    atev1alpha1.WorkerPoolSandboxClassConditionResolved,
				Status:  metav1.ConditionTrue,
				Reason:  atev1alpha1.WorkerPoolReasonResolved,
				Message: tc.wantMessage,
			}
			if tc.wantReason != "" {
				want.Status = metav1.ConditionFalse
				want.Reason = tc.wantReason
			}
			if gotName != tc.want || cond != want {
				t.Fatalf("resolve = (%q, %+v), want (%q, %+v)", gotName, cond, tc.want, want)
			}
		})
	}
}

// TestResolveSandboxClasses verifies the status entries built for a pool:
// the config each class uses, a condition stamped with the pool's generation
// whose lastTransitionTime moves only when its status does, and whether
// every class resolved.
func TestResolveClasses(t *testing.T) {
	t.Parallel()
	const generation = 3
	earlier := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	configs := []atev1alpha1.SandboxConfig{*makeSandboxConfig("def", atev1alpha1.SandboxClassGvisor, true)}
	ref := func(name string) *atev1alpha1.SandboxConfigReference {
		if name == "" {
			return nil
		}
		return &atev1alpha1.SandboxConfigReference{Name: name}
	}
	cond := func(status metav1.ConditionStatus, reason, message string, gen int64, at metav1.Time) metav1.Condition {
		return metav1.Condition{
			Type:               atev1alpha1.WorkerPoolSandboxClassConditionResolved,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: gen,
			LastTransitionTime: at,
		}
	}
	resolvedDef := func(gen int64, at metav1.Time) metav1.Condition {
		return cond(metav1.ConditionTrue, atev1alpha1.WorkerPoolReasonResolved, `resolved to SandboxConfig "def"`, gen, at)
	}
	notFound := func(message string, gen int64, at metav1.Time) metav1.Condition {
		return cond(metav1.ConditionFalse, atev1alpha1.WorkerPoolReasonSandboxConfigNotFound, message, gen, at)
	}
	// now stands for a lastTransitionTime set during the call.
	var now metav1.Time

	tests := []struct {
		name string
		// specRef is the pool's configRef. "" follows the class default.
		specRef string
		prev    []atev1alpha1.WorkerPoolSandboxClassStatus
		want    []atev1alpha1.WorkerPoolSandboxClassStatus
		// wantResolved is the second return value.
		wantResolved bool
	}{
		{
			name:         "first resolve",
			want:         []atev1alpha1.WorkerPoolSandboxClassStatus{{Name: atev1alpha1.SandboxClassGvisor, ConfigRef: ref("def"), Conditions: []metav1.Condition{resolvedDef(generation, now)}}},
			wantResolved: true,
		},
		{
			name:         "still resolved keeps lastTransitionTime",
			prev:         []atev1alpha1.WorkerPoolSandboxClassStatus{{Name: atev1alpha1.SandboxClassGvisor, ConfigRef: ref("def"), Conditions: []metav1.Condition{resolvedDef(generation-1, earlier)}}},
			want:         []atev1alpha1.WorkerPoolSandboxClassStatus{{Name: atev1alpha1.SandboxClassGvisor, ConfigRef: ref("def"), Conditions: []metav1.Condition{resolvedDef(generation, earlier)}}},
			wantResolved: true,
		},
		{
			name:    "never resolved records no config",
			specRef: "missing",
			want:    []atev1alpha1.WorkerPoolSandboxClassStatus{{Name: atev1alpha1.SandboxClassGvisor, Conditions: []metav1.Condition{notFound(`SandboxConfig "missing" not found`, generation, now)}}},
		},
		{
			name:    "stops resolving keeps the config and moves lastTransitionTime",
			specRef: "missing",
			prev:    []atev1alpha1.WorkerPoolSandboxClassStatus{{Name: atev1alpha1.SandboxClassGvisor, ConfigRef: ref("def"), Conditions: []metav1.Condition{resolvedDef(generation-1, earlier)}}},
			want:    []atev1alpha1.WorkerPoolSandboxClassStatus{{Name: atev1alpha1.SandboxClassGvisor, ConfigRef: ref("def"), Conditions: []metav1.Condition{notFound(`SandboxConfig "missing" not found; keeping "def"`, generation, now)}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wp := makeWorkerPool("pool", "default")
			wp.Generation = generation
			wp.Spec.SandboxClasses[0].ConfigRef = ref(tc.specRef)
			wp.Status.SandboxClasses = tc.prev
			before := metav1.Now().Rfc3339Copy()
			got, resolved := ResolveClasses(wp, configs)
			if resolved != tc.wantResolved {
				t.Errorf("resolved = %v, want %v", resolved, tc.wantResolved)
			}
			// Replace a lastTransitionTime set during the call with now.
			for i := range got {
				for j := range got[i].Conditions {
					if c := &got[i].Conditions[j]; !c.LastTransitionTime.Before(&before) {
						c.LastTransitionTime = now
					}
				}
			}
			if !equality.Semantic.DeepEqual(got, tc.want) {
				t.Errorf("ResolveClasses = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func makeSandboxConfig(name string, class atev1alpha1.SandboxClass, isDefault bool) *atev1alpha1.SandboxConfig {
	sc := &atev1alpha1.SandboxConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       atev1alpha1.SandboxConfigSpec{SandboxClass: class},
	}
	if isDefault {
		sc.Annotations = map[string]string{atev1alpha1.SandboxConfigClassDefaultAnnotation: "true"}
	}
	return sc
}

func makeWorkerPool(name, ns string) *atev1alpha1.WorkerPool {
	return &atev1alpha1.WorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       atev1alpha1.WorkerPoolSpec{SandboxClasses: []atev1alpha1.WorkerPoolSandboxClass{{Name: atev1alpha1.SandboxClassGvisor}}},
	}
}
