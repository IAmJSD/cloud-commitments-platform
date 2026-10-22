package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type issue235Store struct{ mockOverrideStore }

func (s *issue235Store) GetGlobalConfig(context.Context) (*config.GlobalConfig, error) {
	return &config.GlobalConfig{EnabledProviders: []string{"aws"}, DefaultTerm: 1, DefaultPayment: "all-upfront"}, nil
}

func (s *issue235Store) UpsertRecommendations(_ context.Context, _ time.Time, recs []config.RecommendationRecord, _ []config.SuccessfulCollect) error {
	s.recs = append([]config.RecommendationRecord(nil), recs...)
	return nil
}

func (s *issue235Store) ListStoredRecommendations(_ context.Context, filter config.RecommendationFilter) ([]config.RecommendationRecord, error) {
	var found []config.RecommendationRecord
	for _, r := range s.recs {
		if filter.ID == "" || r.ID == filter.ID {
			found = append(found, r)
		}
	}
	return found, nil
}

func TestRecommendationIdentity_TwoRegisteredAccounts(t *testing.T) {
	for _, reported := range []string{"", "999999999999"} {
		t.Run("provider_account="+reported, func(t *testing.T) {
			ctx := context.Background()
			store := &issue235Store{}
			accounts := []config.CloudAccount{
				{ID: "11111111-1111-4111-8111-111111111111", Provider: "aws", ExternalID: "111111111111", AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::111111111111:role/cudly", Enabled: true},
				{ID: "22222222-2222-4222-8222-222222222222", Provider: "aws", ExternalID: "222222222222", AWSAuthMode: "role_arn", AWSRoleARN: "arn:aws:iam::222222222222:role/cudly", Enabled: true},
			}
			store.On("ListCloudAccounts", mock.Anything, mock.Anything).Return(accounts, nil)
			factory := new(MockProviderFactory)
			prov := new(MockProvider)
			client := new(MockRecommendationsClient)
			factory.On("CreateAndValidateProvider", mock.Anything, "aws", mock.Anything).Return(prov, nil)
			prov.On("GetRecommendationsClient", mock.Anything).Return(client, nil)
			client.On("GetAllRecommendations", mock.Anything).Return([]common.Recommendation{{Provider: common.ProviderAWS, Account: reported, Service: common.ServiceEC2, Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront"}}, nil)
			s := &Scheduler{config: store, providerFactory: factory}
			result, err := s.CollectRecommendations(ctx, "")
			require.NoError(t, err)
			require.Equal(t, 2, result.Recommendations)
			require.Len(t, store.recs, 2)
			assert.NotEqual(t, store.recs[0].ID, store.recs[1].ID)
			ids := make(map[string]string)
			for _, want := range store.recs {
				ids[*want.CloudAccountID] = want.ID
				got, _, lookupErr := s.GetRecommendationByID(ctx, want.ID)
				require.NoError(t, lookupErr)
				require.NotNil(t, got)
				assert.Equal(t, want, *got)
			}
			_, err = s.CollectRecommendations(ctx, "")
			require.NoError(t, err)
			require.Len(t, store.recs, 2)
			for _, got := range store.recs {
				assert.Equal(t, ids[*got.CloudAccountID], got.ID)
			}
		})
	}
}

func TestRecommendationIdentity_Tagging(t *testing.T) {
	for _, cloud := range []string{"aws", "azure", "gcp"} {
		t.Run(cloud, func(t *testing.T) {
			s := &Scheduler{}
			rec := common.Recommendation{Service: common.ServiceEC2, Region: "region", ResourceType: "sku", Term: "1yr", PaymentOption: "all-upfront"}
			payment := map[string]string{"aws": "all-upfront", "azure": "upfront", "gcp": "monthly"}[cloud]
			ambient := s.convertRecommendations([]common.Recommendation{rec}, cloud)
			require.Len(t, ambient, 1)
			assert.Nil(t, ambient[0].CloudAccountID)
			assert.Equal(t, cloud+"||ec2|region|sku||1|"+payment, ambient[0].ID)
			first := s.tagAccount(ambient, "11111111-1111-4111-8111-111111111111")
			want := cloud + "|11111111-1111-4111-8111-111111111111|ec2|region|sku||1|" + payment
			assert.Equal(t, want, first[0].ID)
			assert.Equal(t, want, s.tagAccount(first, *first[0].CloudAccountID)[0].ID)
			rec.Account = "provider-reported-account"
			changed := s.convertRecommendations([]common.Recommendation{rec}, cloud)
			assert.Equal(t, want, s.tagAccount(changed, *first[0].CloudAccountID)[0].ID)
			retagged := s.tagAccount(first, "22222222-2222-4222-8222-222222222222")
			assert.Equal(t, cloud+"|22222222-2222-4222-8222-222222222222|ec2|region|sku||1|"+payment, retagged[0].ID)
		})
	}
}

func TestRecommendationIdentity_NaturalKeyDimensions(t *testing.T) {
	base := config.RecommendationRecord{Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.m5.large", Engine: "mysql", Term: 1, Payment: "all-upfront"}
	for name, change := range map[string]func(*config.RecommendationRecord){
		"account":       func(r *config.RecommendationRecord) { r.CloudAccountID = new("11111111-1111-4111-8111-111111111111") },
		"provider":      func(r *config.RecommendationRecord) { r.Provider = "azure" },
		"service":       func(r *config.RecommendationRecord) { r.Service = "ec2" },
		"region":        func(r *config.RecommendationRecord) { r.Region = "us-west-2" },
		"resource type": func(r *config.RecommendationRecord) { r.ResourceType = "db.m5.xlarge" },
		"engine":        func(r *config.RecommendationRecord) { r.Engine = "postgres" },
		"term":          func(r *config.RecommendationRecord) { r.Term = 3 },
		"payment":       func(r *config.RecommendationRecord) { r.Payment = "no-upfront" },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			change(&other)
			baseID, err := recommendationID(base)
			require.NoError(t, err)
			otherID, err := recommendationID(other)
			require.NoError(t, err)
			assert.NotEqual(t, baseID, otherID)
		})
	}
}

// A zonal EC2 rec keeps its AZ in the persisted details blob, so the purchase
// path can buy in that AZ (go#305).
func TestConvertRecommendations_ZonalEC2KeepsAvailabilityZoneInDetails(t *testing.T) {
	s := &Scheduler{}
	rec := common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceEC2, Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront",
		Details: &common.ComputeDetails{
			InstanceType: "m5.large", Platform: "Linux/UNIX", Tenancy: "default",
			Scope: "availability-zone", AvailabilityZone: "us-east-1b",
		},
	}
	records := s.convertRecommendations([]common.Recommendation{rec}, "aws")
	require.Len(t, records, 1)
	assert.Contains(t, string(records[0].Details), `"availability_zone":"us-east-1b"`)
}

func ec2Rec(platform, tenancy string) common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceEC2, Region: "us-east-1", ResourceType: "m5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront",
		Details: &common.ComputeDetails{InstanceType: "m5.large", Platform: platform, Tenancy: tenancy, Scope: "regional"},
	}
}

