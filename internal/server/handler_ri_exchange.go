package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/recommendations"
	ec2svc "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/ricost"
)

// riExchangeClients holds the injectable dependencies for RI exchange operations.
// In production, handleRIExchangeReshape constructs real AWS clients.
// Tests call executeRIExchangeReshape directly with mock implementations.
type riExchangeClients struct {
	listConvertibleRIs func(ctx context.Context) ([]ec2svc.ConvertibleRI, error)
	getRIUtilization   func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error)
	exchangeClient     exchange.ExchangeClientInterface
	lookupOffering     exchange.LookupOfferingFunc
	accountID          string
	region             string
}

// handleRIExchangeReshape runs the automated RI exchange analysis and execution.
func (app *Application) handleRIExchangeReshape(ctx context.Context) (*exchange.AutoExchangeResult, error) {
	log.Println("Starting RI exchange reshape analysis...")

	cfg, err := app.Config.GetGlobalConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	// Before the enabled check: a stranded hold from an earlier run still
	// blocks approvals of pending exchanges while automation is off.
	app.alarmStaleRIExchanges(ctx)
	if !cfg.RIExchangeEnabled {
		log.Println("RI exchange automation is disabled, skipping")
		return nil, nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	accountID, err := resolveAccountID(ctx, sts.NewFromConfig(awsCfg))
	if err != nil {
		return nil, fmt.Errorf("RI exchange reshape aborted: %w", err)
	}

	ec2Client := awsprovider.NewEC2ClientDirect(awsCfg)
	clients := riExchangeClients{
		listConvertibleRIs: func(ctx context.Context) ([]ec2svc.ConvertibleRI, error) {
			return ec2Client.ListConvertibleReservedInstances(ctx)
		},
		getRIUtilization: func(ctx context.Context, lookbackDays int) ([]recommendations.RIUtilization, error) {
			recsClient := awsprovider.NewRecommendationsClientDirect(awsCfg)
			return recsClient.GetRIUtilization(ctx, lookbackDays, awsCfg.Region)
		},
		exchangeClient: exchange.NewExchangeClient(awsCfg),
		lookupOffering: newConvertibleOfferingLookup(ec2Client),
		accountID:      accountID,
		region:         awsCfg.Region,
	}

	return app.executeRIExchangeReshape(ctx, cfg, clients)
}

// riExchangeStaleAlarmAfter is how long a processing RI exchange may go
// unchanged before the scheduled run raises an operator alarm (#692). Every
// writer settles its row well inside this (4m30s on the API path, the task
// timeout on the scheduled path), so a row this old was most likely stranded
// by a crash between the provider call and the ledger write.
const riExchangeStaleAlarmAfter = time.Hour

// alarmStaleRIExchanges logs an ERROR, which feeds the application error
// alarm, for every processing RI exchange older than riExchangeStaleAlarmAfter.
// Such a row keeps its reserved ceiling counted against the daily cap on every
// later day, and whether the exchange happened at AWS is unknown, so it is
// never settled or released here: an operator verifies it at AWS and resolves
// it via POST /api/ri-exchange/resolve/{id}. It returns how many it reported.
func (app *Application) alarmStaleRIExchanges(ctx context.Context) int {
	stale, err := app.Config.GetStaleProcessingExchanges(ctx, riExchangeStaleAlarmAfter)
	if err != nil {
		logging.Errorf("RI exchange reconciliation: failed to list stale processing exchanges: %v", err)
		return 0
	}
	for i := range stale {
		r := &stale[i]
		logging.Errorf("RI exchange reconciliation needed: record_id=%s account_id=%s region=%s source_ri_ids=%v held_usd=%s processing_since=%s: "+
			"the exchange outcome at AWS is unknown and the row keeps holding daily-cap headroom; check the source RIs in AWS, "+
			"then resolve it with POST /api/ri-exchange/resolve/%s (allowed once unchanged for %s)",
			r.ID, r.AccountID, r.Region, r.SourceRIIDs, r.PaymentDue, r.UpdatedAt.UTC().Format(time.RFC3339),
			r.ID, config.RIExchangeResolveStaleAfter)
	}
	return len(stale)
}

// convertibleOfferingFinder is the slice of the EC2 client the offering
// lookup needs. Satisfied by *ec2svc.Client.
type convertibleOfferingFinder interface {
	FindConvertibleOffering(ctx context.Context, params ec2svc.FindConvertibleOfferingParams) (string, error)
}

// newConvertibleOfferingLookup adapts the EC2 client to exchange.LookupOfferingFunc.
// AvailabilityZone is the source RI's AZ (empty for regional RIs); the provider
// refuses a zonal lookup without one, so a zonal RI never exchanges into an
// offering in another zone.
func newConvertibleOfferingLookup(finder convertibleOfferingFinder) exchange.LookupOfferingFunc {
	return func(ctx context.Context, p exchange.LookupOfferingParams) (string, error) {
		return finder.FindConvertibleOffering(ctx, ec2svc.FindConvertibleOfferingParams{
			InstanceType:       p.InstanceType,
			ProductDescription: p.ProductDescription,
			Tenancy:            p.Tenancy,
			Scope:              p.Scope,
			AvailabilityZone:   p.AvailabilityZone,
			Duration:           p.Duration,
		})
	}
}

// executeRIExchangeReshape contains the core exchange logic, separated from AWS
// client construction for testability.
func (app *Application) executeRIExchangeReshape(ctx context.Context, cfg *config.GlobalConfig, clients riExchangeClients) (*exchange.AutoExchangeResult, error) {
	// Every record the run creates carries this account ID for the audit
	// trail, so a run without one stops before touching AWS or the store.
	if clients.accountID == "" {
		return nil, errors.New("no AWS account ID resolved for the RI exchange audit trail")
	}
	instances, err := clients.listConvertibleRIs(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list convertible RIs: %w", err)
	}

	utilData, err := clients.getRIUtilization(ctx, cfg.RIExchangeLookbackDays)
	if err != nil {
		return nil, fmt.Errorf("failed to get RI utilization: %w", err)
	}

	riInfos, utilInfos, riMetadata := convertForAutoExchange(instances, utilData)

	store := newConfigExchangeStoreAdapter(app.Config)

	result, err := exchange.RunAutoExchange(ctx, exchange.RunAutoExchangeParams{
		Store:          store,
		ExchangeClient: clients.exchangeClient,
		LookupOffering: clients.lookupOffering,
		RIs:            riInfos,
		Utilization:    utilInfos,
		Config: exchange.RIExchangeConfig{
			Mode:                     fourEyesExchangeMode(cfg, cfg.RIExchangeMode),
			UtilizationThreshold:     cfg.RIExchangeUtilizationThreshold,
			MaxPaymentPerExchangeUSD: cfg.RIExchangeMaxPerExchangeUSD,
			MaxPaymentDailyUSD:       cfg.RIExchangeMaxDailyUSD,
			LookbackDays:             cfg.RIExchangeLookbackDays,
		},
		AccountID:    clients.accountID,
		Region:       clients.region,
		DashboardURL: app.appConfig.DashboardURL,
		RIMetadata:   riMetadata,
	})
	if err != nil {
		return nil, fmt.Errorf("auto exchange failed: %w", err)
	}

	notifyEmail := ""
	if cfg.NotificationEmail != nil {
		notifyEmail = *cfg.NotificationEmail
	}
	app.sendExchangeNotification(ctx, result, notifyEmail)

	log.Printf("RI exchange reshape complete: mode=%s completed=%d pending=%d failed=%d skipped=%d",
		result.Mode, len(result.Completed), len(result.Pending), len(result.Failed), len(result.Skipped))
	return result, nil
}

// callerIdentityGetter is the slice of the STS client resolveAccountID needs.
// Satisfied by *sts.Client.
type callerIdentityGetter interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// resolveAccountID fetches the caller's AWS account ID via STS and fails when
// it cannot be determined. The ID is written onto every RI exchange record the
// run creates, so a placeholder such as "unknown" would put a fabricated
// account on the audit trail of a money action (#326); the run is aborted
// instead, as defaultLadderAccountResolver does.
func resolveAccountID(ctx context.Context, client callerIdentityGetter) (string, error) {
	identity, err := client.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("resolve caller AWS account via STS: %w", err)
	}
	if identity == nil || identity.Account == nil || *identity.Account == "" {
		return "", errors.New("STS GetCallerIdentity returned no account ID")
	}
	return *identity.Account, nil
}

