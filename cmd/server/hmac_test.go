package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadHMACKey(t *testing.T) {
	t.Run("empty path returns error", func(t *testing.T) {
		// when
		_, err := loadHMACKey("")

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--hmac-key-file is required")
	})

	t.Run("missing file returns error", func(t *testing.T) {
		// when
		_, err := loadHMACKey(filepath.Join(t.TempDir(), "does-not-exist"))

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read HMAC key file")
	})

	t.Run("empty file returns error", func(t *testing.T) {
		// given
		path := filepath.Join(t.TempDir(), "empty.key")
		require.NoError(t, os.WriteFile(path, []byte{}, 0o600))

		// when
		_, err := loadHMACKey(path)

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty or whitespace-only")
	})

	t.Run("whitespace-only file returns error", func(t *testing.T) {
		// given
		path := filepath.Join(t.TempDir(), "blank.key")
		require.NoError(t, os.WriteFile(path, []byte(" \n\t\n"), 0o600))

		// when
		_, err := loadHMACKey(path)

		// then
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty or whitespace-only")
	})

	t.Run("trims trailing newline", func(t *testing.T) {
		// given
		path := filepath.Join(t.TempDir(), "key.key")
		require.NoError(t, os.WriteFile(path, []byte("secret\n"), 0o600))

		// when
		key, err := loadHMACKey(path)

		// then
		require.NoError(t, err)
		assert.Equal(t, "secret", key)
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		// given
		path := filepath.Join(t.TempDir(), "key.key")
		require.NoError(t, os.WriteFile(path, []byte("  my-hmac-key  \n"), 0o600))

		// when
		key, err := loadHMACKey(path)

		// then
		require.NoError(t, err)
		assert.Equal(t, "my-hmac-key", key)
	})
}
