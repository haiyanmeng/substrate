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

package controllers

import (
	"fmt"

	"go.ate.dev/substrate/internal/workerpoolsandbox"
	atev1alpha1 "go.ate.dev/substrate/pkg/api/v1alpha1"
)

// switchesSandboxConfig reports whether class, as resolved, uses a config
// other than the one wp's status uses for it.
func switchesSandboxConfig(wp *atev1alpha1.WorkerPool, class atev1alpha1.WorkerPoolSandboxClassStatus) bool {
	return switchesSandboxConfigFrom(wp.Status.SandboxClasses, class)
}

// switchesSandboxConfigFrom reports whether class, as resolved, uses a config
// other than the one its entry in prev uses.
func switchesSandboxConfigFrom(prev []atev1alpha1.WorkerPoolSandboxClassStatus, class atev1alpha1.WorkerPoolSandboxClassStatus) bool {
	if class.ConfigRef == nil {
		return false
	}
	p := workerpoolsandbox.FindClassStatus(prev, class.Name)
	return p == nil || p.ConfigRef == nil || p.ConfigRef.Name != class.ConfigRef.Name
}

// claimSandboxConfigs returns the status that claims every config a class in
// classes switches to, or nil if no class switches. The claim keeps each
// class's status in wp as it is and names the new config in pendingConfigRef.
// That protects the new config like configRef does while
// confirmSandboxConfigSwitch checks it.
func claimSandboxConfigs(wp *atev1alpha1.WorkerPool, classes []atev1alpha1.WorkerPoolSandboxClassStatus) []atev1alpha1.WorkerPoolSandboxClassStatus {
	claim := make([]atev1alpha1.WorkerPoolSandboxClassStatus, 0, len(classes))
	claimed := false
	for _, class := range classes {
		status := atev1alpha1.WorkerPoolSandboxClassStatus{Name: class.Name}
		if prev := sandboxClassStatus(wp, class.Name); prev != nil {
			status = *prev.DeepCopy()
			status.PendingConfigRef = nil
		}
		if switchesSandboxConfig(wp, class) {
			status.PendingConfigRef = class.ConfigRef.DeepCopy()
			claimed = true
		}
		claim = append(claim, status)
	}
	if !claimed {
		return nil
	}
	return claim
}

// confirmSandboxConfigSwitch returns the status entry for a class that
// switches to a new config. live is that config as read from the API server
// after wp's status claimed it, or nil if it does not exist. The switch stands
// if the config belongs to the class and is not being deleted. Otherwise the
// class keeps the config wp's status uses, with a False Resolved condition.
// configs is the cached list workerpoolsandbox.Keep checks that config against.
func confirmSandboxConfigSwitch(wp *atev1alpha1.WorkerPool, class atev1alpha1.WorkerPoolSandboxClassStatus, live *atev1alpha1.SandboxConfig, configs []atev1alpha1.SandboxConfig) atev1alpha1.WorkerPoolSandboxClassStatus {
	name := class.ConfigRef.Name
	var reason, message string
	switch {
	case live == nil:
		reason, message = atev1alpha1.WorkerPoolReasonSandboxConfigNotFound,
			fmt.Sprintf("SandboxConfig %q not found", name)
	case live.Spec.SandboxClass != class.Name:
		reason, message = atev1alpha1.WorkerPoolReasonSandboxClassMismatch,
			fmt.Sprintf("SandboxConfig %q is for sandbox class %q, not %q", name, live.Spec.SandboxClass, class.Name)
	case !live.DeletionTimestamp.IsZero():
		reason, message = atev1alpha1.WorkerPoolReasonSandboxConfigDeleting,
			fmt.Sprintf("SandboxConfig %q is being deleted", name)
	default:
		return class
	}
	inUse := ""
	if prev := sandboxClassStatus(wp, class.Name); prev != nil && prev.ConfigRef != nil {
		inUse = prev.ConfigRef.Name
	}
	sc, cond := workerpoolsandbox.Keep(class.Name, inUse, configs, reason, message)
	return workerpoolsandbox.ClassStatusFor(wp, class.Name, sc, cond)
}

// sandboxClassStatus returns the status entry wp records for class, or nil.
func sandboxClassStatus(wp *atev1alpha1.WorkerPool, class atev1alpha1.SandboxClass) *atev1alpha1.WorkerPoolSandboxClassStatus {
	return workerpoolsandbox.FindClassStatus(wp.Status.SandboxClasses, class)
}

// sandboxConfigsInUse returns the names of the SandboxConfigs wp's status
// records as in use. That is every configRef and every pendingConfigRef.
func sandboxConfigsInUse(wp *atev1alpha1.WorkerPool) []string {
	names := make([]string, 0, len(wp.Status.SandboxClasses))
	for _, s := range wp.Status.SandboxClasses {
		if s.ConfigRef != nil {
			names = append(names, s.ConfigRef.Name)
		}
		if s.PendingConfigRef != nil {
			names = append(names, s.PendingConfigRef.Name)
		}
	}
	return names
}