// sendExchangeNotification sends email notifications based on the exchange result.
// notifyEmail is the global notification address from GlobalConfig; it becomes
// the RecipientEmail for pending-approval messages that carry approval tokens
// and must not be broadcast via SNS. Email errors are logged but don't fail the task.
func (app *Application) sendExchangeNotification(ctx context.Context, result *exchange.AutoExchangeResult, notifyEmail string) {
	if app.Email == nil {
		return
	}

	if !exchangeHasResults(result) {
		return
	}

	// RecipientEmail is only meaningful for the pending-approval branch:
	// SendRIExchangePendingApproval routes token-bearing emails to a specific
	// inbox and rejects an empty recipient with ErrNoRecipient. The completed
	// path (SendRIExchangeCompleted) ignores RecipientEmail and is
	// transport-driven (SNS broadcast / SMTP notifyEmail), so leave it empty
	// there to keep the struct contract honest.
	data := buildExchangeNotificationData(result, app.appConfig.DashboardURL, "")

	var err error
	if result.Mode == "manual" && len(result.Pending) > 0 {
		data.RecipientEmail = notifyEmail
		err = app.Email.SendRIExchangePendingApproval(ctx, data)
	} else if len(result.Completed)+len(result.Failed) > 0 {
		err = app.Email.SendRIExchangeCompleted(ctx, data)
	}

	if err != nil {
		log.Printf("Warning: failed to send RI exchange notification email: %v", err)
	}
}

