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

// Package workerpoolsandbox resolves the SandboxConfig each sandbox class of a
// WorkerPool uses, and builds the pool's status.sandboxClasses entries from
// it. ate-controller writes those entries; fake-workersync writes them in its
// place on a --fake-data-plane install. Both share this package so the fake
// data plane resolves configs exactly as the real one does.
package workerpoolsandbox

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	atev1alpha1 "go.ate.dev/substrate/pkg/api/v1alpha1"
)

// resolveLatest picks the SandboxConfig the spec of one sandbox
// class entry asks for. It does not fall back to the config the pool already
// uses. On failure it returns nil plus the condition reason and message.
//
//   - With configRef, that config is used. It must exist, belong to the same
//     class, and not be being deleted.
//   - Without configRef, the newest config of the class that is annotated as
//     the class default and is not being deleted is used. If two were created
//     at the same time, the name that sorts first wins.
func resolveLatest(entry atev1alpha1.WorkerPoolSandboxClass, configs []atev1alpha1.SandboxConfig) (*atev1alpha1.SandboxConfig, string, string) {
	if entry.ConfigRef != nil {
		name := entry.ConfigRef.Name
		i := slices.IndexFunc(configs, func(sc atev1alpha1.SandboxConfig) bool { return sc.Name == name })
		if i < 0 {
			return nil, atev1alpha1.WorkerPoolReasonSandboxConfigNotFound,
				fmt.Sprintf("SandboxConfig %q not found", name)
		}
		sc := &configs[i]
		if sc.Spec.SandboxClass != entry.Name {
			return nil, atev1alpha1.WorkerPoolReasonSandboxClassMismatch,
				fmt.Sprintf("SandboxConfig %q is for sandbox class %q, not %q", name, sc.Spec.SandboxClass, entry.Name)
		}
		if !sc.DeletionTimestamp.IsZero() {
			return nil, atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
				fmt.Sprintf("SandboxConfig %q is being deleted", name)
		}
		return sc, "", ""
	}

	isDefault := func(sc atev1alpha1.SandboxConfig) bool {
		return sc.Spec.SandboxClass == entry.Name &&
			sc.Annotations[atev1alpha1.SandboxConfigClassDefaultAnnotation] == "true"
	}
	var best *atev1alpha1.SandboxConfig
	for i := range configs {
		sc := &configs[i]
		if isDefault(*sc) && sc.DeletionTimestamp.IsZero() && (best == nil || newer(sc, best)) {
			best = sc
		}
	}
	if best != nil {
		return best, "", ""
	}
	isDeletingDefault := func(sc atev1alpha1.SandboxConfig) bool {
		return isDefault(sc) && !sc.DeletionTimestamp.IsZero()
	}
	if slices.ContainsFunc(configs, isDeletingDefault) {
		return nil, atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			fmt.Sprintf("the default SandboxConfig for sandbox class %q is being deleted", entry.Name)
	}
	return nil, atev1alpha1.WorkerPoolReasonDefaultConfigNotFound,
		fmt.Sprintf("no SandboxConfig of sandbox class %q is annotated %s=true", entry.Name, atev1alpha1.SandboxConfigClassDefaultAnnotation)
}

// resolve returns the SandboxConfig one sandbox class entry uses
// and its Resolved condition. inUse is the config the pool's status already
// uses for this class, or "". When the spec does not resolve, the condition is
// False and the pool keeps inUse. See Keep.
func resolve(entry atev1alpha1.WorkerPoolSandboxClass, inUse string, configs []atev1alpha1.SandboxConfig) (*atev1alpha1.SandboxConfig, metav1.Condition) {
	sc, reason, message := resolveLatest(entry, configs)
	if sc != nil {
		return sc, metav1.Condition{
			Type:    atev1alpha1.WorkerPoolSandboxClassConditionResolved,
			Status:  metav1.ConditionTrue,
			Reason:  atev1alpha1.WorkerPoolReasonResolved,
			Message: fmt.Sprintf("resolved to SandboxConfig %q", sc.Name),
		}
	}
	return Keep(entry.Name, inUse, configs, reason, message)
}

