/*
Copyright 2026. projectsveltos.io. All rights reserved.

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

// exec-plugin is a minimal Kubernetes exec credential plugin used only in FV tests.
// It reads a bearer token from a well-known file and writes an ExecCredential JSON
// to stdout so that the clusterinventory-controller can embed it in a kubeconfig.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	tokenFile = "/var/run/secrets/test-exec-plugin/token" //nolint: gosec // used for testing
)

func main() {
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec-plugin: read token: %v\n", err)
		os.Exit(1)
	}

	cred := map[string]interface{}{
		"apiVersion": "client.authentication.k8s.io/v1",
		"kind":       "ExecCredential",
		"status": map[string]interface{}{
			"token": strings.TrimSpace(string(raw)),
		},
	}

	if err := json.NewEncoder(os.Stdout).Encode(cred); err != nil {
		fmt.Fprintf(os.Stderr, "exec-plugin: encode: %v\n", err)
		os.Exit(1)
	}
}