func exchangeHasResults(result *exchange.AutoExchangeResult) bool {
	return len(result.Completed)+len(result.Pending)+len(result.Failed)+len(result.Skipped) > 0
}

// buildExchangeNotificationData converts AutoExchangeResult into email notification data.
// notifyEmail populates RecipientEmail. Callers should pass it only for the
// pending-approval branch (where token-bearing emails must route to a specific
// inbox rather than the SNS broadcast topic) and pass "" for the completed
// branch, which is transport-driven and ignores RecipientEmail.
func buildExchangeNotificationData(result *exchange.AutoExchangeResult, dashboardURL, notifyEmail string) email.RIExchangeNotificationData {
	data := email.RIExchangeNotificationData{
		DashboardURL:   dashboardURL,
		Mode:           result.Mode,
		RecipientEmail: notifyEmail,
	}

	allOutcomes := make([]exchange.ExchangeOutcome, 0, len(result.Completed)+len(result.Pending)+len(result.Failed))
	allOutcomes = append(allOutcomes, result.Completed...)
	allOutcomes = append(allOutcomes, result.Pending...)
	allOutcomes = append(allOutcomes, result.Failed...)

	for i := range allOutcomes {
		o := allOutcomes[i]
		data.Exchanges = append(data.Exchanges, email.RIExchangeItem{
			RecordID:           o.RecordID,
			ApprovalToken:      o.ApprovalToken,
			SourceRIID:         o.SourceRIID,
			SourceInstanceType: o.SourceInstanceType,
			TargetInstanceType: o.TargetInstanceType,
			TargetCount:        int(o.TargetCount),
			PaymentDue:         o.PaymentDue,
			ExchangeID:         o.ExchangeID,
			UtilizationPct:     o.UtilizationPct,
			Error:              o.Error,
			OutcomeUnknown:     o.OutcomeUnknown,
		})
	}

	for _, s := range result.Skipped {
		data.Skipped = append(data.Skipped, email.SkippedExchange{
			SourceRIID:         s.SourceRIID,
			SourceInstanceType: s.SourceInstanceType,
			Reason:             s.Reason,
		})
	}

	return data
}

// monthlyCostFromConvertibleRI delegates to ricost.MonthlyCostFromConvertibleRI,
// the single implementation of the convertible RI monthly cost formula shared
// with internal/api. It errors for a non-positive duration or no positive
// price, and callers must exclude the RI.
func monthlyCostFromConvertibleRI(ri ec2svc.ConvertibleRI) (float64, error) {
	return ricost.MonthlyCostFromConvertibleRI(ri)
}

