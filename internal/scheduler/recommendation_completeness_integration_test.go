//go:build integration

package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/database/postgres/testhelpers"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer safe for concurrent use. The test captures
// the process-wide logger, and connection pools from other subtests on the
// shared Postgres server (#1400) can still log from background goroutines
// (pgxpool's idle-connection creation) while the test reads the capture.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type completenessHTTP struct {
	resourceType string
	accounts     []any
	details      []string
	fallback     []string
	failScope    bool
	spMode       string
	spFallback   string
	rdsCalls     atomic.Int32
	unexpected   atomic.Int32
	mu           sync.Mutex
	requests     []completenessRequest
}

type completenessRequest struct {
	Operation            string
	Service              string
	LookbackPeriodInDays string
	TermInYears          string
	PaymentOption        string
	SavingsPlansType     string
	NextPageToken        string
	AccountScope         string
}

func (f *completenessHTTP) Do(req *http.Request) (*http.Response, error) {
	var input completenessRequest
	if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
		return nil, err
	}
	input.Operation = req.Header.Get("X-Amz-Target")
	f.mu.Lock()
	f.requests = append(f.requests, input)
	f.mu.Unlock()
	body := any(map[string]any{})
	status := http.StatusOK
	switch req.Header.Get("X-Amz-Target") {
	case "AWSInsightsIndexService.GetReservationPurchaseRecommendation":
		allowed := map[string]bool{
			"Amazon Elastic Compute Cloud - Compute": true, "Amazon Relational Database Service": true,
			"Amazon ElastiCache": true, "Amazon OpenSearch Service": true, "Amazon Redshift": true, "Amazon MemoryDB Service": true,
		}
		if !allowed[input.Service] {
			f.unexpected.Add(1)
			return nil, fmt.Errorf("invalid Cost Explorer Service %q", input.Service)
		}
		details := []any{}
		if input.Service == "Amazon Relational Database Service" {
			call := f.rdsCalls.Add(1)
			quantities := f.details
			if input.LookbackPeriodInDays == "THIRTY_DAYS" {
				quantities = f.fallback
			}
			for i, quantity := range quantities {
				detail := map[string]any{
					"RecommendedNumberOfInstancesToPurchase": quantity,
					"EstimatedMonthlySavingsAmount":          "10", "EstimatedMonthlyOnDemandCost": "30",
					"InstanceDetails": map[string]any{"RDSInstanceDetails": map[string]any{
						"InstanceType": f.resourceType, "Region": "us-east-1", "DeploymentOption": "Single-AZ",
					}},
				}
				if i < len(f.accounts) {
					detail["AccountId"] = f.accounts[i]
				}
				details = append(details, detail)
			}
			if f.failScope && call == 1 {
				status = http.StatusBadRequest
			}
		}
		body = map[string]any{"Recommendations": []any{map[string]any{"RecommendationDetails": details}}}
	case "AWSInsightsIndexService.GetSavingsPlansPurchaseRecommendation":
		body, status = f.savingsPlansResponse(input)
	case "AWSInsightsIndexService.GetReservationCoverage":
	default:
		f.unexpected.Add(1)
		return nil, fmt.Errorf("unexpected AWS operation %s", req.Header.Get("X-Amz-Target"))
	}
	if status != http.StatusOK {
		body = map[string]any{"__type": "AccessDeniedException", "message": "synthetic scope failure"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/x-amz-json-1.1"}},
		Body: io.NopCloser(strings.NewReader(string(raw))), Request: req}, nil
}

func (f *completenessHTTP) savingsPlansResponse(input completenessRequest) (any, int) {
	mode := f.spMode
	if input.LookbackPeriodInDays == "THIRTY_DAYS" {
		mode = f.spFallback
	}
	if mode == "failed type" && input.SavingsPlansType == "DATABASE_SP" ||
		mode == "late page" && input.NextPageToken == "next" {
		return nil, http.StatusBadRequest
	}
	details := []any{}
	if mode != "" && mode != "empty" {
		commitments := []string{"2"}
		if mode == "invalid" {
			commitments = []string{"invalid"}
		} else if mode == "mixed" && input.SavingsPlansType == "COMPUTE_SP" {
			commitments = append(commitments, "invalid")
		}
		for _, commitment := range commitments {
			details = append(details, map[string]any{"HourlyCommitmentToPurchase": commitment,
				"EstimatedMonthlySavingsAmount": "10", "UpfrontCost": "3",
				"CurrentAverageHourlyOnDemandSpend": "4", "EstimatedSavingsPercentage": "25"})
		}
	}
	body := map[string]any{"SavingsPlansPurchaseRecommendation": map[string]any{
		"SavingsPlansPurchaseRecommendationDetails": details}}
	if mode == "late page" && input.SavingsPlansType == "COMPUTE_SP" {
		body["NextPageToken"] = "next"
	}
	return body, http.StatusOK
}

