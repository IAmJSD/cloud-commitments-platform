package api

import (
	"context"
	"fmt"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
)

// Ladder execution arming is a money-spending authorization, gated like the
// auto-purchase switch (auto_purchase_grant.go): execute:purchases for a real
// user, never the admin API key. Plain LadderingEnabled (planning only) and
// every de-escalation (switch off, disarm, lower the cap) stay ordinary
// update:config writes.

// requireLadderExecutionSwitchGrant gates and stamps a change of
// GlobalConfig.LadderExecutionEnabled inside the UpdateGlobalConfigAtomic
// closure. Only off->on needs the grant; any change is stamped. grant is the
// pre-resolved execute:purchases outcome (armingGrants.purchases), so nothing
// here reads through the pool inside the transaction (issue #1517).
func requireLadderExecutionSwitchGrant(ctx context.Context, session *Session, grant error, before bool, cfg *config.GlobalConfig) error {
	if before == cfg.LadderExecutionEnabled {
		return nil
	}
	if cfg.LadderExecutionEnabled {
		if err := prefetchedAutoPurchaseGrant(session, grant); err != nil {
			return err
		}
	}
	by, via, at := session.UserID, credentialKind(ctx, session), time.Now().UTC()
	cfg.LadderExecutionChangedBy, cfg.LadderExecutionChangedVia, cfg.LadderExecutionChangedAt = &by, &via, &at
	return nil
}

// logLadderExecutionSwitchChange records a committed change of the switch.
func logLadderExecutionSwitchChange(ctx context.Context, session *Session, before bool, after *config.GlobalConfig) {
	if after == nil || after.LadderExecutionEnabled == before {
		return
	}
	logging.Infof("ladder execution switch changed: actor=%s via=%s %t -> %t", session.UserID, credentialKind(ctx, session), before, after.LadderExecutionEnabled)
}

// ladderConfigArmed reports whether the config, with the global switches on,
// would buy without an approver: enabled and in auto-approve mode.
func ladderConfigArmed(c *config.LadderConfigDB) bool {
	return c != nil && c.Enabled && c.Mode == string(ladder.ModeAutoApprove)
}

// ladderCapRaised reports whether after allows more spend than before. A nil
// cap means no cap, so clearing a cap is a raise. Caps reaching the gate have
// passed Validate; an unrepresentable one fails closed as the maximum.
func ladderCapRaised(before, after *float64) bool {
	if after == nil {
		return before != nil
	}
	if before == nil {
		return false
	}
	return ladderCapMicros(after) > ladderCapMicros(before)
}

func ladderCapMicros(v *float64) int64 {
	n, err := config.LadderCapMicros(*v)
	if err != nil {
		return 1<<63 - 1
	}
	return n
}

// ladderConfigArmEscalates reports whether the write takes the config from
// not-armed to armed, or raises or clears the cap of an armed config. before
// is nil when no row exists.
func ladderConfigArmEscalates(before, after *config.LadderConfigDB) bool {
	if !ladderConfigArmed(after) {
		return false
	}
	if !ladderConfigArmed(before) {
		return true
	}
	return ladderCapRaised(before.MaxHourlyCommitPerRun, after.MaxHourlyCommitPerRun)
}

// requireLadderConfigArmGrant gates a ladder_config write and logs the
// authorized escalation. before is nil when no row exists.
func (h *Handler) requireLadderConfigArmGrant(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, before, after *config.LadderConfigDB) error {
	if !ladderConfigArmEscalates(before, after) {
		return nil
	}
	if err := h.requireAutoPurchaseGrant(ctx, req, session); err != nil {
		return err
	}
	var oldCap *float64
	if before != nil {
		oldCap = before.MaxHourlyCommitPerRun
	}
	logging.Infof("ladder config arm or cap raise authorized (write follows): account=%s provider=%s config=%q actor=%s via=%s armed %t -> true cap %s -> %s",
		after.CloudAccountID, after.Provider, ladderConfigID(before), session.UserID, credentialKind(ctx, session),
		ladderConfigArmed(before), formatLadderCap(oldCap), formatLadderCap(after.MaxHourlyCommitPerRun))
	return nil
}

// gateLadderConfigWrite loads the stored row and applies the arming gate. A
// lookup failure refuses the write (fail closed).
func (h *Handler) gateLadderConfigWrite(ctx context.Context, req *events.LambdaFunctionURLRequest, session *Session, after *config.LadderConfigDB) error {
	before, err := h.config.GetLadderConfig(ctx, after.CloudAccountID, after.Provider)
	if err != nil {
		return fmt.Errorf("failed to load ladder config: %w", err)
	}
	return h.requireLadderConfigArmGrant(ctx, req, session, before, after)
}

func ladderConfigID(c *config.LadderConfigDB) string {
	if c == nil {
		return ""
	}
	return c.ID
}

func formatLadderCap(v *float64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%.6f", *v)
}
