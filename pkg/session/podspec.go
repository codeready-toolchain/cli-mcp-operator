package session

import (
	"cmp"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildBasePodSpec constructs the shared sandbox pod spec used by MCP on-demand
// create and (later) the operator warm pool. It sets instance+component labels,
// dedicated SA, automountServiceAccountToken false, kubeconfig mount, and
// today's non-root / drop-caps security context, then merges the class overlay
// (image, resources, env, imagePullPolicy). Callers add session-specific
// fields (session-id label, SANDBOX_AUTH_TOKEN env) on assigned pods only.
func BuildBasePodSpec(name string, config SandboxConfig) *corev1.Pod {
	now := time.Now().UTC().Format(time.RFC3339)
	runAsNonRoot := true
	allowPrivEsc := false
	automount := false
	defaults := DefaultConfig()
	agentPort := cmp.Or(config.AgentPort, defaults.AgentPort)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.Namespace,
			Labels: map[string]string{
				LabelComponent: ComponentSandbox,
				LabelInstance:  config.InstanceName,
			},
			Annotations: map[string]string{
				AnnotationCreatedAt:    now,
				AnnotationLastActivity: now,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           config.ServiceAccountName,
			AutomountServiceAccountToken: &automount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
			},
			Containers: []corev1.Container{
				{
					Name:            "sandbox",
					Image:           config.Image,
					ImagePullPolicy: config.ImagePullPolicy,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cmp.Or(config.CPURequest, defaults.CPURequest)),
							corev1.ResourceMemory: resource.MustParse(cmp.Or(config.MemoryRequest, defaults.MemoryRequest)),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cmp.Or(config.CPULimit, defaults.CPULimit)),
							corev1.ResourceMemory: resource.MustParse(cmp.Or(config.MemoryLimit, defaults.MemoryLimit)),
						},
					},
					// Exec probe hits /health on loopback so kubelet does not need NetworkPolicy
					// ingress (HTTPGet from the node IP is blocked when only component=server
					// may reach :8090). /health reflects bash session liveness (IsAlive), not
					// merely TCP accept. curl-minimal is part of the sandbox image contract.
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{
								Command: []string{
									"curl",
									"-fsS",
									"--max-time",
									"1",
									fmt.Sprintf("http://127.0.0.1:%d/health", agentPort),
								},
							},
						},
						InitialDelaySeconds: 2,
						TimeoutSeconds:      2, // > curl --max-time so shell/startup does not consume the whole budget
						PeriodSeconds:       10,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivEsc,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					Env: overlaySandboxEnv(config.Env),
					VolumeMounts: []corev1.VolumeMount{
						{Name: "kubeconfig", MountPath: "/config", ReadOnly: true},
						{Name: "workspace", MountPath: "/workspace"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: config.KubeconfigSecret,
						},
					},
				},
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}
}

func overlaySandboxEnv(userEnv []corev1.EnvVar) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "KUBECONFIG", Value: "/config/kubeconfig"},
		{Name: "HOME", Value: "/workspace"},
	}
	for _, e := range userEnv {
		if _, reserved := reservedSandboxEnv[e.Name]; reserved {
			continue
		}
		env = append(env, e)
	}
	return env
}
