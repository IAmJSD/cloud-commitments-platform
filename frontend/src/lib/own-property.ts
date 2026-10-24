/**
 * Own-property lookup for display/ordering tables keyed by API strings.
 *
 * `TABLE[key]` walks the prototype chain, so a value such as "constructor"
 * or "toString" resolves to an Object.prototype member instead of falling
 * through to the caller's `??` default. The same shape reached production via
 * tab routing (cloud-commitments-cli#1855) before #1854 fixed it there.
 */
export function ownValue<T>(table: Readonly<Record<string, T>>, key: string): T | undefined {
  return Object.prototype.hasOwnProperty.call(table, key) ? table[key] : undefined;
}
