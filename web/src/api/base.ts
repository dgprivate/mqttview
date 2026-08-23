/**
 * Where the UI is served from.
 *
 * Standalone, that is the root. Behind Home Assistant ingress it is
 * `/api/hassio_ingress/<token>/`, and every URL the app builds has to sit
 * under it or the request leaves the panel and is refused.
 *
 * The prefix is never hard-coded or configured: the server writes it into the
 * `<base href>` of index.html, and everything here is derived from that. One
 * source, set by the only party that knows the answer.
 */

/** basePath is the prefix, without a trailing slash. Empty at the root. */
export function basePath(): string {
  try {
    return new URL(document.baseURI).pathname.replace(/\/+$/, '')
  } catch {
    return ''
  }
}

/**
 * apiURL resolves an API path against the base.
 *
 * The leading slash is stripped on purpose: `new URL('/api/x', base)` throws
 * the prefix away and resolves against the origin, which is exactly the bug
 * this function exists to prevent.
 */
export function apiURL(path: string): string {
  return new URL(path.replace(/^\/+/, ''), document.baseURI).toString()
}

/** socketURL is the WebSocket endpoint, on the same origin and prefix. */
export function socketURL(): string {
  const url = new URL('api/ws', document.baseURI)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return url.toString()
}