func completenessProvider(t *testing.T, fixture *completenessHTTP) *MockProvider {
	t.Helper()
	client := awsprovider.NewRecommendationsClient(aws.Config{
		Region: "us-east-1", HTTPClient: fixture, Retryer: func() aws.Retryer { return aws.NopRetryer{} },
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "synthetic", SecretAccessKey: "synthetic"}, nil
		}),
	})
	prov := new(MockProvider)
	prov.On("GetRecommendationsClient", mock.Anything).Return(client, nil).Once()
	t.Cleanup(func() {
		prov.AssertExpectations(t)
		require.Zero(t, fixture.unexpected.Load())
		require.Positive(t, fixture.rdsCalls.Load())
	})
	return prov
}

func TestAWSRecommendationCompletenessPersistence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		registered    bool
		details       []string
		fallback      []string
		failScope     bool
		complete      bool
		wantRows      int
		spMode        string
		spFallback    string
		failedDetails int
		failedScopes  int
	}{
		{name: "ambient mixed", details: []string{"2", "invalid"}, wantRows: 6},
		{name: "ambient all invalid", details: []string{"invalid"}},
		{name: "ambient valid", details: []string{"2"}, complete: true, wantRows: 6},
		{name: "ambient empty", complete: true},
		{name: "configured lookback returns valid fallback", fallback: []string{"2"}, complete: true, wantRows: 1},
		{name: "configured lookback remains empty", fallback: []string{}, complete: true},
		{name: "ambient all invalid then clean fallback", details: []string{"invalid"}, fallback: []string{"2"}, wantRows: 1},
		{name: "ambient empty then invalid fallback", fallback: []string{"invalid"}},
		{name: "ambient failed API scope", details: []string{"2"}, failScope: true, wantRows: 5},
		{name: "registered mixed", registered: true, details: []string{"2", "invalid"}, wantRows: 6},
		{name: "registered all invalid", registered: true, details: []string{"invalid"}},
		{name: "SP valid", spMode: "valid", complete: true, wantRows: 24},
		{name: "SP empty", spMode: "empty", complete: true},
		{name: "SP mixed", spMode: "mixed", wantRows: 24, failedDetails: 6},
		{name: "SP all invalid", spMode: "invalid", failedDetails: 24},
		{name: "SP failed type", spMode: "failed type", wantRows: 18, failedScopes: 6},
		{name: "SP late page", spMode: "late page", wantRows: 24, failedScopes: 6},
		{name: "SP invalid then clean fallback", spMode: "invalid", spFallback: "valid", fallback: []string{}, wantRows: 4, failedDetails: 24},
		{name: "SP empty then incomplete fallback", spMode: "empty", spFallback: "mixed", fallback: []string{}, wantRows: 4, failedDetails: 1},
		{name: "SP clean fallback", spMode: "empty", spFallback: "valid", fallback: []string{}, complete: true, wantRows: 4},
		{name: "SP empty fallback", spMode: "empty", spFallback: "empty", fallback: []string{}, complete: true},
		{name: "SP registered mixed", registered: true, spMode: "mixed", wantRows: 24, failedDetails: 6},
		{name: "SP registered failed type", registered: true, spMode: "failed type", wantRows: 18, failedScopes: 6},
		{name: "SP registered late page", registered: true, spMode: "late page", wantRows: 24, failedScopes: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var diagnostics lockedBuffer
			previous := logging.SetOutput(&diagnostics)
			t.Cleanup(func() { logging.SetOutput(previous) })
			container := testhelpers.RequireMigratedDatabase(ctx, t, "../database/postgres/migrations")
			store := config.NewPostgresStore(container.DB)
			factory := new(MockProviderFactory)
			fixture := &completenessHTTP{resourceType: "db.t3.medium", details: tc.details, fallback: tc.fallback, failScope: tc.failScope, spMode: tc.spMode, spFallback: tc.spFallback}
			factory.On("CreateAndValidateProvider", mock.Anything, "aws", (*provider.ProviderConfig)(nil)).
				Return(completenessProvider(t, fixture), nil).Once()
			var accountID *string
			var completeAccountID string
			uncollectedID := uuid.NewString()
			if tc.spMode != "" && !tc.registered {
				acct := config.CloudAccount{ID: uncollectedID, Name: "disabled", Provider: "aws", ExternalID: "333333333333", Enabled: false, AWSAuthMode: "role_arn"}
				require.NoError(t, store.CreateCloudAccount(ctx, &acct))
			}
			if tc.registered {
				id := uuid.NewString()
				accountID = &id
				completeAccountID = uuid.NewString()
				for _, acct := range []config.CloudAccount{
					{ID: id, Name: "incomplete", Provider: "aws", ExternalID: "111111111111", Enabled: true, AWSAuthMode: "role_arn"},
					{ID: completeAccountID, Name: "complete", Provider: "aws", ExternalID: "222222222222", Enabled: true, AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::222222222222:role/synthetic"},
					{ID: uncollectedID, Name: "disabled", Provider: "aws", ExternalID: "333333333333", Enabled: false, AWSAuthMode: "role_arn"},
				} {
					require.NoError(t, store.CreateCloudAccount(ctx, &acct))
				}
				cleanFixture := &completenessHTTP{resourceType: "db.r5.large", details: []string{"2"}}
				if tc.spMode != "" {
					cleanFixture = &completenessHTTP{spMode: "valid"}
				}
				factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.MatchedBy(func(cfg *provider.ProviderConfig) bool { return cfg != nil })).
					Return(completenessProvider(t, cleanFixture), nil).Once()
			}
			seed := []config.RecommendationRecord{
				{ID: "missing-offer", Provider: "aws", CloudAccountID: accountID, Service: "rds", Region: "us-east-1", ResourceType: "db.t3.large", Savings: 40, Count: 1, Term: 12, Payment: "no-upfront"},
				{ID: "unswept-provider", Provider: "azure", Service: "vm", Region: "eastus", ResourceType: "D2", Savings: 20, Count: 1, Term: 12, Payment: "no-upfront"},
			}
			if tc.registered {
				seed = append(seed, config.RecommendationRecord{ID: "complete-stale", Provider: "aws", CloudAccountID: &completeAccountID,
					Service: "rds", Region: "us-east-1", ResourceType: "db.t3.large", Savings: 40, Count: 1, Term: 12, Payment: "no-upfront"})
				for _, scope := range []struct {
					id      string
					account *string
				}{{"uncollected", &uncollectedID}, {"ambient", nil}} {
					seed = append(seed, config.RecommendationRecord{ID: scope.id, Provider: "aws", CloudAccountID: scope.account,
						Service: "savings-plans-compute", Savings: 40, Count: 1, Term: 1, Payment: "no-upfront"})
				}
			}
			if tc.spMode != "" && !tc.registered {
				seed = append(seed, config.RecommendationRecord{ID: "uncollected", Provider: "aws", CloudAccountID: &uncollectedID,
					Service: "savings-plans-compute", Savings: 40, Count: 1, Term: 1, Payment: "no-upfront"})
			}
			require.NoError(t, store.UpsertRecommendations(ctx, time.Now().Add(-time.Hour), seed, nil))
			s := &Scheduler{config: store, providerFactory: factory, stsClient: &fakeSTSClient{accountID: "111111111111"}}
			var globalCfg *config.GlobalConfig
			if tc.fallback != nil {
				globalCfg = &config.GlobalConfig{DefaultTerm: 1, DefaultPayment: "no-upfront", RecommendationsLookbackDays: 30}
			}
			recs, ids, _, err := s.collectAWSRecommendations(ctx, globalCfg)
			require.NoError(t, err)
			wantRows := tc.wantRows
			switch {
			case tc.registered:
				if tc.spMode == "" {
					wantRows += 6
				} else {
					wantRows += 24
				}
				require.Equal(t, []string{completeAccountID}, ids)
			case tc.complete:
				require.Equal(t, []string{""}, ids)
			default:
				require.Empty(t, ids)
			}
			require.Len(t, recs, wantRows)
			s.persistCollection(ctx, recs, expandSuccessfulCollects("aws", ids), nil, nil)
			rows, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{})
			require.NoError(t, err)
			byID := make(map[string]config.RecommendationRecord, len(rows))
			for _, row := range rows {
				byID[row.ID] = row
			}
			_, missing := byID["missing-offer"]
			require.Equal(t, !tc.complete, missing, "incomplete collection must retain previous offers")
			require.Contains(t, byID, "unswept-provider")
			require.NotContains(t, byID, "complete-stale")
			for _, rec := range recs {
				require.Contains(t, byID, rec.ID, "surviving SDK recommendations must be persisted")
				require.Equal(t, rec.CloudAccountID, byID[rec.ID].CloudAccountID)
			}
			if tc.spMode != "" {
				assertSavingsPlansPersistence(t, rows, accountID, tc.wantRows, completeAccountID, tc.spMode, tc.fallback != nil)
				require.Contains(t, byID, "uncollected")
				if tc.failedDetails+tc.failedScopes > 0 {
					require.Contains(t, diagnostics.String(), fmt.Sprintf("failed_details=%d failed_scopes=%d", tc.failedDetails, tc.failedScopes))
				} else {
					require.NotContains(t, diagnostics.String(), "recommendations incomplete")
				}
				if tc.registered {
					require.Contains(t, byID, "uncollected")
					require.Contains(t, byID, "ambient")
					for _, id := range []string{*accountID, completeAccountID} {
						filtered, err := store.ListStoredRecommendations(ctx, config.RecommendationFilter{AccountIDs: []string{id}})
						require.NoError(t, err)
						count := 24
						if id == *accountID {
							count = tc.wantRows + 1
						}
						require.Len(t, filtered, count)
						for _, row := range filtered {
							require.Equal(t, &id, row.CloudAccountID)
						}
					}
				}
			}
			fallbackServices := []string{}
			for _, request := range fixture.requests {
				if request.Operation == "AWSInsightsIndexService.GetReservationCoverage" {
					continue
				}
				require.Contains(t, []string{"SEVEN_DAYS", "THIRTY_DAYS"}, request.LookbackPeriodInDays)
				if request.LookbackPeriodInDays == "SEVEN_DAYS" {
					continue
				}
				require.Equal(t, "ONE_YEAR", request.TermInYears)
				require.Equal(t, "NO_UPFRONT", request.PaymentOption)
				service := request.Service
				if request.Operation == "AWSInsightsIndexService.GetSavingsPlansPurchaseRecommendation" {
					service = "SP:" + request.SavingsPlansType
				}
				fallbackServices = append(fallbackServices, service)
			}
			if tc.fallback != nil {
				require.Equal(t, int32(7), fixture.rdsCalls.Load())
				require.ElementsMatch(t, []string{
					"Amazon Elastic Compute Cloud - Compute", "Amazon Relational Database Service", "Amazon ElastiCache",
					"Amazon OpenSearch Service", "Amazon Redshift", "SP:COMPUTE_SP", "SP:EC2_INSTANCE_SP", "SP:SAGEMAKER_SP", "SP:DATABASE_SP",
				}, fallbackServices)
			} else {
				require.Empty(t, fallbackServices)
			}
			if tc.spMode != "" {
				assertSavingsPlansRequests(t, fixture.requests, tc.fallback != nil, tc.spMode == "late page")
			}
			factory.AssertExpectations(t)
		})
	}
}

