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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	climcpv1alpha1 "github.com/codeready-toolchain/cli-mcp-operator/api/v1alpha1"
)

func TestNormalizeDeploymentFillsAPIDefaults(t *testing.T) {
	t.Parallel()
	r := &CliMcpInstanceReconciler{Images: testImages()}
	inst := &climcpv1alpha1.CliMcpInstance{}
	inst.Name = "oc"
	inst.Namespace = "ns"
	hmac := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}}

	deploy := &appsv1.Deployment{}
	deploy.Spec.Template = r.mcpPodTemplate(inst, nil, hmac)
	NormalizeDeployment(deploy)

	server := deploy.Spec.Template.Spec.Containers[0]
	assert.Equal(t, corev1.PullIfNotPresent, server.ImagePullPolicy)
	require.NotNil(t, server.ReadinessProbe)
	assert.Equal(t, int32(3), server.ReadinessProbe.FailureThreshold)
	assert.Equal(t, int32(1), server.ReadinessProbe.SuccessThreshold)
	assert.Equal(t, []string{"/bin/sh", "-c", "echo GET /live >/dev/tcp/127.0.0.1/8080"}, server.ReadinessProbe.Exec.Command)

	require.NotEmpty(t, deploy.Spec.Template.Spec.Volumes)
	require.NotNil(t, deploy.Spec.Template.Spec.Volumes[0].Secret)
	require.NotNil(t, deploy.Spec.Template.Spec.Volumes[0].Secret.DefaultMode)
	assert.Equal(t, int32(0644), *deploy.Spec.Template.Spec.Volumes[0].Secret.DefaultMode)

	assert.Equal(t, corev1.RestartPolicyAlways, deploy.Spec.Template.Spec.RestartPolicy)
	require.NotNil(t, deploy.Spec.RevisionHistoryLimit)
	assert.Equal(t, int32(10), *deploy.Spec.RevisionHistoryLimit)
}

func TestNormalizeDeploymentPullAlwaysForLatest(t *testing.T) {
	t.Parallel()
	deploy := &appsv1.Deployment{}
	deploy.Spec.Template.Spec.Containers = []corev1.Container{{
		Name:  "c",
		Image: "example.com/cli-mcp-server:latest",
	}}
	NormalizeDeployment(deploy)
	assert.Equal(t, corev1.PullAlways, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy)
}
