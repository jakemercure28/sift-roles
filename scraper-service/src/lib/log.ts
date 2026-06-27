/**
 * Minimal structured logger for the native scrapers. Writes one JSON line per
 * event to stderr, matching the worker's existing stderr-JSON style in
 * bridge.ts. Logging never affects JobLead output, so this is intentionally
 * thin (no levels config, no child loggers).
 */
function emit(level: string, msg: string, meta: Record<string, unknown>): void {
  process.stderr.write(JSON.stringify({ level, msg, ...meta }) + '\n');
}

export function info(msg: string, meta: Record<string, unknown> = {}): void {
  emit('info', msg, meta);
}

export function warn(msg: string, meta: Record<string, unknown> = {}): void {
  emit('warn', msg, meta);
}