func assertSavingsPlansPersistence(t *testing.T, rows []config.RecommendationRecord, accountID *string, want int, cleanID, mode string, fallback bool) {
	t.Helper()
	count := 0
	actual := []string{}
	for _, row := range rows {
		if !strings.HasPrefix(row.ID, "aws|") || !strings.HasPrefix(row.Service, "savings-plans-") {
			continue
		}
		scope := ""
		if row.CloudAccountID != nil {
			scope = *row.CloudAccountID
		}
		if scope != cleanID || cleanID == "" {
			count++
		}
		actual = append(actual, row.ID)
		require.Contains(t, []string{"savings-plans-compute", "savings-plans-ec2instance", "savings-plans-sagemaker", "savings-plans-database"}, row.Service)
		require.Empty(t, row.Region)
		require.Empty(t, row.ResourceType)
		require.Empty(t, row.Engine)
		require.Equal(t, fmt.Sprintf("aws|%s|%s||||%d|%s", scope, row.Service, row.Term, row.Payment), row.ID)
		require.Contains(t, []int{1, 3}, row.Term)
		require.Contains(t, []string{"no-upfront", "partial-upfront", "all-upfront"}, row.Payment)
		require.Equal(t, 1, row.Count)
		require.Equal(t, 10.0, row.Savings)
		require.Equal(t, 3.0, row.UpfrontCost)
		require.NotNil(t, row.OnDemandCost)
		require.Equal(t, 2920.0, *row.OnDemandCost)
		require.NotNil(t, row.SavingsPercentage)
		require.Equal(t, 25.0, *row.SavingsPercentage)
		require.NotNil(t, row.MonthlyCost)
		// Fixture: $2/hr commitment, $3 upfront. Recurring = (2 x 8760 x years - 3) / (12 x years).
		monthly := (2.0*8760*float64(row.Term) - 3) / (12 * float64(row.Term))
		if row.Payment == "all-upfront" {
			monthly = 0
		}
		require.Equal(t, monthly, *row.MonthlyCost)
		if cleanID == "" {
			require.Equal(t, accountID, row.CloudAccountID)
		} else {
			require.Contains(t, []string{*accountID, cleanID}, scope)
		}
	}
	require.Equal(t, want, count)
	expected := []string{}
	accounts := []string{""}
	if accountID != nil {
		accounts = []string{*accountID, cleanID}
	}
	for _, account := range accounts {
		cleanSibling := cleanID != "" && account == cleanID
		if want == 0 && !cleanSibling {
			continue
		}
		for _, service := range []string{"savings-plans-compute", "savings-plans-ec2instance", "savings-plans-sagemaker", "savings-plans-database"} {
			if mode == "failed type" && service == "savings-plans-database" && !cleanSibling {
				continue
			}
			for _, term := range []int{1, 3} {
				for _, payment := range []string{"no-upfront", "partial-upfront", "all-upfront"} {
					if fallback && !cleanSibling && (term != 1 || payment != "no-upfront") {
						continue
					}
					expected = append(expected, fmt.Sprintf("aws|%s|%s||||%d|%s", account, service, term, payment))
				}
			}
		}
	}
	require.ElementsMatch(t, expected, actual)
}

