/**
 * Coarse relative time, for "last seen" style labels.
 *
 * Deliberately low-resolution: these timestamps come from a server whose clock
 * may differ from the browser's by seconds, so "12s ago" would imply a precision
 * that does not exist. Anything under a minute is just "now".
 */
export function ago(iso: string | undefined): string {
  if (!iso) return "";
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "";

  const seconds = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

/** Shorten a path for a header line, keeping the end — the part that identifies it. */
export function shortPath(path: string | undefined, keep = 3): string {
  if (!path) return "";
  const parts = path.split("/").filter(Boolean);
  if (parts.length <= keep) return path;
  return "…/" + parts.slice(-keep).join("/");
}
