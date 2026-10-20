package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelperFunctions(t *testing.T) {
	t.Run("generateToken returns unique values", func(t *testing.T) {
		token1, err := generateToken()
		require.NoError(t, err)
		assert.NotEmpty(t, token1)

		token2, err := generateToken()
		require.NoError(t, err)
		assert.NotEqual(t, token1, token2, "tokens should be unique")
	})

	t.Run("password validation", func(t *testing.T) {
		service := &Service{}

		// Valid password
		err := service.validatePassword("SecurePass@123")
		assert.NoError(t, err)

		// Too short
		err = service.validatePassword("Short1")
		assert.Error(t, err)

		// No uppercase
		err = service.validatePassword("lowercase123")
		assert.Error(t, err)

		// No lowercase
		err = service.validatePassword("UPPERCASE123")
		assert.Error(t, err)

		// No number
		err = service.validatePassword("NoNumberHere")
		assert.Error(t, err)

		// Empty password
		err = service.validatePassword("")
		assert.Error(t, err)
	})

	t.Run("password hashing and verification", func(t *testing.T) {
		service := newTestService()
		password := "TestPassword123"

		hash, err := service.hashPassword(password)
		require.NoError(t, err)
		assert.NotEmpty(t, hash)

		// Verify correct password
		assert.True(t, service.verifyPassword(password, hash))

		// Verify wrong password
		assert.False(t, service.verifyPassword("wrongpassword", hash))
	})
}

func TestContainsAll(t *testing.T) {
	t.Run("returns false when only some requested values are allowed", func(t *testing.T) {
		allowed := []string{"a", "b", "c"}
		requested := []string{"b", "d"}
		assert.False(t, containsAll(allowed, requested))
	})

	t.Run("returns false when no intersection", func(t *testing.T) {
		allowed := []string{"a", "b", "c"}
		requested := []string{"d", "e"}
		assert.False(t, containsAll(allowed, requested))
	})

	t.Run("returns false when allowed is empty", func(t *testing.T) {
		allowed := []string{}
		requested := []string{"a", "b"}
		assert.False(t, containsAll(allowed, requested))
	})

	t.Run("fails closed when requested is empty", func(t *testing.T) {
		allowed := []string{"a", "b"}
		assert.False(t, containsAll(allowed, []string{}))
		assert.False(t, containsAll(allowed, nil))
	})

	t.Run("returns true when every requested value is allowed", func(t *testing.T) {
		allowed := []string{"a", "b", "c"}
		requested := []string{"a", "b"}
		assert.True(t, containsAll(allowed, requested))
	})

	t.Run("comparison is exact", func(t *testing.T) {
		assert.False(t, containsAll([]string{"A"}, []string{"a"}))
		assert.False(t, containsAll([]string{"a "}, []string{"a"}))
	})
}
