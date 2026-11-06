import { healthPayload } from '../../../lib/health.mjs';

export const dynamic = 'force-dynamic';

export function GET() {
  return Response.json(healthPayload());
}