// Keep returns the config a class keeps when its spec does not
// resolve, and a False Resolved condition with reason and message.
//
// The class keeps inUse, so a running pool never loses its config. The result
// is nil only when inUse is "" or no longer names a config of the class. A
// config in use that is being deleted carries the protection finalizer while
// any pool's status names it. One without the finalizer is about to
// disappear, so it is not kept.
func Keep(class atev1alpha1.SandboxClass, inUse string, configs []atev1alpha1.SandboxConfig, reason, message string) (*atev1alpha1.SandboxConfig, metav1.Condition) {
	var kept *atev1alpha1.SandboxConfig
	if inUse != "" {
		i := slices.IndexFunc(configs, func(sc atev1alpha1.SandboxConfig) bool { return sc.Name == inUse })
		if i >= 0 && configs[i].Spec.SandboxClass == class &&
			(configs[i].DeletionTimestamp.IsZero() || slices.Contains(configs[i].Finalizers, atev1alpha1.SandboxConfigWorkerPoolProtectionFinalizer)) {
			kept = &configs[i]
			message = fmt.Sprintf("%s; keeping %q", message, kept.Name)
		}
	}
	return kept, metav1.Condition{
		Type:    atev1alpha1.WorkerPoolSandboxClassConditionResolved,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
}

// newer reports whether a takes precedence over b as a class
// default.
func newer(a, b *atev1alpha1.SandboxConfig) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name < b.Name
}

// ResolveClasses resolves every entry of wp.Spec.SandboxClasses. It
// returns one status entry per class, and whether every class resolved. Each
// entry carries the config the class uses, nil if it has none, and its
// Resolved condition.
func ResolveClasses(wp *atev1alpha1.WorkerPool, configs []atev1alpha1.SandboxConfig) ([]atev1alpha1.WorkerPoolSandboxClassStatus, bool) {
	statuses := make([]atev1alpha1.WorkerPoolSandboxClassStatus, 0, len(wp.Spec.SandboxClasses))
	resolved := true
	for _, entry := range wp.Spec.SandboxClasses {
		prev := FindClassStatus(wp.Status.SandboxClasses, entry.Name)
		inUse := ""
		if prev != nil && prev.ConfigRef != nil {
			inUse = prev.ConfigRef.Name
		}
		sc, cond := resolve(entry, inUse, configs)
		if cond.Status != metav1.ConditionTrue {
			resolved = false
		}
		statuses = append(statuses, ClassStatusFor(wp, entry.Name, sc, cond))
	}
	return statuses, resolved
}

// ClassStatusFor builds the status entry for class from the config it
// uses, nil if none, and its Resolved condition. The entry starts from the
// class's previous conditions in wp, so lastTransitionTime only moves when the
// condition's status does.
func ClassStatusFor(wp *atev1alpha1.WorkerPool, class atev1alpha1.SandboxClass, sc *atev1alpha1.SandboxConfig, cond metav1.Condition) atev1alpha1.WorkerPoolSandboxClassStatus {
	cond.ObservedGeneration = wp.Generation
	status := atev1alpha1.WorkerPoolSandboxClassStatus{Name: class}
	if prev := FindClassStatus(wp.Status.SandboxClasses, class); prev != nil {
		status.Conditions = slices.Clone(prev.Conditions)
	}
	meta.SetStatusCondition(&status.Conditions, cond)
	if sc != nil {
		status.ConfigRef = &atev1alpha1.SandboxConfigReference{Name: sc.Name}
	}
	return status
}

// FindClassStatus returns the entry of class in statuses, or nil.
func FindClassStatus(statuses []atev1alpha1.WorkerPoolSandboxClassStatus, class atev1alpha1.SandboxClass) *atev1alpha1.WorkerPoolSandboxClassStatus {
	for i := range statuses {
		if statuses[i].Name == class {
			return &statuses[i]
		}
	}
	return nil
}