// Two EC2 recs for the same type and term that differ only by platform or
// tenancy must get distinct IDs (issue #806). The ID carries the verbatim AWS
// platform string and always the platform/tenancy pair.
func TestConvertRecommendations_EC2PlatformAndTenancyInID(t *testing.T) {
	s := &Scheduler{}
	records := s.convertRecommendations([]common.Recommendation{
		ec2Rec("Linux/UNIX", "default"), ec2Rec("Windows", "default"),
		ec2Rec("Linux/UNIX", "dedicated"), ec2Rec("Red Hat Enterprise Linux", "default"),
	}, "aws")
	require.Len(t, records, 4)
	ids := map[string]bool{}
	for _, r := range records {
		ids[r.ID] = true
	}
	assert.Len(t, ids, 4)
	assert.Equal(t, "aws||ec2|us-east-1|m5.large||1|all-upfront|Linux/UNIX|default", records[0].ID)
	assert.Equal(t, "aws||ec2|us-east-1|m5.large||1|all-upfront|Red Hat Enterprise Linux|default", records[3].ID)
}

// The ID survives tagAccount's recompute with the pair intact.
func TestTagAccount_KeepsPlatformTenancyPair(t *testing.T) {
	s := &Scheduler{}
	recs := s.convertRecommendations([]common.Recommendation{ec2Rec("Windows", "dedicated")}, "aws")
	tagged := s.tagAccount(recs, "acct")
	require.Len(t, tagged, 1)
	assert.Equal(t, "aws|acct|ec2|us-east-1|m5.large||1|all-upfront|Windows|dedicated", tagged[0].ID)
}

