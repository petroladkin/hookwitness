// What a register's closed live feed means. EventSource closes on any
// answer but a stream: a proxy's 502 or 504, a restart, or the login having
// ended. Only the last is worth a reload (onto the login page); for the rest
// the page says the feed is lost and the SSE extension reopens it with its
// back-off. Reloading on every close would loop when a proxy fails the
// feed but serves the page.
(function (root) {
  "use strict";

  // closed asks the page itself, without following a redirect, and
  // resolves "reload" when the login ended (a redirect to the login page,
  // or 401/403) and "retry" otherwise (any other answer, or none).
  async function closed(probe) {
    let res;
    try {
      res = await probe();
    } catch (e) {
      return "retry";
    }
    if (res.type === "opaqueredirect" || (res.status >= 300 && res.status < 400) || res.status === 401 || res.status === 403) {
      return "reload";
    }
    return "retry";
  }

  root.HookwitnessFeed = { closed };
})(typeof window !== "undefined" ? window : this);
