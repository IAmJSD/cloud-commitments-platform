// Run every suite outside UTC so code that mixes local and UTC dates cannot
// pass on a UTC CI runner by coincidence. New York is UTC-4/-5, so its
// calendar day trails the UTC day for several hours each evening.
export default function globalSetup(): void {
  process.env['TZ'] = 'America/New_York';
}
