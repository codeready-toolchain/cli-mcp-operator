/*
Copyright 2026 CodeReady Toolchain.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"os"
	"strings"
)

const (
	envWatchNamespace = "WATCH_NAMESPACE"
	envPodNamespace   = "POD_NAMESPACE"
)

// watchNamespaces returns OperatorGroup targetNamespaces (OLM injects
// WATCH_NAMESPACE from olm.targetNamespaces). Empty WATCH_NAMESPACE falls
// back to POD_NAMESPACE so kustomize OwnNamespace still scopes the cache.
// Both unset (make run) watches all namespaces.
func watchNamespaces() []string {
	raw := strings.TrimSpace(os.Getenv(envWatchNamespace))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(envPodNamespace))
	}
	if raw == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if ns := strings.TrimSpace(part); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}
