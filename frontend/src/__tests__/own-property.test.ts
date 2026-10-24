import { ownValue } from '../lib/own-property';

describe('ownValue (#317)', () => {
  const table: Record<string, string> = { aws: 'AWS', 'no-upfront': 'No Upfront' };

  test('returns own entries', () => {
    expect(ownValue(table, 'aws')).toBe('AWS');
    expect(ownValue(table, 'no-upfront')).toBe('No Upfront');
  });

  test.each(['constructor', 'toString', '__proto__', 'hasOwnProperty', 'valueOf', ''])(
    'returns undefined for %p instead of an Object.prototype member',
    (key) => {
      expect(ownValue(table, key)).toBeUndefined();
    },
  );
});
