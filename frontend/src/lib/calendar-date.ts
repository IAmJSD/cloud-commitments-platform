export function suggestedPurchaseStartDate(noticeDays: number, now = new Date()): string {
  return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + noticeDays + 2)).toISOString().slice(0, 10);
}
