// Kept outside the route so it can be unit-tested with plain node.
export function healthPayload(now = new Date()) {
  return { status: 'ok', app: '[[.Name]]', time: now.toISOString() };
}
