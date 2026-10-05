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
// create and the operator warm pool. It sets instance+component labels,
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
					Env:          overlaySandboxEnv(config),
					VolumeMounts: sandboxMounts(config),
				},
			},
			Volumes: sandboxVolumes(config),
		},
	}
}

// OperatorEnv is the env the sandbox builder owns. User overlay entries with
// these names are dropped.
func OperatorEnv(config SandboxConfig) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, 10)
	if config.DummyKubeconfigConfigMap != "" || config.KubeconfigSecret != "" {
		env = append(env, corev1.EnvVar{Name: "KUBECONFIG", Value: KubeconfigPath})
	}
	env = append(env, corev1.EnvVar{Name: "HOME", Value: "/workspace"})
	if config.ProxyService != "" {
		proxyURL := fmt.Sprintf("http://%s:%d", config.ProxyService, ProxyListenPort)
		for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, corev1.EnvVar{Name: name, Value: proxyURL})
		}
		for _, name := range []string{"NO_PROXY", "no_proxy"} {
			env = append(env, corev1.EnvVar{Name: name, Value: NoProxyValue})
		}
	}
	if config.ProxyCASecret != "" {
		env = append(env,
			corev1.EnvVar{Name: "SSL_CERT_FILE", Value: ProxyCAFile},
			corev1.EnvVar{Name: "REQUESTS_CA_BUNDLE", Value: ProxyCAFile},
		)
	}
	return env
}

func overlaySandboxEnv(config SandboxConfig) []corev1.EnvVar {
	env := OperatorEnv(config)
	for _, e := range config.Env {
		if _, reserved := reservedSandboxEnv[e.Name]; reserved {
			continue
		}
		env = append(env, e)
	}
	return env
}

func sandboxMounts(config SandboxConfig) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount
	if config.DummyKubeconfigConfigMap != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "kubeconfig",
			MountPath: KubeconfigPath,
			SubPath:   "kubeconfig",
			ReadOnly:  true,
		})
	} else if config.KubeconfigSecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "kubeconfig",
			MountPath: "/config",
			ReadOnly:  true,
		})
	}
	if config.ProxyCASecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "proxy-ca",
			MountPath: ProxyCAFile,
			SubPath:   "ca.crt",
			ReadOnly:  true,
		})
	}
	mounts = append(mounts, corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"})
	return mounts
}

func sandboxVolumes(config SandboxConfig) []corev1.Volume {
	var volumes []corev1.Volume
	if config.DummyKubeconfigConfigMap != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "kubeconfig",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: config.DummyKubeconfigConfigMap},
					Items: []corev1.KeyToPath{{
						Key:  "kubeconfig",
						Path: "kubeconfig",
					}},
				},
			},
		})
	} else if config.KubeconfigSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "kubeconfig",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: config.KubeconfigSecret},
			},
		})
	}
	if config.ProxyCASecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "proxy-ca",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: config.ProxyCASecret,
					Items: []corev1.KeyToPath{{
						Key:  "ca.crt",
						Path: "ca.crt",
					}},
				},
			},
		})
	}
	volumes = append(volumes, corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	return volumes
}
