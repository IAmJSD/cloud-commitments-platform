/**
 * Resolves once every promise callback queued so far, and every callback
 * those queue in turn, has run.
 *
 * Use it after dispatching an event whose async handler awaits mocked
 * (already settled) promises. A zero-delay timer fires only after the
 * microtask queue is empty, so the whole handler chain settles however many
 * awaits it has, with no guessed wall-clock window. It does not wait for
 * timers the code under test schedules itself; use jest fake timers for
 * those. Do not call it while fake timers are installed: the zero-delay
 * timer would never fire.
 */
export function flushPromises(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}
