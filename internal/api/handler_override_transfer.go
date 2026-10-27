package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// exportAccountServiceOverrides handles GET
// /api/accounts/service-overrides/export (issue #9). It returns every
// override on the accounts the caller's scope allows, as JSON
// (OverrideExport) or, with ?format=csv, as CSV.
func (h *Handler) exportAccountServiceOverrides(ctx context.Context, req *events.LambdaFunctionURLRequest) (any, error) {
	session, err := h.requirePermission(ctx, req, "view", "accounts")
	if err != nil {
		return nil, err
	}
	format := req.QueryStringParameters["format"]
	if format != "" && format != "json" && format != "csv" {
		return nil, NewClientError(400, "format must be json or csv")
	}
	accounts, err := h.scopedAccounts(ctx, session)
	if err != nil {
		return nil, err
	}
	rows, err := h.overrideTransferRows(ctx, accounts)
	if err != nil {
		return nil, err
	}
	if format != "csv" {
		return OverrideExport{Version: overrideExportVersion, ExportedAt: time.Now().UTC(), Overrides: rows}, nil
	}
	body, err := encodeOverridesCSV(rows)
	if err != nil {
		return nil, fmt.Errorf("accounts: encode overrides csv: %w", err)
	}
	return &rawResponse{contentType: "text/csv; charset=utf-8", body: body}, nil
}

// scopedAccounts lists the cloud accounts the session's account scope allows,
// ordered by provider, external ID and ID.
func (h *Handler) scopedAccounts(ctx context.Context, session *Session) ([]config.CloudAccount, error) {
	accts, err := h.config.ListCloudAccounts(ctx, config.CloudAccountFilter{})
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	scope, err := h.getAccountScope(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("failed to get allowed accounts: %w", err)
	}
	out := make([]config.CloudAccount, 0, len(accts))
	for i := range accts {
		if scope.Allows(accts[i].ID, accts[i].Name) {
			out = append(out, accts[i])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.ExternalID != b.ExternalID {
			return a.ExternalID < b.ExternalID
		}
		return a.ID < b.ID
	})
	return out, nil
}

// overrideTransferRows reads each account's overrides, sorted by service.
func (h *Handler) overrideTransferRows(ctx context.Context, accounts []config.CloudAccount) ([]OverrideTransferRow, error) {
	rows := []OverrideTransferRow{}
	for i := range accounts {
		overrides, err := h.config.ListAccountServiceOverrides(ctx, accounts[i].ID)
		if err != nil {
			return nil, fmt.Errorf("accounts: %w", err)
		}
		sort.Slice(overrides, func(a, b int) bool {
			if overrides[a].Provider != overrides[b].Provider {
				return overrides[a].Provider < overrides[b].Provider
			}
			return overrides[a].Service < overrides[b].Service
		})
		for j := range overrides {
			rows = append(rows, overrideTransferRowFrom(&accounts[i], &overrides[j]))
		}
	}
	return rows, nil
}
