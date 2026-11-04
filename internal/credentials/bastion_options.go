package credentials

import (
	"context"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// CloudAccountGetter is the config-store method the bastion path needs to load
// the bastion account. config.StoreInterface satisfies it.
type CloudAccountGetter interface {
	GetCloudAccount(ctx context.Context, id string) (*config.CloudAccount, error)
}

// AccountLookupFromStore returns an AccountLookupFunc backed by store, or nil
// when store is nil (bastion resolution then fails closed instead of
// panicking on a nil store).
func AccountLookupFromStore(store CloudAccountGetter) AccountLookupFunc {
	if store == nil {
		return nil
	}
	return store.GetCloudAccount
}

// NewSTSClientFactory returns an STSClientFactory that builds an STS client
// from base (region, endpoint, HTTP client, retryer) signed with the given
// provider instead of base's own credentials. The provider is cached so a
// role-based bastion is not re-assumed on every STS call.
func NewSTSClientFactory(base aws.Config) STSClientFactory {
	return func(provider aws.CredentialsProvider) STSClient {
		cfg := base.Copy()
		if _, cached := provider.(*aws.CredentialsCache); cached {
			cfg.Credentials = provider
		} else {
			cfg.Credentials = aws.NewCredentialsCache(provider)
		}
		return sts.NewFromConfig(cfg)
	}
}
