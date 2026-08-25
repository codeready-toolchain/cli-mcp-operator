package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestK8sHealthCheckerListsPodsInNamespace(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "namespaces", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("cluster-scoped namespace get must not be used")
	})
	checker := &k8sHealthChecker{clientset: client, namespace: "cli-mcp"}

	require.NoError(t, checker.CheckHealth(t.Context()))
}