// A rec with no platform/tenancy (non-compute, legacy) keeps its pre-#806 ID.
func TestRecommendationID_NoComputeIdentityIsUnchanged(t *testing.T) {
	rec := config.RecommendationRecord{Provider: "aws", Service: "rds", Region: "us-east-1", ResourceType: "db.m5.large", Engine: "postgres", Term: 1, Payment: "all-upfront"}
	id, err := recommendationID(rec)
	require.NoError(t, err)
	assert.Equal(t, "aws||rds|us-east-1|db.m5.large|postgres|1|all-upfront", id)
}

// A rec whose Details cannot be read is dropped loudly, never emitted with an
// ID missing its pair.
func TestRecommendationID_MalformedDetailsErrorsAndTagAccountDrops(t *testing.T) {
	bad := config.RecommendationRecord{Provider: "aws", Service: "ec2", Details: []byte(`{"platform":5}`)}
	_, err := recommendationID(bad)
	require.Error(t, err)
	s := &Scheduler{}
	assert.Empty(t, s.tagAccount([]config.RecommendationRecord{bad}, "acct"))
}

func rdsAZConfigRec(azConfig string) common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceRDS, Region: "us-east-1", ResourceType: "db.r5.large", Count: 1, Term: "1yr", PaymentOption: "all-upfront",
		Details: &common.DatabaseDetails{Engine: "mysql", AZConfig: azConfig, InstanceClass: "db.r5.large"},
	}
}

// Two RDS recs for the same class, engine and term that differ only by
// Single-AZ vs Multi-AZ must get distinct IDs (issue #408); before, both got
// "aws||rds|us-east-1|db.r5.large|mysql|1|all-upfront". A database rec without
// az_config keeps its pre-#408 ID.
func TestConvertRecommendations_RDSAZConfigInID(t *testing.T) {
	s := &Scheduler{}
	records := s.convertRecommendations([]common.Recommendation{rdsAZConfigRec("single-az"), rdsAZConfigRec("multi-az"), rdsAZConfigRec("")}, "aws")
	require.Len(t, records, 3)
	assert.Equal(t, "aws||rds|us-east-1|db.r5.large|mysql|1|all-upfront|single-az", records[0].ID)
	assert.Equal(t, "aws||rds|us-east-1|db.r5.large|mysql|1|all-upfront|multi-az", records[1].ID)
	assert.Equal(t, "aws||rds|us-east-1|db.r5.large|mysql|1|all-upfront", records[2].ID)

	tagged := s.tagAccount(records[1:2], "acct")
	require.Len(t, tagged, 1)
	assert.Equal(t, "aws|acct|rds|us-east-1|db.r5.large|mysql|1|all-upfront|multi-az", tagged[0].ID)
}

// An unreadable az_config is dropped loudly, never emitted with an ID that
// collides with its sibling.
func TestRecommendationID_MalformedAZConfigErrors(t *testing.T) {
	bad := config.RecommendationRecord{Provider: "aws", Service: "rds", Details: []byte(`{"az_config":5}`)}
	_, err := recommendationID(bad)
	require.Error(t, err)
	s := &Scheduler{}
	assert.Empty(t, s.tagAccount([]config.RecommendationRecord{bad}, "acct"))
}
