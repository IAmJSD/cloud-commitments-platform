package config

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// max_step_commitment is stored in NUMERIC(18,2) CHECK > 0: below a cent it
// rounds to 0 and overflowing values fail the insert, either of which was a 500.
func TestValidateNotificationAndCap_StepCapBounds(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want bool
	}{
		{0, false}, {-1, false}, {0.004, false}, {math.NaN(), false}, {math.Inf(1), false},
		{1e308, false}, {MaxStepCommitmentLimit * 2, false},
		{1.005, false}, {0.015, false}, {1.0049, false}, {1e-3, false},
		{0.01, true}, {100.10, true}, {2500.5, true}, {MaxStepCommitmentLimit, true},
	} {
		v := tc.v
		p := &PurchasePlan{MaxStepCommitment: &v}
		err := p.validateNotificationAndCap()
		assert.Equal(t, tc.want, err == nil, "cap %v: %v", tc.v, err)
	}
	assert.NoError(t, (&PurchasePlan{}).validateNotificationAndCap(), "a NULL cap stays valid (refused at run time)")
}