// convertForAutoExchange converts provider-specific types to exchange package types,
// including RI metadata needed for offering lookup. It serves both the
// auto-exchange run and the ladder write path (ladder_write.go); an RI whose
// monthly cost is not computable is logged and dropped on both.
func convertForAutoExchange(instances []ec2svc.ConvertibleRI, utilData []recommendations.RIUtilization) ([]exchange.RIInfo, []exchange.UtilizationInfo, map[string]exchange.RIMetadataInfo) {
	riInfos := make([]exchange.RIInfo, 0, len(instances))
	riMetadata := make(map[string]exchange.RIMetadataInfo, len(instances))

	for i := range instances {
		inst := instances[i]
		monthlyCost, err := monthlyCostFromConvertibleRI(inst)
		if err != nil {
			logging.Warnf("convertForAutoExchange: excluding RI from auto-exchange and ladder reshaping: %v", err)
			continue
		}
		riInfos = append(riInfos, exchange.RIInfo{
			ID:                  inst.ReservedInstanceID,
			InstanceType:        inst.InstanceType,
			InstanceCount:       inst.InstanceCount,
			OfferingClass:       "convertible",
			NormalizationFactor: inst.NormalizationFactor,
			MonthlyCost:         monthlyCost,
			CurrencyCode:        inst.CurrencyCode,
		})
		riMetadata[inst.ReservedInstanceID] = exchange.RIMetadataInfo{
			ProductDescription: inst.ProductDescription,
			InstanceTenancy:    inst.InstanceTenancy,
			Scope:              inst.Scope,
			AvailabilityZone:   inst.AvailabilityZone,
			Duration:           inst.Duration,
		}
	}

	utilInfos := make([]exchange.UtilizationInfo, len(utilData))
	for i, u := range utilData {
		utilInfos[i] = exchange.UtilizationInfo{
			RIID:               u.ReservedInstanceID,
			UtilizationPercent: u.UtilizationPercent,
		}
	}

	return riInfos, utilInfos, riMetadata
}

// configExchangeStoreAdapter adapts config.StoreInterface to exchange.RIExchangeStore.
// It converts between config.RIExchangeRecord and exchange.ExchangeRecord.
type configExchangeStoreAdapter struct {
	store config.StoreInterface
}

func newConfigExchangeStoreAdapter(store config.StoreInterface) *configExchangeStoreAdapter {
	return &configExchangeStoreAdapter{store: store}
}

// awsAccountIDPattern is the shape of an AWS account ID: exactly 12 ASCII digits
// (RE2's \d is ASCII-only).
var awsAccountIDPattern = regexp.MustCompile(`^\d{12}$`)

// requireAWSExchangeAccountID refuses an exchange record whose account ID is not
// an AWS account ID. Every record this adapter writes comes from the AWS EC2
// exchange engine, so its account ID must be the 12-digit account STS
// reported. ri_exchange_history.account_id itself no longer enforces that
// shape (migration 000137 dropped the AWS-only CHECK so the column can hold
// Azure and GCP identifiers, issue #144); this check keeps the guarantee for
// AWS rows at the boundary where the provider is known, including refusing the
// "unknown" placeholder resolveAccountID falls back to when STS fails.
func requireAWSExchangeAccountID(accountID string) error {
	if !awsAccountIDPattern.MatchString(accountID) {
		return fmt.Errorf("refusing to record RI exchange: account ID %q is not a 12-digit AWS account ID", accountID)
	}
	return nil
}

func (a *configExchangeStoreAdapter) SaveRIExchangeRecord(ctx context.Context, record *exchange.ExchangeRecord) error {
	if err := requireAWSExchangeAccountID(record.AccountID); err != nil {
		return err
	}
	cfgRecord := exchangeToConfigRecord(record)
	if err := a.store.SaveRIExchangeRecord(ctx, cfgRecord); err != nil {
		return err
	}
	record.ID = cfgRecord.ID
	return nil
}

func (a *configExchangeStoreAdapter) ReserveRIExchange(ctx context.Context, record *exchange.ExchangeRecord, dailyCapUSD, perExchangeCapUSD string) (string, error) {
	if err := requireAWSExchangeAccountID(record.AccountID); err != nil {
		return "", err
	}
	cfgRecord := exchangeToConfigRecord(record)
	ceiling, err := a.store.ReserveRIExchange(ctx, cfgRecord, dailyCapUSD, perExchangeCapUSD)
	if err != nil {
		return "", err
	}
	record.ID = cfgRecord.ID
	record.PaymentDue = cfgRecord.PaymentDue
	return ceiling, nil
}

