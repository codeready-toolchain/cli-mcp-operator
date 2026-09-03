package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWatchNamespaces(t *testing.T) {
	t.Run("WATCH_NAMESPACE wins", func(t *testing.T) {
		t.Setenv(envWatchNamespace, "app-ns")
		t.Setenv(envPodNamespace, "operator-ns")
		assert.Equal(t, []string{"app-ns"}, watchNamespaces())
	})
	t.Run("falls back to POD_NAMESPACE", func(t *testing.T) {
		t.Setenv(envWatchNamespace, "")
		t.Setenv(envPodNamespace, "operator-ns")
		assert.Equal(t, []string{"operator-ns"}, watchNamespaces())
	})
	t.Run("comma-separated", func(t *testing.T) {
		t.Setenv(envWatchNamespace, "a, b")
		t.Setenv(envPodNamespace, "")
		assert.Equal(t, []string{"a", "b"}, watchNamespaces())
	})
	t.Run("both empty watches all", func(t *testing.T) {
		t.Setenv(envWatchNamespace, "")
		t.Setenv(envPodNamespace, "")
		assert.Nil(t, watchNamespaces())
	})
}
