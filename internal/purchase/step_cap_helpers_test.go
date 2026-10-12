package purchase

import "github.com/LeanerCloud/cloud-commitments-platform/internal/config"

// capOf returns a pointer to a plan cap value (fixture helper).
func capOf(v float64) *float64 { return &v }

// autoPurchaseOnCfg is a fixture global config with the kill switch on.
func autoPurchaseOnCfg() *config.GlobalConfig {
	return &config.GlobalConfig{AutoPurchaseEnabled: true}
}
