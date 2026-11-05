package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDecodeExecutionSettle(t *testing.T) {
	ok, err := decodeExecutionSettle(`{"reason":"  checked the account  ","acknowledge_unknown_outcome":true}`)
	require.NoError(t, err)
	require.Equal(t, "checked the account", ok.Reason)

	for name, body := range map[string]string{
		"empty":         ``,
		"no ack":        `{"reason":"checked the account"}`,
		"ack false":     `{"reason":"checked the account","acknowledge_unknown_outcome":false}`,
		"short":         `{"reason":"  short  ","acknowledge_unknown_outcome":true}`,
		"long":          `{"reason":"` + strings.Repeat("x", executionSettleReasonMax+1) + `","acknowledge_unknown_outcome":true}`,
		"control":       `{"reason":"checked the\naccount","acknowledge_unknown_outcome":true}`,
		"unknown field": `{"reason":"checked the account","acknowledge_unknown_outcome":true,"x":1}`,
		"two bodies":    `{"reason":"checked the account","acknowledge_unknown_outcome":true}{}`,
		"too large":     strings.Repeat(" ", executionSettleBodyMax+1),
	} {
		_, err := decodeExecutionSettle(body)
		var ce *clientError
		require.True(t, errors.As(err, &ce), name)
		require.Equal(t, 400, ce.code, name)
	}
}

func TestMapExecutionSettleError(t *testing.T) {
	require.True(t, IsNotFoundError(mapExecutionSettleError(config.ErrExecutionSettleNotFound)))
	for sentinel, code := range map[error]int{config.ErrExecutionSettleInvalid: 400, config.ErrExecutionSettleConflict: 409} {
		var ce *clientError
		require.True(t, errors.As(mapExecutionSettleError(fmt.Errorf("%w: detail", sentinel)), &ce))
		require.Equal(t, code, ce.code)
	}
	other := errors.New("db down")
	require.Equal(t, other, mapExecutionSettleError(other), "an internal error is not a client error")
}
