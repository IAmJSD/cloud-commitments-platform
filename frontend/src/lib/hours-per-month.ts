/**
 * Hours in a month for every hourly <-> monthly cost conversion in the UI.
 *
 * 730 is AWS's canonical convention: 8760 hours in a 365-day year divided by
 * 12 months. The backend uses the same figure (ricost.HoursPerMonth in
 * internal/ricost/monthly_cost.go), so an hourly figure shown here matches the
 * monthly figures the API derives. Keep the two in sync (issues #337, #394).
 */
export const HOURS_PER_MONTH = 730;
