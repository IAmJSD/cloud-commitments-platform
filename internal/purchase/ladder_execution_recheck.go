package purchase

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// ladderExecutionRefusal is the execute-time recheck for ladder purchases
// (platform#118, PR4r). A ladder execution can sit approved or scheduled for
// hours, so the switches that allowed it to be created are read again here,
// right before any provider call: both global ladder flags, the ladder config
// (enabled, auto_approve) and the run (not canceled).
//
// An execution belongs to a ladder tranche when its idempotency key equals the
// tranche id. The lookup compares as text, so every other key shape
// ("<root>:<account>" multi-account keys, legacy execution ids, fingerprints)
// simply matches nothing and is not a ladder execution. Only a real lookup
// error refuses, because a recheck that cannot read the switches must not
// guess "allowed".
//
// A refusal is wrapped by the caller as a beforeSubmission error: the row ends
// failed with no_submission stamped, which lets the ladder reconciler fail the
// tranche and release its netting (decideFiredTranche reads that proof).
//
// Accepted residual race: the switch can be flipped after this check passes
// and before the provider call. That window is milliseconds and no lock is
// held across a provider call, so it cannot be closed here.
func (m *Manager) ladderExecutionRefusal(ctx context.Context, exec *config.PurchaseExecution) error {
	gate, err := m.config.GetLadderExecutionGate(ctx, exec.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("refusing to execute: ladder tranche lookup failed: %w", err)
	}
	if gate == nil {
		return nil
	}
	if err := m.ladderFlagsRefusal(ctx); err != nil {
		return err
	}
	return ladderGateRefusal(gate)
}

func (m *Manager) ladderFlagsRefusal(ctx context.Context) error {
	global, err := m.config.GetGlobalConfig(ctx)
	if err != nil {
		return fmt.Errorf("refusing to execute ladder purchase: global config unreadable: %w", err)
	}
	if global == nil {
		return errors.New("refusing to execute ladder purchase: global config unavailable")
	}
	if !global.LadderingEnabled {
		return errors.New("refusing to execute ladder purchase: ladder planning is switched off")
	}
	if !global.LadderExecutionEnabled {
		return errors.New("refusing to execute ladder purchase: ladder execution is switched off")
	}
	return nil
}

func ladderGateRefusal(gate *config.LadderExecutionGate) error {
	switch {
	case gate.ConfigEnabled == nil || gate.ConfigMode == nil:
		return errors.New("refusing to execute ladder purchase: its ladder config no longer exists")
	case !*gate.ConfigEnabled:
		return errors.New("refusing to execute ladder purchase: its ladder config is disabled")
	case ladder.LadderMode(*gate.ConfigMode) != ladder.ModeAutoApprove:
		return fmt.Errorf("refusing to execute ladder purchase: ladder config mode is %q, not auto_approve", *gate.ConfigMode)
	case gate.RunStatus == nil:
		return errors.New("refusing to execute ladder purchase: its ladder run no longer exists")
	case ladder.RunStatus(*gate.RunStatus) == ladder.RunStatusCancelled:
		return errors.New("refusing to execute ladder purchase: its ladder run was canceled")
	}
	return nil
}
