package server

import (
	"log"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/exchange"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// fourEyesExchangeMode returns the mode an unattended RI exchange run (the
// scheduled reshape task or a ladder reshape) may use under the current
// 4-eyes policy (issue #679).
//
// In "auto" mode, pkg/exchange executes exchanges on the spot with no human
// approval. That bypasses RequireDifferentApprover, which the direct execute
// endpoints already refuse (#517). So with 4-eyes on, every mode but "manual"
// is downgraded to "manual": the run only raises pending records, and those go
// through the approve endpoint and its 4-eyes check. "" and typos get the
// downgrade too; pkg/exchange would otherwise refuse them as unknown modes, so
// the downgrade keeps the scheduled run working rather than failing it.
//
// A nil global config fails closed: it is treated as 4-eyes on, never as off.
func fourEyesExchangeMode(cfg *config.GlobalConfig, mode string) string {
	manual := string(exchange.ExchangeModeManual)
	if mode == manual {
		return mode
	}
	if cfg != nil && !cfg.RequireDifferentApprover {
		return mode
	}
	log.Printf("RI exchange: 4-eyes approval mode is on; running mode %q as %q so every exchange waits for approval", mode, manual)
	return manual
}
