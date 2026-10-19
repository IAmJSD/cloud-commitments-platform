package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/archera"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

const (
	recItem  = "22222222-2222-4222-8222-222222222222"
	recOffer = "11111111-1111-4111-8111-111111111111"
	recSpace = "sensitive-vendor-text"
)

type recClient struct {
	items     []insurance.LineItem
	cmp       *insurance.Comparison
	itemsErr  error
	cmpErr    error
	hang      bool
	itemCalls int
	cmpCalls  int
	request   insurance.ComparisonRequest
}

func (c *recClient) LineItems(ctx context.Context, _ string) ([]insurance.LineItem, error) {
	c.itemCalls++
	if c.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.items, c.itemsErr
}

func (c *recClient) Comparison(_ context.Context, r insurance.ComparisonRequest) (*insurance.Comparison, error) {
	c.cmpCalls++
	c.request = r
	return c.cmp, c.cmpErr
}

func (c *recClient) Plan(context.Context, string) (*insurance.Plan, error) {
	panic("unexpected plan read")
}

type recProvider struct {
	client insurance.QuoteClient
	keys   int
}

func (p *recProvider) Status() archera.Status { return archera.Status{Configured: true} }
func (p *recProvider) PlanID() string         { return insTestPlan }
func (p *recProvider) Client(context.Context) (insurance.QuoteClient, error) {
	p.keys++
	return p.client, nil
}

type quoteOnly struct{ insurance.QuoteClient }

func recRecord() config.RecommendationRecord {
	source, cloud := "Source-account-a", "cloud-account-a"
	return config.RecommendationRecord{ID: "stored-a", Provider: "aws", Service: "savings-plans-ec2instance", Region: "us-east-1",
		SourceAccountID: &source, CloudAccountID: &cloud, Term: 1, Payment: "no-upfront",
		Details: []byte(`{"offering_id":"provider-offer-a","plan_type":"EC2Instance","region":"us-east-1","instance_family":"m5","hourly_commitment":0.5}`)}
}

func recVendor(t *testing.T) ([]insurance.LineItem, *insurance.Comparison) {
	t.Helper()
	item := `{"id":"` + recItem + `","plan_id":"` + insTestPlan + `","offer_id":"` + recOffer + `","account_id":"Source-account-a","selected_quantity":7,"selected_commitment":0.5,"is_selected":true,"contract_term":"one_year_gris","payment_option":"No Upfront","contract_spec":{"commitment_type":"x"},"offer":{"id":"` + recOffer + `","type":"savings_plan","provider":"aws","provider_offering_id":"provider-offer-a","offering_id":"cat","duration_seconds":31536000,"region":"us-east-1","az":null,"instance_type":null,"instance_family":"m5","product_description":null,"tenancy":null,"plan_type":"EC2Instance","is_multi_az":null,"is_flexible":null,"payment_option":"No Upfront"}}`
	items, err := insurance.DecodeLineItems(strings.NewReader("["+item+"]"), insTestPlan)
	require.NoError(t, err)
	cur := `{"is_current":true,"offer_id":"` + recOffer + `","selected_amount":0.5,"commitment_type":"savings_plan","contract_term":"one_year_gris","payment_option":"no_upfront","offer":{"provider":"aws","region":"us-east-1"},"discount_rate":0,"commitment_upfront_cost":0,"commitment_financials_monthly_rate":{},"delta_vs_current":{"monthly_net_savings":0,"upfront_cost":0,"discount_rate":0}}`
	body := `{"current_totals":{"commitment_financials_monthly_rate":{},"commitment_upfront_cost":0},"hypothetical_totals":[],"data":[{"line_item_id":"` + recItem + `","current":` + cur + `,"candidates":[]}]}`
	cmp, err := insurance.DecodeComparison(strings.NewReader(body), "org", insTestPlan, time.Unix(1, 0))
	require.NoError(t, err)
	return items, cmp
}

func fetchRig(t *testing.T) (*Handler, *recClient, *recProvider, *archera.RecommendationIdentity) {
	t.Helper()
	items, cmp := recVendor(t)
	c := &recClient{items: items, cmp: cmp}
	p := &recProvider{client: c}
	row := recRecord()
	rec, err := archera.ProjectRecommendation(&row)
	require.NoError(t, err)
	return &Handler{insurance: p}, c, p, rec
}

func TestFetchRecommendationComparisonScopesToSelectedItemWithAllPayments(t *testing.T) {
	h, c, _, rec := fetchRig(t)
	cmp, items, err := h.FetchRecommendationComparison(context.Background(), rec)
	require.NoError(t, err)
	require.NotNil(t, cmp)
	require.Len(t, items, 1)
	require.Equal(t, []string{recItem}, c.request.LineItemIDs)
	require.Equal(t, insTestPlan, c.request.PlanID)
	require.ElementsMatch(t, []insurance.PaymentOption{insurance.PaymentNoUpfront, insurance.PaymentPartialUpfront, insurance.PaymentAllUpfront}, c.request.PaymentOptions)
}

func TestFetchRecommendationComparisonNoneOrAmbiguousSkipsComparison(t *testing.T) {
	for name, edit := range map[string]func(*recClient){
		"none": func(c *recClient) { c.items = nil },
		"ambiguous": func(c *recClient) {
			twin := c.items[0]
			twin.ID = "33333333-3333-4333-8333-333333333333"
			c.items = append(c.items, twin)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, c, _, rec := fetchRig(t)
			edit(c)
			cmp, _, err := h.FetchRecommendationComparison(context.Background(), rec)
			require.NoError(t, err)
			require.NotNil(t, cmp)
			require.Empty(t, cmp.Rows)
			require.Zero(t, c.cmpCalls)
		})
	}
}

func TestFetchRecommendationComparisonFailuresReturnNoPartialResult(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*recClient, *recProvider)
		code  int
		retry string
	}{
		"line_items": {func(c *recClient, _ *recProvider) { c.itemsErr = errors.New(recSpace) }, 502, ""},
		"comparison": {func(c *recClient, _ *recProvider) { c.cmpErr = errors.New(recSpace) }, 502, ""},
		"rate_limited": {func(c *recClient, _ *recProvider) {
			c.itemsErr = &insurance.HTTPError{StatusCode: 429, RetryAfter: 3 * time.Second}
		}, 429, "3"},
		"unsupported": {func(c *recClient, p *recProvider) { p.client = quoteOnly{c} }, 503, ""},
		"deadline":    {func(c *recClient, _ *recProvider) { c.hang = true }, 504, ""},
	} {
		t.Run(name, func(t *testing.T) {
			prev := insuranceRecommendationTimeout
			insuranceRecommendationTimeout = 50 * time.Millisecond
			t.Cleanup(func() { insuranceRecommendationTimeout = prev })
			h, c, p, rec := fetchRig(t)
			tc.setup(c, p)
			cmp, items, err := h.FetchRecommendationComparison(context.Background(), rec)
			require.Nil(t, cmp)
			require.Nil(t, items)
			ce, ok := IsClientError(err)
			require.True(t, ok, "got %v", err)
			require.Equal(t, tc.code, ce.code)
			require.NotContains(t, ce.message, recSpace)
			if tc.retry != "" {
				require.Equal(t, tc.retry, ce.headers["Retry-After"])
			}
		})
	}
}
