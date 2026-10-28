package api

import (
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// overrideRowRejection is a row-level refusal, reported rather than returned.
type overrideRowRejection struct{ msg string }

func (e *overrideRowRejection) Error() string { return e.msg }

func rejectRow(format string, args ...any) error {
	return &overrideRowRejection{msg: fmt.Sprintf(format, args...)}
}

func isRowRejection(err error) bool {
	var rej *overrideRowRejection
	return errors.As(err, &rej)
}

// overrideAccountIndex looks up the in-scope accounts an import row can name.
// Out-of-scope accounts are absent, so a row naming one reads as unknown and
// does not reveal that the account exists.
type overrideAccountIndex struct {
	byID       map[string]*config.CloudAccount
	byExternal map[string][]*config.CloudAccount
}

func newOverrideAccountIndex(accounts []config.CloudAccount) *overrideAccountIndex {
	idx := &overrideAccountIndex{
		byID:       make(map[string]*config.CloudAccount, len(accounts)),
		byExternal: make(map[string][]*config.CloudAccount, len(accounts)),
	}
	for i := range accounts {
		a := &accounts[i]
		idx.byID[a.ID] = a
		key := a.Provider + ":" + a.ExternalID
		idx.byExternal[key] = append(idx.byExternal[key], a)
	}
	return idx
}

// resolve finds the row's account: account_id first, then provider plus
// account_external_id (an account_id from another tenant matches nothing
// here, so the portable key takes over). The account's provider must be the
// row's provider.
func (idx *overrideAccountIndex) resolve(row *OverrideTransferRow) (*config.CloudAccount, error) {
	if !validAccountProviders[row.Provider] {
		return nil, rejectRow("invalid provider %q: must be one of aws, azure, gcp", row.Provider)
	}
	if a, ok := idx.byID[row.AccountID]; ok {
		return checkResolvedAccount(a, row)
	}
	if row.AccountExternalID == "" {
		return nil, rejectRow("unknown account %q and no account_external_id to match on", row.AccountID)
	}
	matches := idx.byExternal[row.Provider+":"+row.AccountExternalID]
	switch len(matches) {
	case 0:
		return nil, rejectRow("no %s account with external ID %q", row.Provider, row.AccountExternalID)
	case 1:
		return matches[0], nil
	default:
		return nil, rejectRow("%d %s accounts share external ID %q; name the account by account_id", len(matches), row.Provider, row.AccountExternalID)
	}
}

// checkResolvedAccount refuses an account_id whose account belongs to another
// provider, or whose external ID disagrees with the row's.
func checkResolvedAccount(a *config.CloudAccount, row *OverrideTransferRow) (*config.CloudAccount, error) {
	if a.Provider != row.Provider {
		return nil, rejectRow("account %s is a %s account, not %s", a.ID, a.Provider, row.Provider)
	}
	if row.AccountExternalID != "" && row.AccountExternalID != a.ExternalID {
		return nil, rejectRow("account %s has external ID %q, not %q", a.ID, a.ExternalID, row.AccountExternalID)
	}
	return a, nil
}
