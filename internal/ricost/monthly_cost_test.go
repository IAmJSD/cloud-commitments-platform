package ricost

import (
	"testing"

	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonthlyCostFromConvertibleRI pins the exact float64 results the
// duplicated copies in internal/api and internal/server returned before they
// were merged into this package (issue #337), except that an RI with no
// positive price is now an error (issue #1188). The comparisons are exact on
// purpose: moving the formula here must not change a single bit of any
// dollar figure.
func TestMonthlyCostFromConvertibleRI(t *testing.T) {
	const oneYear, threeYears = 31536000, 94608000
	cases := []struct {
		name    string
		ri      ec2svc.ConvertibleRI
		want    float64
		wantErr string
		wantIs  error // sentinel the error must wrap; ErrNonPositiveDuration when nil
	}{
		{name: "1y all upfront", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-a", FixedPrice: 8760, Duration: oneYear}, want: 730},
		{name: "3y all upfront", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-b", FixedPrice: 3000, Duration: threeYears}, want: 83.33333333333333},
		{name: "1y no upfront", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-c", RecurringHourlyAmount: 0.05, Duration: oneYear}, want: 36.5},
		{name: "1y partial upfront", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-d", FixedPrice: 438, RecurringHourlyAmount: 0.025, Duration: oneYear}, want: 54.75000000000001},
		{name: "usage price only", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-e", UsagePrice: 0.04, Duration: oneYear}, want: 29.2},
		{name: "all components 3y", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-f", FixedPrice: 1234.56, UsagePrice: 0.011, RecurringHourlyAmount: 0.0207, Duration: threeYears}, want: 57.43433333333333},
		// Zero prices with a valid term mean AWS returned no pricing, not a
		// free RI (issue #1188).
		{name: "zero prices", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-g", Duration: oneYear},
			wantErr: "convertible RI ri-g has no positive price (fixed 0, usage 0, recurring hourly 0): monthly cost is not computable", wantIs: ErrNoPricing},
		{name: "negative price sum", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-l", FixedPrice: -876, Duration: oneYear},
			wantErr: "convertible RI ri-l has no positive price (fixed -876, usage 0, recurring hourly 0): monthly cost is not computable", wantIs: ErrNoPricing},
		{name: "one second term", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-h", FixedPrice: 1, Duration: 1}, want: 2628000},
		{name: "zero duration", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-i", FixedPrice: 100},
			wantErr: "convertible RI ri-i has non-positive duration 0: monthly cost is not computable"},
		{name: "negative duration", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-j", FixedPrice: 100, Duration: -oneYear},
			wantErr: "convertible RI ri-j has non-positive duration -31536000: monthly cost is not computable"},
		{name: "zero duration and zero prices", ri: ec2svc.ConvertibleRI{ReservedInstanceID: "ri-k"},
			wantErr: "convertible RI ri-k has non-positive duration 0: monthly cost is not computable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MonthlyCostFromConvertibleRI(tc.ri)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				wantIs := tc.wantIs
				if wantIs == nil {
					wantIs = ErrNonPositiveDuration
				}
				assert.ErrorIs(t, err, wantIs)
				assert.Zero(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got) // exact on purpose: no numeric drift allowed
		})
	}
}

func TestHoursPerMonth(t *testing.T) {
	assert.Equal(t, 365*24/12, int(HoursPerMonth))
	assert.Equal(t, 730.0, HoursPerMonth)
}