func (a *configExchangeStoreAdapter) CancelAllPendingExchanges(ctx context.Context) (int64, error) {
	return a.store.CancelAllPendingExchanges(ctx)
}

func (a *configExchangeStoreAdapter) CancelPendingExchangesByOrigin(ctx context.Context, origin common.ExchangeOrigin) (int64, error) {
	return a.store.CancelPendingExchangesByOrigin(ctx, origin)
}

func (a *configExchangeStoreAdapter) GetStaleProcessingExchanges(ctx context.Context, olderThan time.Duration) ([]exchange.ExchangeRecord, error) {
	cfgRecords, err := a.store.GetStaleProcessingExchanges(ctx, olderThan)
	if err != nil {
		return nil, err
	}
	result := make([]exchange.ExchangeRecord, len(cfgRecords))
	for i := range cfgRecords {
		r := cfgRecords[i]
		result[i] = configToExchangeRecord(&r)
	}
	return result, nil
}

func (a *configExchangeStoreAdapter) GetRIExchangeDailySpend(ctx context.Context, date time.Time) (string, error) {
	return a.store.GetRIExchangeDailySpend(ctx, date)
}

func (a *configExchangeStoreAdapter) CompleteRIExchange(ctx context.Context, id, exchangeID string) error {
	return a.store.CompleteRIExchange(ctx, id, exchangeID)
}

func (a *configExchangeStoreAdapter) CompleteRIExchangeWithPayment(ctx context.Context, id, exchangeID, acceptedPaymentDue string) error {
	return a.store.CompleteRIExchangeWithPayment(ctx, id, exchangeID, acceptedPaymentDue)
}

func (a *configExchangeStoreAdapter) FailRIExchange(ctx context.Context, id, errorMsg string) error {
	return a.store.FailRIExchange(ctx, id, errorMsg)
}

// exchangeToConfigRecord leaves CreatedByUserID nil on purpose: these rows are
// written by the scheduler or the ladder engine, which have no human creator,
// and pkg/exchange.ExchangeRecord has no field to carry one (issue #518).
func exchangeToConfigRecord(r *exchange.ExchangeRecord) *config.RIExchangeRecord {
	return &config.RIExchangeRecord{
		ID:                 r.ID,
		AccountID:          r.AccountID,
		ExchangeID:         r.ExchangeID,
		Region:             r.Region,
		SourceRIIDs:        r.SourceRIIDs,
		SourceInstanceType: r.SourceInstanceType,
		SourceCount:        r.SourceCount,
		TargetOfferingID:   r.TargetOfferingID,
		TargetInstanceType: r.TargetInstanceType,
		TargetCount:        r.TargetCount,
		PaymentDue:         r.PaymentDue,
		Status:             r.Status,
		ApprovalToken:      r.ApprovalToken,
		Error:              r.Error,
		Mode:               r.Mode,
		LadderRunID:        r.LadderRunID,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
		CompletedAt:        r.CompletedAt,
		ExpiresAt:          r.ExpiresAt,
	}
}

func configToExchangeRecord(r *config.RIExchangeRecord) exchange.ExchangeRecord {
	return exchange.ExchangeRecord{
		ID:                 r.ID,
		AccountID:          r.AccountID,
		ExchangeID:         r.ExchangeID,
		Region:             r.Region,
		SourceRIIDs:        r.SourceRIIDs,
		SourceInstanceType: r.SourceInstanceType,
		SourceCount:        r.SourceCount,
		TargetOfferingID:   r.TargetOfferingID,
		TargetInstanceType: r.TargetInstanceType,
		TargetCount:        r.TargetCount,
		PaymentDue:         r.PaymentDue,
		Status:             r.Status,
		ApprovalToken:      r.ApprovalToken,
		Error:              r.Error,
		Mode:               r.Mode,
		LadderRunID:        r.LadderRunID,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
		CompletedAt:        r.CompletedAt,
		ExpiresAt:          r.ExpiresAt,
	}
}