func assertSavingsPlansRequests(t *testing.T, requests []completenessRequest, fallback, latePage bool) {
	t.Helper()
	actual := []string{}
	for _, request := range requests {
		if request.Operation != "AWSInsightsIndexService.GetSavingsPlansPurchaseRecommendation" {
			continue
		}
		require.Equal(t, "LINKED", request.AccountScope)
		actual = append(actual, strings.Join([]string{request.SavingsPlansType, request.TermInYears, request.PaymentOption, request.LookbackPeriodInDays, request.NextPageToken}, "/"))
	}
	expected := []string{}
	for _, plan := range []string{"COMPUTE_SP", "EC2_INSTANCE_SP", "SAGEMAKER_SP", "DATABASE_SP"} {
		for _, term := range []string{"ONE_YEAR", "THREE_YEARS"} {
			for _, payment := range []string{"NO_UPFRONT", "PARTIAL_UPFRONT", "ALL_UPFRONT"} {
				expected = append(expected, strings.Join([]string{plan, term, payment, "SEVEN_DAYS", ""}, "/"))
				if latePage && plan == "COMPUTE_SP" {
					expected = append(expected, strings.Join([]string{plan, term, payment, "SEVEN_DAYS", "next"}, "/"))
				}
			}
		}
		if fallback {
			expected = append(expected, plan+"/ONE_YEAR/NO_UPFRONT/THIRTY_DAYS/")
		}
	}
	require.ElementsMatch(t, expected, actual)
}
